package freshness

import (
	"testing"
	"time"
)

// Spec C7 content key: inline sha256(InRelease); detached
// sha256(sha256(Release) || sha256(Release.gpg)).
func TestContentKey(t *testing.T) {
	rel, sig1, sig2 := []byte("Release body"), []byte("sig one"), []byte("sig two")
	if got := contentKey(adoptionFormInline, rel, nil, nil); got != hashOf(rel) {
		t.Fatalf("inline key = %s, want sha256 of the body", got)
	}
	k1 := contentKey(adoptionFormDetached, nil, rel, sig1)
	if k1 != contentKey(adoptionFormDetached, nil, rel, sig1) {
		t.Fatal("detached key not deterministic")
	}
	if k1 == contentKey(adoptionFormDetached, nil, rel, sig2) {
		t.Fatal("a re-signature must change the detached key")
	}
	if k1 == hashOf(rel) || len(k1) != 64 {
		t.Fatalf("detached key %q must be a distinct sha256 hex", k1)
	}
}

func TestBackoffDelay(t *testing.T) {
	want := map[int64]time.Duration{
		0: 0, 1: 2 * time.Minute, 2: 4 * time.Minute, 3: 8 * time.Minute, 4: 16 * time.Minute,
		5: 32 * time.Minute, 6: time.Hour, 7: time.Hour, 32: time.Hour, 1 << 40: time.Hour,
	}
	for n, w := range want {
		if got := backoffDelay(n); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", n, got, w)
		}
	}
}

func TestJitterDelay(t *testing.T) {
	d := 40 * time.Minute
	if got := jitterDelay(d, 0.5); got != d {
		t.Fatalf("u=0.5: %v, want %v", got, d)
	}
	if got := jitterDelay(d, 0); got != 30*time.Minute {
		t.Fatalf("u=0: %v, want 30m (-25%%)", got)
	}
	if got := jitterDelay(d, 0.999999); got >= 50*time.Minute || got < 49*time.Minute {
		t.Fatalf("u->1: %v, want just under 50m (+25%%)", got)
	}
	if retryHorizon != 75*time.Minute {
		t.Fatalf("retryHorizon = %v, want 75m (1h + 25%% jitter)", retryHorizon)
	}
}
