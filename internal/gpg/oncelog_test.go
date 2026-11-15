package gpg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

func TestVerifierLogsAdoptionContext(t *testing.T) {
	var logs bytes.Buffer
	v := &Verifier{logger: slog.New(slog.NewJSONHandler(&logs, nil)), requirePinned: true}
	suite := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"}
	first, second := observability.NewAdoptionTrace(), observability.NewAdoptionTrace()
	_, _ = v.VerifyInline(observability.WithAdoptionTrace(t.Context(), first), suite, nil)
	_, _ = v.VerifyDetached(observability.WithAdoptionTrace(t.Context(), second), suite, []byte("Release"), []byte("signature"))
	_, _ = v.VerifyInline(t.Context(), suite, nil)
	decoder := json.NewDecoder(&logs)
	for _, want := range []any{first.ID, second.ID, nil} {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["msg"] != "adoption_unpinned_suite" || event["adoption_id"] != want {
			t.Fatalf("verifier log: %v; want adoption_id=%v", event, want)
		}
	}
}

// Spec C9: each once-logger set holds at most 10,000 keys; a new key when
// it is full starts the set over, so an earlier suite logs its once-line
// again instead of the set growing for the process lifetime.
func TestOnceSet_StartsOverPastTenThousandKeys(t *testing.T) {
	var s onceSet
	if !s.first("a") || s.first("a") {
		t.Fatal("a key is not new exactly once")
	}
	for i := 1; i < onceSetMax; i++ {
		if !s.first(fmt.Sprintf("k%d", i)) {
			t.Fatalf("key %d was not new", i)
		}
	}
	if s.first("a") {
		t.Fatal("a full set forgot a key before overflowing")
	}
	if !s.first("overflow") {
		t.Fatal("the 10,001st key was not new")
	}
	if !s.first("a") {
		t.Fatal("after starting over, the set still remembered an old key")
	}
	if n := len(s.keys); n != 2 {
		t.Fatalf("the set holds %d keys after starting over, want 2", n)
	}
}

// countingHandler counts log records by message.
type countingHandler struct {
	mu sync.Mutex
	n  map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n[r.Message]++
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// The Task 12 review's minor 1: each of the three once-loggers keeps its
// own set — a suite one has logged is still new to the other two — and
// logs a suite once. A logger wired to another's set logs nothing here.
func TestVerifier_EachOnceLoggerHasItsOwnSet(t *testing.T) {
	h := &countingHandler{n: map[string]int{}}
	v := &Verifier{logger: slog.New(h)}
	signer := newTestEntity(t, "Once Logger", "once@example.invalid")
	suite := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/s"}
	for range 2 {
		v.logUntrustedSignerOnce(t.Context(), suite, nil)
		v.logShortKeyIDFallbackOnce(t.Context(), suite, signer, signer.PrimaryKey.KeyId)
		v.logUnverifiedSignerOnce(t.Context(), suite, sigPacket{n: 1, issuer: signer.PrimaryKey.KeyId})
	}
	for _, msg := range []string{"adoption_untrusted_signer", "adoption_short_keyid_fallback", "adoption_unverified_signer"} {
		if n := h.n[msg]; n != 1 {
			t.Errorf("%s logged %d times for one suite, want 1", msg, n)
		}
	}
}

// The verifier's once-loggers use the bounded set: the untrusted-signer
// line is logged once per suite, and again for a suite seen before once
// 10,000 others have filled the set.
func TestVerifier_OnceLoggersAreBounded(t *testing.T) {
	h := &countingHandler{n: map[string]int{}}
	v := &Verifier{logger: slog.New(h)}
	suite := func(path string) freshness.SuiteRef {
		return freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: path}
	}
	v.logUntrustedSignerOnce(t.Context(), suite("/first"), nil)
	v.logUntrustedSignerOnce(t.Context(), suite("/first"), nil)
	for i := 1; i < onceSetMax; i++ {
		v.logUntrustedSignerOnce(t.Context(), suite(fmt.Sprintf("/s%d", i)), nil)
	}
	v.logUntrustedSignerOnce(t.Context(), suite("/overflow"), nil)
	v.logUntrustedSignerOnce(t.Context(), suite("/first"), nil)
	if n := h.n["adoption_untrusted_signer"]; n != onceSetMax+2 {
		t.Fatalf("adoption_untrusted_signer logged %d times, want %d", n, onceSetMax+2)
	}
}
