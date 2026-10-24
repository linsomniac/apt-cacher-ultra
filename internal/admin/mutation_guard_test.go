package admin

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// guardedEndpoint is one of the admin listener's mutating endpoints, which
// requireMutationGuard protects (spec C9, §10 G-1).
type guardedEndpoint struct {
	path string
	// start runs an admin server whose stub answers this endpoint's form
	// body "host=h&suite=/s" with 202, and returns its base URL, the stub's
	// call count and the cleanup.
	start func(t *testing.T, opts ...adminOpt) (base string, calls func() int, cleanup func())
}

var guardedEndpoints = []guardedEndpoint{
	{path: "/forget", start: func(t *testing.T, opts ...adminOpt) (string, func() int, func()) {
		stub := &stubForgetter{res: forgotten(0)}
		_, base, cleanup := startAdminServer(t, append(opts, withForgetter(stub))...)
		return base, func() int { calls, _, _, _ := stub.snapshot(); return calls }, cleanup
	}},
	{path: "/reconcile", start: func(t *testing.T, opts ...adminOpt) (string, func() int, func()) {
		stub := &stubReconciler{result: true}
		_, base, cleanup := startAdminServer(t, append(opts, withReconciler(stub))...)
		return base, func() int { return stub.calls }, cleanup
	}},
}

// postForm sends POST url with the form body "host=h&suite=/s"; edit, when
// non-nil, sets the request's headers. It returns the status and the body.
func postForm(t *testing.T, url string, edit func(*http.Request)) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("host=h&suite=/s"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if edit != nil {
		edit(req)
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
	return resp.StatusCode, string(text)
}

// postReconcile sends POST /reconcile as a script does: a form body and
// X-ACU-Admin: 1 (spec §10 G-1).
func postReconcile(base, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/reconcile", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ACU-Admin", "1")
	return http.DefaultClient.Do(req)
}

// Spec C9, §10 G-1 and ruling PF-6: a cross-site form or fetch cannot
// trigger /forget or /reconcile (requireMutationGuard). A form POST (no
// X-ACU-Admin: 1) and a cross-site or same-site fetch (Sec-Fetch-Site) are
// refused with 403 before the endpoint's work runs, the body saying which
// rule refused it — a same-site fetch is refused too, so that text names
// any other origin, not only another site (the Task 10 re-review's
// out-of-scope 1); a same-origin request, a browser's direct navigation
// (none) and curl (no Sec-Fetch-Site) pass.
func TestMutatingEndpoints_RefuseCrossOriginRequests(t *testing.T) {
	const noHeader = "this endpoint requires the header X-ACU-Admin: 1"
	const otherOrigin = "requests from another origin are refused: Sec-Fetch-Site must be same-origin or none"
	for _, ep := range guardedEndpoints {
		for _, tc := range []struct {
			name    string
			headers []string
			status  int
			body    string // the 403's text
		}{
			{"form POST without the header", nil, http.StatusForbidden, noHeader},
			{"header not 1", []string{"X-ACU-Admin", "true"}, http.StatusForbidden, noHeader},
			{"cross-site fetch", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "cross-site"}, http.StatusForbidden, otherOrigin},
			{"same-site fetch", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "same-site"}, http.StatusForbidden, otherOrigin},
			{"same-origin", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "same-origin"}, http.StatusAccepted, ""},
			{"direct (none)", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "none"}, http.StatusAccepted, ""},
			{"curl", []string{"X-ACU-Admin", "1"}, http.StatusAccepted, ""},
		} {
			t.Run(strings.TrimPrefix(ep.path, "/")+" "+tc.name, func(t *testing.T) {
				base, calls, cleanup := ep.start(t)
				defer cleanup()
				status, body := postForm(t, base+ep.path, func(req *http.Request) {
					for i := 0; i+1 < len(tc.headers); i += 2 {
						req.Header.Set(tc.headers[i], tc.headers[i+1])
					}
				})
				if status != tc.status {
					t.Errorf("status = %d, want %d", status, tc.status)
				}
				if got := strings.TrimRight(body, "\n"); tc.status == http.StatusForbidden && got != tc.body {
					t.Errorf("403 body = %q, want %q", got, tc.body)
				}
				if want := map[bool]int{true: 1, false: 0}[tc.status == http.StatusAccepted]; calls() != want {
					t.Errorf("calls = %d, want %d", calls(), want)
				}
			})
		}
	}
}

const rebindingRefused = "without admin.htpasswd_file this endpoint accepts only an IP address or localhost " +
	"as the Host (a guard against DNS rebinding): connect by address, or set admin.htpasswd_file"

// Spec §10 G-1 (the maintainer's decision, 2026-10-24): without
// htpasswd_file, /forget and /reconcile refuse a request whose Host header
// is a DNS name other than localhost and admin.listen's own host. A page
// that rebinds its name to the listener's address makes same-origin
// requests that pass the header checks, but they carry that name as their
// Host; a browser that connects to an IP address or localhost sends it as
// the Host, and so do curl and scripts.
func TestMutatingEndpoints_RefuseRebindingHosts(t *testing.T) {
	for _, ep := range guardedEndpoints {
		for _, tc := range []struct {
			name   string
			host   string // "" keeps the URL's 127.0.0.1:port
			listen string // admin.listen
			status int
		}{
			{"a DNS name", "evil.example:6789", "", http.StatusForbidden},
			{"a DNS name without a port", "evil.example", "", http.StatusForbidden},
			{"a DNS name in capitals", "EVIL.EXAMPLE", "", http.StatusForbidden},
			{"a name under localhost's label", "localhost.evil.example", "", http.StatusForbidden},
			{"a name ending in localhost", "evillocalhost:6789", "", http.StatusForbidden},
			{"a name other than admin.listen's", "evil.example:6789", "cache01.example:6789", http.StatusForbidden},
			{"the IPv4 address", "", "", http.StatusAccepted},
			{"the IPv6 loopback", "[::1]:6789", "", http.StatusAccepted},
			{"the IPv6 loopback without a port", "[::1]", "", http.StatusAccepted},
			{"another IP address", "192.0.2.7", "", http.StatusAccepted},
			{"localhost", "localhost:6789", "", http.StatusAccepted},
			{"localhost in capitals", "LOCALHOST", "", http.StatusAccepted},
			{"admin.listen's host name", "cache01.example:6789", "cache01.example:6789", http.StatusAccepted},
			{"admin.listen's host name in capitals", "CACHE01.Example", "cache01.example:6789", http.StatusAccepted},
		} {
			t.Run(strings.TrimPrefix(ep.path, "/")+" "+tc.name, func(t *testing.T) {
				base, calls, cleanup := ep.start(t, func(cfg *Config) { cfg.Admin.Listen = tc.listen })
				defer cleanup()
				status, body := postForm(t, base+ep.path, func(req *http.Request) {
					req.Header.Set("X-ACU-Admin", "1")
					req.Header.Set("Sec-Fetch-Site", "same-origin")
					if tc.host != "" {
						req.Host = tc.host
					}
				})
				if status != tc.status {
					t.Errorf("status = %d, want %d (body %q)", status, tc.status, body)
				}
				if got := strings.TrimRight(body, "\n"); tc.status == http.StatusForbidden && got != rebindingRefused {
					t.Errorf("403 body = %q, want %q", got, rebindingRefused)
				}
				if want := map[bool]int{true: 1, false: 0}[tc.status == http.StatusAccepted]; calls() != want {
					t.Errorf("calls = %d, want %d", calls(), want)
				}
			})
		}
	}
}

// An HTTP/1.0 request may omit Host; no browser sends one so, and the
// guard refuses it like any Host it cannot match — also when admin.listen
// binds every interface (":6789", an empty host).
func TestMutatingEndpoints_RefuseAnEmptyHost(t *testing.T) {
	for _, ep := range guardedEndpoints {
		t.Run(strings.TrimPrefix(ep.path, "/"), func(t *testing.T) {
			base, calls, cleanup := ep.start(t, func(cfg *Config) { cfg.Admin.Listen = ":6789" })
			defer cleanup()
			conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			const body = "host=h&suite=/s"
			if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.0\r\nX-ACU-Admin: 1\r\n"+
				"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\n\r\n%s",
				ep.path, len(body), body); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			text, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || strings.TrimRight(string(text), "\n") != rebindingRefused || calls() != 0 {
				t.Fatalf("status %d, body %q, calls %d; want 403 %q and no call", resp.StatusCode, text, calls(), rebindingRefused)
			}
		})
	}
}

// With htpasswd_file the Host check is off: a rebinding page does not have
// the credentials, and an operator behind a reverse proxy that forwards
// the client's Host keeps working.
func TestMutatingEndpoints_HostCheckOffWithHtpasswd(t *testing.T) {
	for _, ep := range guardedEndpoints {
		t.Run(strings.TrimPrefix(ep.path, "/"), func(t *testing.T) {
			base, calls, cleanup := ep.start(t, withHtpasswd(makeBcryptHtpasswd(t, "alice", "secret")))
			defer cleanup()
			status, body := postForm(t, base+ep.path, func(req *http.Request) {
				req.Header.Set("X-ACU-Admin", "1")
				req.Host = "apt-admin.example"
				req.SetBasicAuth("alice", "secret")
			})
			if status != http.StatusAccepted || calls() != 1 {
				t.Fatalf("status = %d (body %q), calls = %d; want 202 and 1 call", status, body, calls())
			}
		})
	}
}

// The Host check is the mutating endpoints' alone (the maintainer's
// decision): the status page, /metrics and /healthz answer a request by
// any name, so a Prometheus server that scrapes the listener by host name
// keeps working.
func TestReadOnlyEndpoints_AnswerAnyHost(t *testing.T) {
	_, base, cleanup := startAdminServer(t)
	defer cleanup()
	for _, path := range []string{"/", "/metrics", "/healthz"} {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "cache01.example:6789"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with Host cache01.example: status %d, want 200", path, resp.StatusCode)
		}
	}
}
