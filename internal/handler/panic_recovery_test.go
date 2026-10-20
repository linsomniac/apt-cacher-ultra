package handler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

type panickingChecker struct{}

func (panickingChecker) Check(context.Context, string, string, string) { panic("checker bug") }

// I-1: a panicking T1 check must not kill the process, must be logged,
// and must still release the handler's drain WaitGroup.
func TestMaybeFireFreshness_RecoversPanic(t *testing.T) {
	h := newTestHandlerWithFreshness(t, panickingChecker{})
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.maybeFireFreshness(&proxy.Request{CanonicalScheme: "https", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", IsMetadata: true, SuitePath: "/r/dists/x"})
	h.Close() // blocks until the goroutine finishes; returns only if Done ran
	out := logs.String()
	if !strings.Contains(out, "freshness_check_panicked") || !strings.Contains(out, "where=t1") {
		t.Fatalf("T1 panic not logged:\n%s", out)
	}
	// The suite's scheme, as every freshness_check_panicked line names it
	// (logPanic in internal/freshness): an http and an https copy of a
	// suite are told apart.
	for _, want := range []string{"canonical_scheme=https", "canonical_host=h", "suite_path=/r/dists/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("T1 panic line lacks %q:\n%s", want, out)
		}
	}
}
