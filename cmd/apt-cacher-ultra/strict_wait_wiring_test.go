package main

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// Spec C10: strict mode waits on the checker's adoptions in flight when
// adoption is enabled (main builds an adopter); with no adopter, or no
// checker, there is nothing to wait for and the field stays a nil
// interface (a typed nil would be called).
func TestHandlerConfig_StrictModeWaitsOnTheCheckersAdoptions(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	cfg := newAdoptionEnabledCfg(t, false)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatal(err)
	}
	ck, err := freshness.New(checkerConfig(context.Background(), cfg, c, fetcher, hosts, silentBuildLogger(), a, nil))
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), ck, a).Adoptions.(*freshness.Checker); !ok || w != ck {
		t.Fatal("the handler does not wait on the checker's adoptions")
	}
	if hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), ck, nil); hc.Adoptions != nil {
		t.Fatalf("no adopter, yet Adoptions = %T", hc.Adoptions)
	}
	if hc := handlerConfig(cfg, nil, c, fetcher, hosts, silentBuildLogger(), nil, a); hc.Adoptions != nil {
		t.Fatalf("no checker, yet Adoptions = %T", hc.Adoptions)
	}
}
