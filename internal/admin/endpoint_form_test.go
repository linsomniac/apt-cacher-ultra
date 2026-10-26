package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postAdminBody sends POST url with body as a form and X-ACU-Admin: 1,
// as a script does, and returns the status and the response body.
func postAdminBody(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-ACU-Admin", "1")
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

// Deferred minor F-5, Review Focus 3: a body that is not a valid form (a
// stray "%", as from a script that forgets --data-urlencode) is 400
// "malformed form body" from both mutating endpoints, never 413; only a
// body over 8 KiB is 413. Either way the endpoint's work never runs, and
// the body ends in one newline (F-6).
func TestMutatingEndpoints_MalformedBodyIs400(t *testing.T) {
	for _, ep := range guardedEndpoints {
		t.Run(ep.path, func(t *testing.T) {
			base, calls, cleanup := ep.start(t)
			defer cleanup()
			if status, body := postAdminBody(t, base+ep.path, "host=h&suite=/a%zz"); status != http.StatusBadRequest ||
				body != "malformed form body\n" {
				t.Fatalf("a malformed body: %d %q, want 400 \"malformed form body\\n\"", status, body)
			}
			if status, body := postAdminBody(t, base+ep.path, "host=h&suite=/s&pad="+strings.Repeat("x", 9<<10)); status != http.StatusRequestEntityTooLarge ||
				body != "request body too large\n" {
				t.Fatalf("a body over 8 KiB: %d %q, want 413 \"request body too large\\n\"", status, body)
			}
			if n := calls(); n != 0 {
				t.Fatalf("the endpoint ran %d times, want never", n)
			}
		})
	}
}

// Deferred minor F-6: every error body of the mutating endpoints ends in
// exactly one newline (http.Error adds it; the messages carry none) — the
// guard's 403, a missing parameter's 400, a refusal's 409 and a 501 — and
// OPTIONS /reconcile names POST, as OPTIONS /forget does.
func TestMutatingEndpoints_BodiesEndInOneNewlineAndOptionsNamePost(t *testing.T) {
	checkBody := func(what, body string) {
		t.Helper()
		if !strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\n\n") {
			t.Errorf("%s: body %q, want exactly one trailing newline", what, body)
		}
	}
	_, base, cleanup := startAdminServer(t, withReconciler(&stubReconciler{result: false}),
		withForgetter(&stubForgetter{})) // a zero result: not deleted, 409
	defer cleanup()
	for _, path := range []string{"/reconcile", "/forget"} {
		status, body := postForm(t, base+path, nil) // no X-ACU-Admin: the guard's 403
		if status != http.StatusForbidden {
			t.Fatalf("%s without X-ACU-Admin: %d, want 403", path, status)
		}
		checkBody(path+" 403", body)
		status, body = postAdminBody(t, base+path, "host=h")
		if status != http.StatusBadRequest {
			t.Fatalf("%s without suite: %d, want 400", path, status)
		}
		checkBody(path+" 400", body)
		status, body = postAdminBody(t, base+path, "host=h&suite=/s")
		if status != http.StatusConflict {
			t.Fatalf("%s refused: %d, want 409", path, status)
		}
		checkBody(path+" 409", body)
		req, err := http.NewRequest(http.MethodOptions, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Allow") != "POST, OPTIONS" {
			t.Fatalf("OPTIONS %s: %d, Allow %q; want 204 and \"POST, OPTIONS\"", path, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	_, unwired, cleanup2 := startAdminServer(t)
	defer cleanup2()
	for _, path := range []string{"/reconcile", "/forget"} {
		status, body := postAdminBody(t, unwired+path, "host=h&suite=/s")
		if status != http.StatusNotImplemented {
			t.Fatalf("%s unwired: %d, want 501", path, status)
		}
		checkBody(path+" 501", body)
	}
}

// Deferred minor F-16 (design note 4): admin_reconcile_requested keeps
// scheme, which it had on main, and gains canonical_scheme beside it, so
// a filter on either finds it.
func TestEndpoint_Reconcile_LogsBothSchemeFields(t *testing.T) {
	var buf lockedBuffer
	stub := &stubReconciler{result: true}
	_, base, cleanup := startAdminServer(t, withReconciler(stub), withLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	defer cleanup()
	if status, _ := postAdminBody(t, base+"/reconcile", "scheme=http&host=h&suite=/s"); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil || m["msg"] != "admin_reconcile_requested" {
			continue
		}
		found = true
		if m["scheme"] != "http" || m["canonical_scheme"] != "http" || m["canonical_host"] != "h" || m["suite_path"] != "/s" {
			t.Fatalf("admin_reconcile_requested %v, want scheme and canonical_scheme http", m)
		}
	}
	if !found {
		t.Fatal("no admin_reconcile_requested line")
	}
}

// The H1 final review's ops I-1 (parked item 11): a scheme other than
// http or https, and a host holding one (the status page's Host cell,
// scheme://host, pasted whole), are 400 from both mutating endpoints with
// /forget's hints. /reconcile answered both 409 "not triggered (busy, …)",
// a request that could never succeed read as one to retry. Neither runs
// the endpoint's work or logs its requested line.
func TestMutatingEndpoints_SchemeAndSchemeInHostAre400(t *testing.T) {
	for _, ep := range guardedEndpoints {
		for _, tc := range []struct{ name, body, text string }{
			{"another scheme", "scheme=ftp&host=h&suite=/s", "host and suite are required; scheme is http or https"},
			{"a scheme in host", "host=https://h&suite=/s",
				"host is the host name alone: pass the scheme in scheme (http or https) and the host without it"},
		} {
			t.Run(strings.TrimPrefix(ep.path, "/")+" "+tc.name, func(t *testing.T) {
				var buf lockedBuffer
				base, calls, cleanup := ep.start(t, withLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
				defer cleanup()
				if status, body := postAdminBody(t, base+ep.path, tc.body); status != http.StatusBadRequest ||
					body != tc.text+"\n" {
					t.Errorf("%q: %d %q, want 400 %q", tc.body, status, body, tc.text+"\n")
				}
				if n := calls(); n != 0 {
					t.Errorf("the endpoint ran %d times, want never", n)
				}
				if strings.Contains(buf.String(), "_requested") {
					t.Errorf("a 400 logged a requested line: %s", buf.String())
				}
			})
		}
	}
}

// Deferred minor F-4: one parser for both endpoints — host, suite and
// scheme trimmed, an empty scheme https.
func TestParseSuiteForm(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/forget", strings.NewReader("host=+h+&suite=%20/s%20&scheme="))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f, ok := parseSuiteForm(httptest.NewRecorder(), r)
	if !ok || f != (suiteForm{scheme: "https", host: "h", suite: "/s"}) {
		t.Fatalf("parseSuiteForm = %+v, %v; want https h /s", f, ok)
	}
}

// Deferred minor F-6's OPTIONS under G-1's mutation guard (execution
// review G-5): OPTIONS answers Allow and nothing more, never a CORS grant.
// An Access-Control-Allow-* header on a mutating endpoint's preflight
// would let a page on another origin send the X-ACU-Admin header the
// guard relies on.
func TestOptions_GrantsNoCORS(t *testing.T) {
	_, base, cleanup := startAdminServer(t, withReconciler(&stubReconciler{result: true}), withForgetter(&stubForgetter{}))
	defer cleanup()
	for _, path := range []string{"/forget", "/reconcile", "/", "/metrics"} {
		req, err := http.NewRequest(http.MethodOptions, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "X-ACU-Admin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		for name := range resp.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-Allow") {
				t.Errorf("OPTIONS %s answered %s: %q; want no CORS grant", path, name, resp.Header.Get(name))
			}
		}
	}
}
