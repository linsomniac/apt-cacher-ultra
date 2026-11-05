package servertls

import (
	"context"
	"crypto/tls"
	"log/slog"
	"sync/atomic"
	"time"
)

// Files retains the last valid pair across failed/partial external rotations.
type Files struct {
	Holder
	certPath, keyPath string
	problem           atomic.Bool
}

func NewFiles(certPath, keyPath string) (*Files, error) {
	f := &Files{certPath: certPath, keyPath: keyPath}
	if err := f.Reload(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Files) Reload() error {
	cert, err := f.load()
	if err != nil {
		f.problem.Store(true)
		return err
	}
	f.Install(cert)
	f.problem.Store(false)
	return nil
}

func (f *Files) load() (*tls.Certificate, error) {
	c, err := ReadFile(f.certPath)
	if err != nil {
		return nil, err
	}
	k, err := ReadFile(f.keyPath)
	if err != nil {
		return nil, err
	}
	return Parse(c, k, "", time.Now())
}

func (f *Files) Snapshot() Snapshot {
	problem := ""
	if f.problem.Load() {
		problem = "file_reload_failed"
	}
	return f.Holder.Snapshot("files", problem, time.Now())
}

func (f *Files) Run(ctx context.Context, logger *slog.Logger) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			failed := f.problem.Load()
			if err := f.Reload(); err != nil && !failed {
				logger.Warn("server_tls_reload_failed", "detail", "retaining the last valid certificate; check configured files")
			} else if err == nil && failed {
				logger.Info("server_tls_reload_recovered")
			}
		}
	}
}
