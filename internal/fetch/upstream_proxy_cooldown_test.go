package fetch

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestUpstreamProxy_CooldownSharedAcrossRepositories(t *testing.T) {
	var mu sync.Mutex
	var deadlines []bool
	var addresses []string
	client := newUpstreamProxyClient(t, Options{
		Proxy:                   "http://proxy.invalid:3128",
		AllowedHostRegex:        []string{`^(first|second)\.invalid$`},
		UnreachableCooldown:     time.Minute,
		UnreachableProbeTimeout: time.Second,
		dialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			_, limited := ctx.Deadline()
			mu.Lock()
			addresses = append(addresses, addr)
			deadlines = append(deadlines, limited)
			mu.Unlock()
			return nil, errors.New("simulated proxy dial failure")
		},
	})
	for i, host := range []string{"first.invalid", "second.invalid"} {
		_, err := client.Fetch(context.Background(), &Target{host, "http://" + host + "/InRelease"}, &bufDst{})
		if !errors.Is(err, ErrUpstreamUnavailable) {
			t.Fatalf("repository %d error = %v, want unavailable", i, err)
		}
		if errors.Is(err, ErrHostUnreachable) != (i == 1) {
			t.Errorf("repository %d error = %v; only the second should inherit cooldown", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(addresses) != 2 || addresses[0] != "proxy.invalid:3128" || addresses[1] != addresses[0] {
		t.Fatalf("dial endpoints = %v, want two proxy dials", addresses)
	}
	if deadlines[0] || !deadlines[1] {
		t.Errorf("probe deadlines = %v, want [false true]", deadlines)
	}
}
