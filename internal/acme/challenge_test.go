package acme

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestChallengeIsolation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := StartChallenge(ln, testName)
	defer c.Close()
	token := "abcdefghijklmnop_0123456789"
	if err := c.set(token, "test-authorization"); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	for _, tt := range []struct {
		name, method, path, host, body string
		status                         int
	}{
		{"valid", "GET", challengePrefix + token, testName, "", 200},
		{"head", "HEAD", challengePrefix + token, testName + ":80", "", 200},
		{"wrong host", "GET", challengePrefix + token, "attacker.example", "", 404},
		{"status page", "GET", "/", testName, "", 404},
		{"metrics", "GET", "/metrics", testName, "", 404},
		{"mutation", "POST", "/forget", testName, "", 405},
		{"query", "GET", challengePrefix + token + "?x=1", testName, "", 404},
		{"encoded", "GET", challengePrefix + "%61" + token[1:], testName, "", 404},
		{"body", "GET", challengePrefix + token, testName, "body", 404},
		{"inactive", "GET", challengePrefix + token + "x", testName, "", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest(tt.method, "http://"+ln.Addr().String()+tt.path, strings.NewReader(tt.body))
			r.Host = tt.host
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if tt.status == 200 && tt.method == "GET" && string(body) != "test-authorization" {
				t.Fatalf("body %q", body)
			}
		})
	}
	c.clear()
	r, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+challengePrefix+token, nil)
	r.Host = testName
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("token survived cleanup")
	}
	// An unfinished HTTP header must not delay cancellation/shutdown.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost:")
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("challenge shutdown stuck")
	}
}

func TestAuthorityTransport(t *testing.T) {
	calls := 0
	base := roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (2<<20)+1))), Request: r}, nil
	})
	client := newHTTPClient("https://ca.example/directory", base, nil)
	for _, s := range []string{"http://ca.example/order", "https://other.example/order", "https://ca.example:444/order", "https://ca.example/order?secret=1", "https://user:pass@ca.example/order"} {
		if resp, err := client.Get(s); err == nil {
			_ = resp.Body.Close()
			t.Errorf("accepted %s", s)
		}
	}
	if calls != 0 {
		t.Fatal("forbidden request reached transport")
	}
	resp, err := client.Get("https://ca.example/order")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err == nil || len(body) != 2<<20 {
		t.Fatalf("unbounded response: %d %v", len(body), err)
	}
	now := time.Now()
	if !retryAfter("7200", now).Equal(now.Add(2 * time.Hour)) {
		t.Fatal("seconds cooldown")
	}
	if !retryAfter("9223372036854775807", now).After(now.Add(365 * 24 * time.Hour)) {
		t.Fatal("overflow shortened cooldown")
	}
}
