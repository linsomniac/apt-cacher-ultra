package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// flipSimHost is the one upstream host of the flip simulations (mirror
// mode: every suite on one host, one shared client queue).
const flipSimHost = "mirror.example"

// flipStack is a real cache, adopter, flip barrier and handler for
// virtual-clock simulations of spec C8. Create it inside the bubble.
type flipStack struct {
	t       *testing.T
	c       *cache.Cache
	h       *Handler
	gate    *flipgate.Gate
	ad      *freshness.Adopter
	members *memberStore
}

// memberStore is the adopter's upstream: canned member bodies by URL.
type memberStore struct {
	mu     sync.Mutex
	bodies map[string][]byte
}

func (m *memberStore) put(url string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies[url] = body
}

func (m *memberStore) Fetch(_ context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	m.mu.Lock()
	body, ok := m.bodies[target.URL]
	m.mu.Unlock()
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	if _, err := dst.Write(body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(body))}, nil
}

// plainVerifier passes every body through as its own verified text.
type plainVerifier struct{}

func (plainVerifier) VerifyInline(_ context.Context, _ freshness.SuiteRef, b []byte) ([]byte, error) {
	return b, nil
}

func (plainVerifier) VerifyDetached(_ context.Context, _ freshness.SuiteRef, rel, _ []byte) ([]byte, error) {
	return rel, nil
}

// noUpstream answers every handler fetch 404: in these simulations every
// file a client gets comes from a snapshot.
type noUpstream struct{}

func (noUpstream) HostAllowed(string) bool { return true }

func (noUpstream) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	return nil, &fetch.StatusError{Code: 404}
}

func newFlipStack(t *testing.T, quiet time.Duration) *flipStack {
	t.Helper()
	return newFlipStackLogging(t, quiet, simtest.DiscardLogger())
}

// newFlipStackLogging is newFlipStack whose handler logs to logger.
func newFlipStackLogging(t *testing.T, quiet time.Duration, logger *slog.Logger) *flipStack {
	t.Helper()
	c := simtest.OpenCache(t)
	gate, err := flipgate.New(flipgate.Config{Quiet: quiet})
	if err != nil {
		t.Fatal(err)
	}
	members := &memberStore{bodies: map[string][]byte{}}
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: members, Verifier: plainVerifier{},
		HostLimiter: hostsem.New(64), Logger: simtest.DiscardLogger(), FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Parser: parser, Cache: c, Fetch: noUpstream{}, Logger: logger,
		HostLimiter: hostsem.New(64), FlipGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close) // registered after the cache's cleanup, so it runs first
	return &flipStack{t: t, c: c, h: h, gate: gate, ad: ad, members: members}
}

// publish puts generation gen of suite's Packages upstream and returns
// the suite and its Release text.
func (s *flipStack) publish(suite string, gen int) (freshness.SuiteRef, []byte) {
	pkgs := []byte(fmt.Sprintf("Package: p\nArchitecture: amd64\nVersion: %d\nFilename: pool/p_%d_amd64.deb\nSize: 1\nSHA256: %064x\n\n",
		gen, gen, gen))
	s.members.put("http://"+flipSimHost+suite+"/main/binary-amd64/Packages", pkgs)
	sum := sha256.Sum256(pkgs)
	rel := []byte(fmt.Sprintf("Label: %s generation %d\nSHA256:\n %s %d main/binary-amd64/Packages\n",
		suite, gen, hex.EncodeToString(sum[:]), len(pkgs)))
	return freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: flipSimHost, SuitePath: suite}, rel
}

// adopt publishes generation gen of suite upstream and adopts it (inline
// form) through the barrier; it returns once the commit is done. The
// InRelease is clearsigned (spec §7 "Fixtures"); plainVerifier hands the
// whole envelope to ParseRelease, which skips the armor lines.
func (s *flipStack) adopt(suite string, gen int) {
	ref, rel := s.publish(suite, gen)
	if err := s.ad.Run(context.Background(), ref, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		s.t.Errorf("adopt %s generation %d: %v", suite, gen, err)
	}
}

// current returns suite's current snapshot id as X-Cache-Snapshot shows it.
func (s *flipStack) current(suite string) string {
	sf, err := s.c.GetSuiteFreshness(context.Background(), "http", flipSimHost, suite)
	if err != nil || sf.CurrentSnapshotID == nil {
		return ""
	}
	return strconv.FormatInt(*sf.CurrentSnapshotID, 10)
}

// get serves one GET of path. The response takes transfer to reach the
// client, and the handler is still writing it — in flight — meanwhile.
// It returns the response's X-Cache-Snapshot and status.
func (s *flipStack) get(path string, transfer time.Duration) (gen string, code int) {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(&slowWriter{ResponseRecorder: rec, delay: transfer},
		httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
	return rec.Header().Get(hdrXCacheSnapshot), rec.Code
}

// slowWriter models a client's link: the first body write takes delay.
type slowWriter struct {
	*httptest.ResponseRecorder
	delay time.Duration
	slow  bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	if !w.slow {
		w.slow = true
		time.Sleep(w.delay)
	}
	return w.ResponseRecorder.Write(p)
}
