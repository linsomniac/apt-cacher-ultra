package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Exercise a shared adopter and real transport together: overlapping suites,
// redirects, local metadata reuse, hot downloads, and hot reuse must each be
// attributed to exactly one attempt and reconcile to its terminal byte total.
func TestAdoptionLoggingConcurrentTransfersAndReuse(t *testing.T) {
	env := newAdoptionTestEnv(t)
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	suites := []SuiteRef{env.suite, env.suite}
	suites[1].SuitePath += "-updates"
	trackSuite(t, env.cache, suites[1])
	bodies := map[string][]byte{}
	releases := make([][]byte, 2)
	debs := [][]byte{[]byte("first package"), []byte("second package")}
	paths := []string{"/ubuntu/pool/foo_1.deb", "/ubuntu/pool/bar_1.deb"}
	for i, suite := range suites {
		pkgs := fakePackagesStanzas(map[string]string{strings.TrimPrefix(paths[i], "/ubuntu/"): shaOf(debs[i])})
		bodies[suite.SuitePath+"/main/binary-amd64/Packages"] = pkgs
		bodies[paths[i]] = debs[i]
		releases[i], _ = makeRelease(map[string][]byte{"main/binary-amd64/Packages": pkgs})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/redirected")
		if path == r.URL.Path {
			w.Header().Set("Location", "/redirected"+path)
			w.WriteHeader(http.StatusFound)
			return
		}
		body, ok := bodies[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	fc, err := fetch.New(fetch.Options{Proxy: server.URL, AllowedHostRegex: []string{`^archive\.ubuntu\.com$`}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	env.adopter.fetcher, env.adopter.logger = fc, logger
	env.adopter.hotPackagesWindow = 300 * 24 * time.Hour
	env.adopter.commitDelay = 20 * time.Millisecond
	run := func(round int) {
		var wg sync.WaitGroup
		for i, suite := range suites {
			wg.Go(func() {
				release := append([]byte(fmt.Sprintf("Label: round-%d\n", round)), releases[i]...)
				if err := env.adopter.Run(t.Context(), suite, release, "", ""); err != nil {
					t.Errorf("Run: %v", err)
				}
			})
		}
		wg.Wait()
	}
	run(1)
	for i, suite := range suites {
		hash := writeFixtureBlob(t, env.cache, debs[i])
		now := time.Now().Unix()
		if err := env.cache.PutURLPath(t.Context(), cache.URLPath{
			CanonicalScheme: suite.CanonicalScheme, CanonicalHost: suite.CanonicalHost,
			Path: paths[i], BlobHash: &hash, UpstreamURL: "http://" + suite.CanonicalHost + paths[i],
			RequestCount: 1, LastRequestedAt: &now,
		}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := os.Remove(env.cache.BlobPath(hash)); err != nil {
				t.Fatal(err)
			}
		}
	}
	run(2)
	starts, finishes := logRecords(t, logs, "adoption_started"), logRecords(t, logs, "adoption_finished")
	if len(starts) != 4 || len(finishes) != 4 {
		t.Fatalf("starts=%d finishes=%d\n%s", len(starts), len(finishes), logs.String())
	}
	ids := map[any]bool{}
	for _, start := range starts {
		id := start["adoption_id"]
		if id == nil || id == "" || ids[id] {
			t.Fatalf("invalid/duplicate ID: %v", start)
		}
		ids[id] = true
	}
	transfers := logRecords(t, logs, "upstream_response")
	if len(transfers) != 6 {
		t.Fatalf("transfers=%d, want two metadata downloads and one hot download, each redirected", len(transfers))
	}
	for _, transfer := range transfers {
		if !ids[transfer["adoption_id"]] {
			t.Fatalf("transfer has no matching attempt: %v", transfer)
		}
	}
	var hotFetched, hotReused, membersReused float64
	for _, finish := range finishes {
		id := finish["adoption_id"]
		if !ids[id] || finish["outcome"] != "success" || finish["resulting_snapshot_id"].(float64) <= 0 {
			t.Fatalf("finish: %v", finish)
		}
		delete(ids, id)
		var bytes, responses, memberBytes, hotBytes float64
		for _, transfer := range logRecords(t, logs, "upstream_response") {
			if transfer["adoption_id"] != id {
				continue
			}
			bytes += transfer["bytes_read"].(float64)
			responses++
			if transfer["purpose"] == "adoption_member" {
				memberBytes += transfer["bytes_read"].(float64)
			}
			if transfer["purpose"] == "hot_prefetch" {
				hotBytes += transfer["bytes_read"].(float64)
			}
		}
		if finish["upstream_bytes"] != bytes || finish["upstream_responses"] != responses || finish["member_upstream_bytes"] != memberBytes || finish["hot_prefetch_upstream_bytes"] != hotBytes {
			t.Fatalf("totals do not reconcile: %v; bytes=%v responses=%v", finish, bytes, responses)
		}
		if finish["previous_snapshot_id"] == float64(0) {
			want := float64(len(bodies[finish["suite_path"].(string)+"/main/binary-amd64/Packages"]))
			if memberBytes != want || hotBytes != 0 {
				t.Fatalf("first adoption bytes: %v; want member bytes %v", finish, want)
			}
		} else if finish["suite_path"] == suites[0].SuitePath {
			if memberBytes != 0 || hotBytes != float64(len(debs[0])) {
				t.Fatalf("hot download bytes: %v", finish)
			}
		} else if bytes != 0 {
			t.Fatalf("local reuse downloaded bytes: %v", finish)
		}
		phases := finish["phase_seconds"].(map[string]any)
		var total float64
		for _, duration := range phases {
			total += duration.(float64)
		}
		if phases["commit"].(float64) < .02 || total > finish["duration_seconds"].(float64) {
			t.Fatalf("timings: %v", finish)
		}
		hotFetched += finish["hot_fetched"].(float64)
		hotReused += finish["hot_reused"].(float64)
		membersReused += finish["members_reused"].(float64)
	}
	if hotFetched != 1 || hotReused != 1 || membersReused != 2 {
		t.Fatalf("hot fetched=%v reused=%v members reused=%v\n%s", hotFetched, hotReused, membersReused, logs.String())
	}
	for _, hot := range logRecords(t, logs, "adoption_hot_prefetch_complete") {
		if hot["duration_seconds"].(float64) < 0 {
			t.Fatal(hot)
		}
		for _, done := range finishes {
			if hot["adoption_id"] == done["adoption_id"] && hot["time"].(string) >= done["time"].(string) {
				t.Fatalf("prefetch completed after adoption: %v", hot)
			}
		}
	}
}

func TestAdoptionLoggingTerminalOutcomes(t *testing.T) {
	for _, outcome := range []string{"gpg_failed", "parse_failed", "member_fetch_failed", "canceled", "timed_out", "abandoned_at_shutdown", "panicked"} {
		t.Run(outcome, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			logs := &lockedBuffer{}
			env.adopter.logger = slog.New(slog.NewJSONHandler(logs, nil))
			ctx := t.Context()
			p := &adoptionPayload{form: adoptionFormInline, inlineBytes: []byte("invalid Release")}
			switch outcome {
			case "member_fetch_failed":
				p.inlineBytes, _ = makeRelease(map[string][]byte{"main/binary-amd64/Packages": []byte("missing member")})
			case "gpg_failed":
				env.adopter.verifier = failingVerifier{err: errors.New("invalid signature")}
			case "canceled", "abandoned_at_shutdown":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				if outcome == "abandoned_at_shutdown" {
					p.stop = ctx
				}
			case "timed_out":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "panicked":
				env.adopter.verifier = panickingVerifier{}
			}
			var err error
			var panicValue any
			func() {
				defer func() { panicValue = recover() }()
				err = env.adopter.runShared(ctx, env.suite, p, "", "")
			}()
			if outcome == "panicked" && panicValue == nil || outcome != "panicked" && err == nil {
				t.Fatalf("error=%v panic=%v", err, panicValue)
			}
			starts, ends := logRecords(t, logs, "adoption_started"), logRecords(t, logs, "adoption_finished")
			if len(starts) != 1 || len(ends) != 1 {
				t.Fatalf("events: %s", logs.String())
			}
			if ends[0]["outcome"] != outcome || ends[0]["adoption_id"] != starts[0]["adoption_id"] || ends[0]["resulting_snapshot_id"] != float64(0) {
				t.Fatalf("finish: %v", ends[0])
			}
		})
	}
}

func TestAdoptionLoggingRefusalRetainsPreviousSnapshot(t *testing.T) {
	env := newAdoptionTestEnv(t)
	logs := &lockedBuffer{}
	env.adopter.logger = slog.New(slog.NewJSONHandler(logs, nil))
	pkgs := fakePackagesStanzas(map[string]string{"pool/foo.deb": testHex64})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	release, _ := makeRelease(map[string][]byte{"main/binary-amd64/Packages": pkgs})
	for _, label := range []string{"old", "new", "old"} {
		body := append([]byte("Label: "+label+"\n"), release...)
		err := env.adopter.Run(t.Context(), env.suite, body, "", "")
		if err != nil && !errors.Is(err, ErrAdoptionRollbackRefused) {
			t.Fatal(err)
		}
	}
	ends := logRecords(t, logs, "adoption_finished")
	if len(ends) != 3 || ends[2]["outcome"] != "refused" || ends[2]["reason"] != "rollback_refused" || ends[2]["previous_snapshot_id"] != ends[1]["resulting_snapshot_id"] || ends[2]["resulting_snapshot_id"] != float64(0) {
		t.Fatalf("refusal: %v", ends)
	}
}

func TestAdoptionLoggingCheckerTriggersAndRetry(t *testing.T) {
	for _, source := range []string{"client", "periodic", "queued"} {
		t.Run(source, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			seedStaleAnchor(t, env.cache)
			suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
			trackSuite(t, env.cache, suite)
			logs := &lockedBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			env.adopter.logger, env.adopter.verifier = logger, panickingVerifier{}
			up := &changedUpstream{body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")}
			c, err := New(Config{Cache: env.cache, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: env.adopter, Logger: logger})
			if err != nil {
				t.Fatal(err)
			}
			c.check(t.Context(), suite, checkOpts{source: source})
			c.WaitForAdoptions()
			// Let the recorded content's backoff elapse without waiting in real time.
			future := time.Now().Add(2 * time.Hour)
			c.now = func() time.Time { return future }
			c.check(t.Context(), suite, checkOpts{source: source})
			c.WaitForAdoptions()
			starts := logRecords(t, logs, "adoption_started")
			if len(starts) != 2 {
				t.Fatalf("starts=%d\n%s", len(starts), logs.String())
			}
			if starts[0]["trigger_source"] != source || starts[0]["trigger_reason"] != "first_adoption" || starts[0]["retry"] != false || starts[1]["retry"] != true {
				t.Fatalf("triggers: %v", starts)
			}
			for _, event := range logRecords(t, logs, "adoption_panicked") {
				if event["adoption_id"] == nil {
					t.Fatalf("uncorrelated panic: %v", event)
				}
			}
		})
	}
}
