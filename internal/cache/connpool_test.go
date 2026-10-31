package cache

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"testing"
	"testing/synctest"
	"time"
)

// holdConns opens n read connections at once, as n concurrent requests do,
// then hands them all back to the pool. It first lets the writer goroutine
// pin its own connection (a no-op write): pinned later, it would take one
// of the connections just handed back.
func holdConns(t *testing.T, c *Cache, n int) {
	t.Helper()
	if err := c.submitWrite(context.Background(), func(context.Context, *sql.Conn) error { return nil }); err != nil {
		t.Fatalf("pin the writer: %v", err)
	}
	conns := make([]*sql.Conn, 0, n)
	for range n {
		conn, err := c.db.Conn(context.Background())
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("Conn.Close: %v", err)
		}
	}
}

// Security review E-5/I-9: with database/sql's default of 2 idle
// connections, a burst of concurrent reads closed every connection beyond
// the second and the next burst reopened it (re-running the DSN pragmas),
// enough CPU churn to starve the writer. The pool keeps exactly
// max(16, 2*GOMAXPROCS) idle connections: a burst of one more than that
// closes exactly one on release.
func TestOpen_ConcurrentReadersKeepTheirConnections(t *testing.T) {
	cases := []struct {
		procs     int
		idleConns int
	}{
		{procs: 1, idleConns: 16},
		{procs: 8, idleConns: 16},
		{procs: 12, idleConns: 24},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("GOMAXPROCS=%d", tc.procs), func(t *testing.T) {
			prev := runtime.GOMAXPROCS(tc.procs)
			t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
			c := openCache(t)
			holdConns(t, c, tc.idleConns+1)
			if st := c.db.Stats(); st.MaxIdleClosed != 1 {
				t.Fatalf("GOMAXPROCS=%d: %d of %d concurrent read connections were closed on release, want 1 (idle %d)",
					tc.procs, st.MaxIdleClosed, tc.idleConns+1, st.Idle)
			}
		})
	}
}

// The idle connections a burst leaves behind stay open through a quiet
// day: the pool has no idle timeout or lifetime (the maintainer's
// decision, 2026-10-24). A 5-minute idle timeout made every quiet gap cost
// a reopen of each connection, which doubled the 90-day simulation under
// -race; the connections it would free are bounded by the cap.
func TestOpen_IdleConnectionsStayThroughQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := Open(context.Background(), t.TempDir(), discardSlog())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = c.Close() }()
		holdConns(t, c, 16)
		time.Sleep(24 * time.Hour)
		if st := c.db.Stats(); st.Idle < 16 || st.MaxIdleTimeClosed != 0 || st.MaxLifetimeClosed != 0 {
			t.Fatalf("after a quiet day: idle %d, closed for idle time %d, for lifetime %d; want at least 16 idle and none closed",
				st.Idle, st.MaxIdleTimeClosed, st.MaxLifetimeClosed)
		}
	})
}
