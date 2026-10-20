package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

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
	detached            bool // the context it got can never be canceled
}

func (s *stubForgetter) Forget(ctx context.Context, scheme, host, suitePath string) (cache.ForgetResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.scheme, s.host, s.suite = scheme, host, suitePath
	s.detached = ctx.Done() == nil
	return s.res, s.err
}

func (s *stubForgetter) snapshot() (calls int, scheme, host, suite string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.scheme, s.host, s.suite
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
			"not forgotten: busy (try again) or no such suite (scheme defaults to https; host and suite as the status page shows them)"},
		{"database error 500", &stubForgetter{err: errors.New("disk I/O error")}, "host=h&suite=/s", http.StatusInternalServerError, 1,
			"forget failed"},
		{"missing suite 400", &stubForgetter{res: forgotten(0)}, "host=h", http.StatusBadRequest, 0,
			"host and suite are required; scheme is http or https"},
		{"another scheme 400", &stubForgetter{res: forgotten(0)}, "scheme=ftp&host=h&suite=/s", http.StatusBadRequest, 0,
			"host and suite are required; scheme is http or https"},
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
		stub := &stubForgetter{res: forgotten(0)}
		_, base, cleanup := startAdminServer(t, withForgetter(stub))
		defer cleanup()
		forget(t, base, "host=h&suite=/s", admin...)
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if !stub.detached {
			t.Error("Forget got the request's context; want one a disconnect cannot cancel")
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

// Spec C9 and ruling PF-6: a cross-site form or fetch cannot trigger
// /forget (requireMutationGuard). A form POST (no X-ACU-Admin: 1) and a
// cross-site or same-site fetch (Sec-Fetch-Site) are refused with 403
// before the Forgetter runs; a same-origin request, a browser's direct
// navigation (none) and curl (no Sec-Fetch-Site) pass. (DNS rebinding is
// not covered: spec §10 G-1.)
func TestEndpoint_Forget_RefusesCrossOriginRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		status  int
	}{
		{"form POST without the header", nil, http.StatusForbidden},
		{"header not 1", []string{"X-ACU-Admin", "true"}, http.StatusForbidden},
		{"cross-site fetch", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"same-site fetch", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "same-site"}, http.StatusForbidden},
		{"same-origin", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "same-origin"}, http.StatusAccepted},
		{"direct (none)", []string{"X-ACU-Admin", "1", "Sec-Fetch-Site", "none"}, http.StatusAccepted},
		{"curl", []string{"X-ACU-Admin", "1"}, http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubForgetter{res: forgotten(0)}
			_, base, cleanup := startAdminServer(t, withForgetter(stub))
			defer cleanup()
			if status, _, _ := forget(t, base, "host=h&suite=/s", tc.headers...); status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			calls, _, _, _ := stub.snapshot()
			if want := map[bool]int{true: 1, false: 0}[tc.status == http.StatusAccepted]; calls != want {
				t.Errorf("calls = %d, want %d", calls, want)
			}
		})
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
