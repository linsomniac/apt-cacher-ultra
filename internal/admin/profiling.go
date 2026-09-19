package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime/pprof"
	"strconv"
	"time"
)

// Use runtime/pprof directly: importing net/http/pprof would also register
// diagnostics on http.DefaultServeMux. These three routes belong only to the
// private admin dispatcher, which supplies authentication and request logging.
func (s *Server) profileRoutes() map[string]http.HandlerFunc {
	if !s.cfg.Admin.PprofEnabled {
		return nil
	}
	return map[string]http.HandlerFunc{
		"/debug/pprof/profile":   s.handleCPUProfile,
		"/debug/pprof/heap":      s.handleRuntimeProfile("heap"),
		"/debug/pprof/goroutine": s.handleRuntimeProfile("goroutine"),
	}
}

func cpuProfileDuration(r *http.Request) (time.Duration, error) {
	values, present := r.URL.Query()["seconds"]
	if !present {
		return 30 * time.Second, nil
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("seconds must be a single integer from 1 to 300")
	}
	seconds, parseErr := strconv.Atoi(values[0])
	if parseErr != nil || seconds < 1 || seconds > 300 {
		return 0, fmt.Errorf("seconds must be an integer from 1 to 300")
	}
	return time.Duration(seconds) * time.Second, nil
}

func profileHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.pprof"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
}

// Profile output is operator-triggered and can be sizable. Bound its writes
// independently of capture duration, and interrupt a blocked write when the
// request or server ends. The production ResponseWriter supports deadlines via
// countingWriter.Unwrap; recorders used by tests may return ErrNotSupported.
func (s *Server) writeProfile(w http.ResponseWriter, r *http.Request, name string, write func(io.Writer) error) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
	interrupt := func() { _ = controller.SetWriteDeadline(time.Now()) }
	stopRequest := interruptProfileWrite(r.Context(), interrupt)
	stopShutdown := interruptProfileWrite(s.profilingCtx, interrupt)
	defer stopRequest()
	defer stopShutdown()
	profileHeaders(w, name)
	if err := write(w); err != nil {
		s.logger.Debug("admin_profile_write_failed", "profile", name, "err", err)
	}
}

func interruptProfileWrite(ctx context.Context, interrupt func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		interrupt()
	})
	return func() {
		// A callback already running must finish before the HTTP handler
		// returns, so it cannot change the next keep-alive request's deadline.
		if !stop() {
			<-done
		}
	}
}

func (s *Server) handleCPUProfile(w http.ResponseWriter, r *http.Request) {
	duration, err := cpuProfileDuration(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.shuttingDown.Load() || s.profilingCtx.Err() != nil {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	// Keep the runtime's profile writer independent of HTTP backpressure:
	// StopCPUProfile must finish even if the diagnostic client stops reading.
	var profile bytes.Buffer
	if err := pprof.StartCPUProfile(&profile); err != nil {
		// The runtime permits one CPU profile at a time for the entire process.
		http.Error(w, "CPU profiling is already active", http.StatusConflict)
		return
	}
	start := time.Now()
	s.logger.Debug("admin_cpu_profile_started", "seconds", duration.Seconds())
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
	case <-s.profilingCtx.Done():
	}
	pprof.StopCPUProfile()
	s.logger.Debug("admin_cpu_profile_finished", "duration_ms", time.Since(start).Milliseconds())
	if r.Context().Err() != nil {
		return
	}
	if s.profilingCtx.Err() != nil {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	s.writeProfile(w, r, "cpu", func(w io.Writer) error {
		_, err := profile.WriteTo(w)
		return err
	})
}

func (s *Server) handleRuntimeProfile(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		// Binary profiles are directly consumable by go tool pprof. In
		// particular, heap capture does not force a Go garbage collection.
		s.writeProfile(w, r, name, func(w io.Writer) error {
			return pprof.Lookup(name).WriteTo(w, 0)
		})
	}
}
