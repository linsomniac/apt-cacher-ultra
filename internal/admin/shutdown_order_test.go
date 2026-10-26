package admin

import (
	"context"
	"testing"
	"time"
)

// Deferred minor F-23: a Shutdown that comes before the refresher starts —
// while Serve's synchronous first refresh still runs — leaves no refresher
// to start, since nothing would ever stop it.
func TestServer_NoRefresherStartsAfterShutdown(t *testing.T) {
	s, ln, c := newAdminServer(t)
	defer func() { _ = c.Close() }()
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	s.startRefresher() // what Serve does once its first refresh returns
	s.mu.Lock()
	started := s.refresherDone != nil
	s.mu.Unlock()
	if started {
		t.Fatal("a refresher started after Shutdown: nothing stops it")
	}
}
