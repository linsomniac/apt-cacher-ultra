package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// stubForgetter is a test double for the Forgetter interface: it records
// every call and returns a fixed result.
type stubForgetter struct {
	mu                  sync.Mutex
	res                 cache.ForgetResult
	err                 error
	calls               int
	scheme, host, suite string
}

func (s *stubForgetter) Forget(_ context.Context, scheme, host, suitePath string) (cache.ForgetResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.scheme, s.host, s.suite = scheme, host, suitePath
	return s.res, s.err
}

func (s *stubForgetter) snapshot() (calls int, scheme, host, suite string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.scheme, s.host, s.suite
}

// blockingForgetter's Forget signals entered, then waits until its context
// ends or the test unblocks it, and returns: an error wrapping the
// context's when it ended, else a forgotten result.
type blockingForgetter struct {
	entered     chan struct{}
	release     chan struct{}
	done        chan struct{}
	once        sync.Once
	ctxErr      error // set before done closes
	unblockOnce sync.Once
}

func newBlockingForgetter() *blockingForgetter {
	return &blockingForgetter{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
}

func (b *blockingForgetter) Forget(ctx context.Context, _, _, _ string) (cache.ForgetResult, error) {
	b.once.Do(func() { close(b.entered) })
	defer close(b.done)
	select {
	case <-ctx.Done():
	case <-b.release:
	}
	if b.ctxErr = ctx.Err(); b.ctxErr != nil {
		return cache.ForgetResult{}, fmt.Errorf("forget: delete metadata: %w", b.ctxErr)
	}
	return forgotten(0), nil
}

// unblock lets a blocked Forget return (idempotent).
func (b *blockingForgetter) unblock() { b.unblockOnce.Do(func() { close(b.release) }) }

// result waits for Forget to return and reports its context's error.
func (b *blockingForgetter) result() error {
	<-b.done
	return b.ctxErr
}

// forgotten is a stub result: the suite was forgotten, with markers
// deleted and the rows in keptBy keeping none.
func forgotten(markers int, keptBy ...cache.SuiteKey) cache.ForgetResult {
	return cache.ForgetResult{Deleted: true, Gone: true, MarkersDeleted: markers, KeptBy: keptBy}
}

func withForgetter(f Forgetter) adminOpt { return func(cfg *Config) { cfg.Forgetter = f } }

// forget sends POST /forget with body and the given headers (name, value
// pairs) and returns the status, the Allow header and the response body.
func forget(t *testing.T, base, body string, headers ...string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/forget", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Allow"), string(text)
}

// Spec C9 (D9) and ruling PF-7: POST /forget answers 202 once the suite
// is forgotten, its body naming the suite, the markers deleted and the
// rows that kept them; 409 for busy or unknown, its body saying what to
// check; 500 for a database failure; 400 for a missing parameter or
// another scheme (no call); 501 without a Forgetter; scheme defaults to
// https. GET is 405 and OPTIONS lists POST.
func TestEndpoint_Forget(t *testing.T) {
	admin := []string{"X-ACU-Admin", "1"}
	cases := []struct {
		name   string
		stub   *stubForgetter
		body   string
		status int
		calls  int
		text   string
	}{
		{"forgotten 202", &stubForgetter{res: forgotten(2)}, "host=deb.example&suite=/debian/dists/stable", http.StatusAccepted, 1,
			"forgotten https://deb.example/debian/dists/stable; signed-seen markers deleted: 2"},
		{"forgotten, markers kept 202", &stubForgetter{res: forgotten(0,
			cache.SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/flat"},
			cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/flat/."})},
			"scheme=http&host=h&suite=/flat", http.StatusAccepted, 1,
			"forgotten http://h/flat; signed-seen markers deleted: 0; kept by https://h/flat http://h/flat/."},
		{"busy or unknown 409", &stubForgetter{}, "host=h&suite=/s", http.StatusConflict, 1,
			"not forgotten: busy (try again) or no such suite (the status page's Host cell shows scheme://host: pass its " +
				"scheme as scheme, default https, and the rest as host; suite as its Suite path column shows it)"},
		{"database error 500", &stubForgetter{err: errors.New("disk I/O error")}, "host=h&suite=/s", http.StatusInternalServerError, 1,
			"forget failed"},
		{"missing suite 400", &stubForgetter{res: forgotten(0)}, "host=h", http.StatusBadRequest, 0,
			"host and suite are required; scheme is http or https"},
		{"another scheme 400", &stubForgetter{res: forgotten(0)}, "scheme=ftp&host=h&suite=/s", http.StatusBadRequest, 0,
			"host and suite are required; scheme is http or https"},
		// The Task 10 review's minor 4: the Host cell's scheme://host
		// copied whole into host.
		{"a scheme in host 400", &stubForgetter{res: forgotten(0)}, "host=http://deb.example&suite=/s", http.StatusBadRequest, 0,
			"host is the host name alone: pass the scheme in scheme (http or https) and the host without it"},
		// The Task 10 review's minor 5: the 8 KiB cap (without it, this
		// body parses and the call is made).
		{"a body over 8 KiB 413", &stubForgetter{res: forgotten(0)}, "host=h&suite=/s&pad=" + strings.Repeat("x", 9<<10),
			http.StatusRequestEntityTooLarge, 0, "request body too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, base, cleanup := startAdminServer(t, withForgetter(tc.stub))
			defer cleanup()
			if status, _, text := forget(t, base, tc.body, admin...); status != tc.status || strings.TrimRight(text, "\n") != tc.text {
				t.Errorf("status = %d, body %q; want %d, %q", status, text, tc.status, tc.text)
			}
			if calls, _, _, _ := tc.stub.snapshot(); calls != tc.calls {
				t.Errorf("calls = %d, want %d", calls, tc.calls)
			}
		})
	}
	t.Run("scheme defaults to https, http is kept", func(t *testing.T) {
		stub := &stubForgetter{res: forgotten(0)}
		_, base, cleanup := startAdminServer(t, withForgetter(stub))
		defer cleanup()
		forget(t, base, "host=deb.example&suite=/debian/dists/stable", admin...)
		if _, scheme, host, suite := stub.snapshot(); scheme != "https" || host != "deb.example" || suite != "/debian/dists/stable" {
			t.Errorf("params scheme=%q host=%q suite=%q", scheme, host, suite)
		}
		forget(t, base, "scheme=http&host=h&suite=/s", admin...)
		if _, scheme, _, _ := stub.snapshot(); scheme != "http" {
			t.Errorf("scheme = %q, want http", scheme)
		}
	})
	t.Run("a client that disconnects does not cut it short", func(t *testing.T) {
		stub := newBlockingForgetter()
		_, base, cleanup := startAdminServer(t, withForgetter(stub))
		defer cleanup()
		defer stub.unblock()
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/forget", strings.NewReader("host=h&suite=/s"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-ACU-Admin", "1")
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		<-stub.entered
		cancel() // the client gives up: its connection closes
		<-sent
		time.Sleep(200 * time.Millisecond) // net/http cancels a request's context once it sees the connection gone
		stub.unblock()
		if err := stub.result(); err != nil {
			t.Fatalf("the forget's context ended (%v) when its client disconnected; want the work done", err)
		}
	})
	t.Run("no forgetter 501", func(t *testing.T) {
		_, base, cleanup := startAdminServer(t)
		defer cleanup()
		if status, _, _ := forget(t, base, "host=h&suite=/s", admin...); status != http.StatusNotImplemented {
			t.Errorf("status = %d, want 501", status)
		}
	})
	t.Run("GET 405, OPTIONS lists POST", func(t *testing.T) {
		_, base, cleanup := startAdminServer(t, withForgetter(&stubForgetter{res: forgotten(0)}))
		defer cleanup()
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			req, _ := http.NewRequest(method, base+"/forget", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			want := map[string]int{http.MethodGet: http.StatusMethodNotAllowed, http.MethodOptions: http.StatusNoContent}[method]
			if resp.StatusCode != want || resp.Header.Get("Allow") != "POST, OPTIONS" {
				t.Errorf("%s: status %d, Allow %q; want %d, \"POST, OPTIONS\"", method, resp.StatusCode, resp.Header.Get("Allow"), want)
			}
		}
	})
}

// The Task 10 review's minor 5: an accepted request — past the guard, the
// body cap and the parameter checks — is logged as INFO
// admin_forget_requested with its scheme, host and suite, and a database
// failure as WARN admin: forget failed with the error; a refused request
// (403, 400) logs neither.
func TestEndpoint_Forget_LogsAcceptedRequestsAndFailures(t *testing.T) {
	admin := []string{"X-ACU-Admin", "1"}
	records := func(buf *lockedBuffer, msg string) []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(buf.String(), "\n") {
			var m map[string]any
			if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
				out = append(out, m)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		stub      *stubForgetter
		body      string
		headers   []string
		requested bool
		failed    bool
	}{
		{"accepted", &stubForgetter{res: forgotten(0)}, "scheme=http&host=h&suite=/s", admin, true, false},
		{"a database failure", &stubForgetter{err: errors.New("disk I/O error")}, "host=h&suite=/s", admin, true, true},
		{"refused by the guard", &stubForgetter{res: forgotten(0)}, "host=h&suite=/s", nil, false, false},
		{"a bad parameter", &stubForgetter{res: forgotten(0)}, "host=h", admin, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf lockedBuffer
			_, base, cleanup := startAdminServer(t, withForgetter(tc.stub),
				withLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
			defer cleanup()
			forget(t, base, tc.body, tc.headers...)
			req := records(&buf, "admin_forget_requested")
			if !tc.requested {
				if len(req) != 0 {
					t.Fatalf("admin_forget_requested lines %v; want none for a refused request", req)
				}
				return
			}
			_, scheme, _, _ := tc.stub.snapshot()
			if len(req) != 1 || req[0]["level"] != "INFO" || req[0]["scheme"] != scheme || req[0]["canonical_host"] != "h" ||
				req[0]["suite_path"] != "/s" {
				t.Fatalf("admin_forget_requested lines %v; want one at INFO for %s h /s", req, scheme)
			}
			failed := records(&buf, "admin: forget failed")
			if !tc.failed {
				if len(failed) != 0 {
					t.Fatalf("admin: forget failed lines %v; want none", failed)
				}
				return
			}
			if len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["err"] != "disk I/O error" ||
				failed[0]["canonical_host"] != "h" || failed[0]["suite_path"] != "/s" {
				t.Fatalf("admin: forget failed lines %v; want one at WARN with the error", failed)
			}
		})
	}
}

// panickingForgetter's Forget panics, as Reaper.Forget re-panics after
// logging a panic under its hold.
type panickingForgetter struct{}

func (panickingForgetter) Forget(context.Context, string, string, string) (cache.ForgetResult, error) {
	panic("forget bug")
}

// Final review spec m1: the admin http.Server routes net/http's own lines
// through slog at ERROR (its ErrorLog), as the proxy listeners route
// theirs: a POST /forget whose handler panics leaves a structured ERROR
// record "http: panic serving …" naming the panic, not unstructured
// stderr text.
func TestEndpoint_Forget_APanicIsLoggedThroughSlog(t *testing.T) {
	var buf lockedBuffer
	_, base, cleanup := startAdminServer(t, withForgetter(panickingForgetter{}),
		withLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	defer cleanup()
	req, err := http.NewRequest(http.MethodPost, base+"/forget", strings.NewReader("host=h&suite=/s"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ACU-Admin", "1")
	if resp, err := http.DefaultClient.Do(req); err == nil { // net/http closes the connection: an error is expected
		_ = resp.Body.Close()
	}
	var found map[string]any
	for deadline := time.Now().Add(5 * time.Second); found == nil && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, line := range strings.Split(buf.String(), "\n") {
			var m map[string]any
			if line != "" && json.Unmarshal([]byte(line), &m) == nil && strings.HasPrefix(fmt.Sprint(m["msg"]), "http: panic serving") {
				found = m
			}
		}
	}
	if found == nil || found["level"] != "ERROR" || !strings.Contains(fmt.Sprint(found["msg"]), "forget bug") {
		t.Fatalf("panic record %v; want an slog ERROR line \"http: panic serving …: forget bug\"; log:\n%s", found, buf.String())
	}
}

// The Task 11 review's minor 3: a POST /forget in flight when the admin
// listener's Shutdown begins is cut short at once — Shutdown does not wait
// out its 5-minute bound and eat the proxies' drain budget — and answers
// 503 (retry), logged at DEBUG, not WARN: a shutdown is no failure (design
// review M-3).
func TestEndpoint_Forget_ShutdownCutsItShort(t *testing.T) {
	stub := newBlockingForgetter()
	var buf lockedBuffer
	s, base, cleanup := startAdminServer(t, withForgetter(stub),
		withLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	defer cleanup()
	defer stub.unblock() // a forget the shutdown did not end still returns
	type answer struct {
		status int
		text   string
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, base+"/forget", strings.NewReader("host=h&suite=/s"))
		if err != nil {
			answered <- answer{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-ACU-Admin", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		text, err := io.ReadAll(resp.Body)
		answered <- answer{status: resp.StatusCode, text: string(text), err: err}
	}()
	<-stub.entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with a forget in flight: %v after %v; want the forget ended and the listener drained",
			err, time.Since(start))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v with a forget in flight; want it to end the forget at once", d)
	}
	a := <-answered
	if a.err != nil || a.status != http.StatusServiceUnavailable ||
		strings.TrimRight(a.text, "\n") != "shutting down; retry once the daemon is back" {
		t.Fatalf("answer %d %q (%v); want 503 \"shutting down; retry once the daemon is back\"", a.status, a.text, a.err)
	}
	var failed []string
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "admin: forget failed" {
			failed = append(failed, m["level"].(string))
		}
	}
	if len(failed) != 1 || failed[0] != "DEBUG" {
		t.Fatalf("admin: forget failed lines at levels %v; want one at DEBUG", failed)
	}
}

// Ruling PF-7 (design review I-7): the status page shows each suite's
// scheme — JSON scheme (additive to the locked contract) and the HTML Host
// cell — so an operator can tell a suite's http and https rows apart and
// give POST /forget the scheme it needs (it defaults to https).
func TestStatus_SuitesShowTheirScheme(t *testing.T) {
	s, base, cleanup := startAdminServer(t)
	defer cleanup()
	for _, scheme := range []string{"http", "https"} {
		if err := s.cfg.Cache.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{CanonicalScheme: scheme,
			CanonicalHost: "deb.example", SuitePath: "/debian/dists/stable"}); err != nil {
			t.Fatal(err)
		}
	}
	resp := mustGet(t, base+"/?format=json")
	var doc struct {
		Suites []struct {
			Scheme string `json:"scheme"`
			Host   string `json:"host"`
		} `json:"suites"`
	}
	err := json.NewDecoder(resp.Body).Decode(&doc)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	schemes := map[string]bool{}
	for _, e := range doc.Suites {
		schemes[e.Scheme+" "+e.Host] = true
	}
	if !schemes["http deb.example"] || !schemes["https deb.example"] {
		t.Fatalf("JSON suites %+v, want an http and an https entry", doc.Suites)
	}
	resp = mustGet(t, base+"/")
	html, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, cell := range []string{`class="host">http://deb.example</td>`, `class="host">https://deb.example</td>`} {
		if !strings.Contains(string(html), cell) {
			t.Errorf("HTML lacks the Host cell %s", cell)
		}
	}
}
