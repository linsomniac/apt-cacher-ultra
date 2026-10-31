package admin

import (
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Ruling R-E2 (phase E): only the refusals whose suite already serves
// content at least as new as upstream hide its lag. Every phase E refusal
// — the suite is in fact behind — keeps it, so a stuck suite stays visible.
func TestRefusalKeepsCurrent_OnlyUpstreamOlderAndRollback(t *testing.T) {
	for reason, want := range map[string]bool{
		cache.ReasonUpstreamOlder:            true,
		cache.ReasonRollbackRefused:          true,
		cache.ReasonUnsignedDowngradeRefused: false,
		cache.ReasonUnsignedUnconfirmed:      false,
		cache.ReasonUnsignedTooLarge:         false,
		cache.ReasonAnchorAbsent:             false,
		"refused":                            false, // classifyAdoptionReason's fallback
	} {
		if got := refusalKeepsCurrent(reason); got != want {
			t.Errorf("refusalKeepsCurrent(%q) = %v, want %v", reason, got, want)
		}
	}
}
