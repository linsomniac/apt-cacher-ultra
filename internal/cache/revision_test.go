package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func readDatabaseRevision(t *testing.T, c *Cache) DatabaseRevision {
	t.Helper()
	revision, err := c.DatabaseRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revision == (DatabaseRevision{}) {
		t.Fatal("successful revision must not equal the invalid zero token")
	}
	return revision
}

func TestDatabaseRevision_UnchangedAcrossReadsAndPoolChurn(t *testing.T) {
	c := openCache(t)
	before := readDatabaseRevision(t, c)
	// Expire ordinary read connections aggressively. The observer must keep
	// using its original pinned connection, whose counter is comparable.
	c.db.SetMaxIdleConns(0)
	c.db.SetConnMaxLifetime(time.Nanosecond)
	for i := 0; i < 5; i++ {
		if _, err := c.GetCacheStats(context.Background()); err != nil {
			t.Fatal(err)
		}
		if after := readDatabaseRevision(t, c); after != before {
			t.Fatalf("read/pool churn changed revision: before=%+v after=%+v", before, after)
		}
	}
	// Tokens cannot be transferred between different cache instances.
	other := openCache(t)
	if readDatabaseRevision(t, other) == before {
		t.Fatal("different observers produced equal tokens")
	}
}

func TestDatabaseRevision_ProductionWriterAndExternalCommit(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	before := readDatabaseRevision(t, c)
	hash := fmt.Sprintf("%064x", 1)
	if err := c.PutBlob(ctx, hash, 10); err != nil {
		t.Fatal(err)
	}
	writerRevision := readDatabaseRevision(t, c)
	if writerRevision == before {
		t.Fatal("production writer commit was not detected")
	}

	external, err := openDB(filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = external.Close() }()
	// An in-place UPDATE preserves table row counts but changes summaries.
	// It bypasses submitWrite, so a Go-side writer generation would miss it.
	if _, err := external.ExecContext(ctx, "UPDATE blob SET size = 20 WHERE hash = ?", hash); err != nil {
		t.Fatal(err)
	}
	externalRevision := readDatabaseRevision(t, c)
	if externalRevision == writerRevision {
		t.Fatal("external in-place commit was not detected")
	}
	if _, err := c.db.ExecContext(ctx, "UPDATE blob SET size = 30 WHERE hash = ?", hash); err != nil {
		t.Fatal(err)
	}
	if readDatabaseRevision(t, c) == externalRevision {
		t.Fatal("direct pooled-connection commit was not detected")
	}
}

func TestDatabaseRevision_UncommittedAndRolledBackWritesDoNotInvalidate(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	hash := fmt.Sprintf("%064x", 2)
	if err := c.PutBlob(ctx, hash, 10); err != nil {
		t.Fatal(err)
	}
	before := readDatabaseRevision(t, c)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "UPDATE blob SET size = 99 WHERE hash = ?", hash); err != nil {
		t.Fatal(err)
	}
	if readDatabaseRevision(t, c) != before {
		t.Fatal("uncommitted WAL write changed the observed revision")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if readDatabaseRevision(t, c) != before {
		t.Fatal("rolled-back write changed the observed revision")
	}
}

func TestDatabaseRevision_DoesNotRetainReadTransaction(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	before := readDatabaseRevision(t, c)
	if err := c.PutBlob(ctx, fmt.Sprintf("%064x", 3), 10); err != nil {
		t.Fatal(err)
	}
	after := readDatabaseRevision(t, c)
	if after == before {
		t.Fatal("observer retained an old snapshot across the committed write")
	}
	// TRUNCATE needs readers to release their WAL snapshots. Polling the
	// observer must not pin one and obstruct database maintenance.
	var busy, walPages, checkpointed int
	if err := c.db.QueryRowContext(ctx, "PRAGMA main.wal_checkpoint(TRUNCATE)").Scan(&busy, &walPages, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if busy != 0 || walPages != 0 || checkpointed != 0 {
		t.Fatalf("observer blocked WAL truncation: busy=%d wal=%d checkpointed=%d", busy, walPages, checkpointed)
	}
}

func TestDatabaseRevision_CanceledPollAndObserverRecovery(t *testing.T) {
	c := openCache(t)
	before := readDatabaseRevision(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := c.DatabaseRevision(ctx); !errors.Is(err, context.Canceled) || got != (DatabaseRevision{}) {
		t.Fatalf("canceled poll = %+v, %v; want zero token and cancellation", got, err)
	}
	if readDatabaseRevision(t, c) != before {
		t.Fatal("already-canceled poll disturbed a healthy observer")
	}

	// Simulate loss of the pinned connection without adding a production
	// hook. The accessor must return an error, then replace the observer.
	c.revisionMu.Lock()
	old := c.revisionConn
	if err := old.Close(); err != nil {
		c.revisionMu.Unlock()
		t.Fatal(err)
	}
	c.revisionMu.Unlock()
	if got, err := c.DatabaseRevision(context.Background()); !errors.Is(err, sql.ErrConnDone) || got != (DatabaseRevision{}) {
		t.Fatalf("failed observer poll = %+v, %v; want zero token and ErrConnDone", got, err)
	}
	after := readDatabaseRevision(t, c)
	if after == before || after.epoch == before.epoch {
		t.Fatalf("replacement reused the old observer's token: before=%+v after=%+v", before, after)
	}
	if again := readDatabaseRevision(t, c); again != after {
		t.Fatalf("replacement did not remain stable: first=%+v next=%+v", after, again)
	}
}

func TestDatabaseRevision_ConnectionAcquisitionHonorsCancellation(t *testing.T) {
	c := openCache(t)
	// Ensure the production writer owns its pinned connection, then prevent
	// another connection from being opened until the poll's deadline expires.
	if err := c.PutBlob(context.Background(), fmt.Sprintf("%064x", 4), 10); err != nil {
		t.Fatal(err)
	}
	c.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got, err := c.DatabaseRevision(ctx); !errors.Is(err, context.DeadlineExceeded) || got != (DatabaseRevision{}) {
		t.Fatalf("blocked acquisition = %+v, %v; want zero token and deadline", got, err)
	}
	c.db.SetMaxOpenConns(0)
	readDatabaseRevision(t, c)
}

func TestDatabaseRevision_ConcurrentPollsAndClose(t *testing.T) {
	c := openCache(t)
	readDatabaseRevision(t, c)
	const pollers = 12
	start := make(chan struct{})
	errs := make(chan error, pollers)
	var wg sync.WaitGroup
	for i := 0; i < pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 50; j++ {
				if _, err := c.DatabaseRevision(context.Background()); err != nil {
					if !errors.Is(err, ErrClosed) {
						errs <- err
					}
					return
				}
			}
		}()
	}
	close(start)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent poll: %v", err)
	}
	if got, err := c.DatabaseRevision(context.Background()); !errors.Is(err, ErrClosed) || got != (DatabaseRevision{}) {
		t.Fatalf("poll after close = %+v, %v", got, err)
	}
	c.revisionMu.Lock()
	defer c.revisionMu.Unlock()
	if c.revisionConn != nil {
		t.Fatal("observer was retained or recreated after Close")
	}
	if inUse := c.db.Stats().InUse; inUse != 0 {
		t.Fatalf("Close left %d database connections in use", inUse)
	}
}

func TestDatabaseRevision_CloseDuringConnectionAcquisition(t *testing.T) {
	c := openCache(t)
	if err := c.PutBlob(context.Background(), fmt.Sprintf("%064x", 5), 10); err != nil {
		t.Fatal(err)
	}
	c.db.SetMaxOpenConns(1) // only the writer's existing pinned connection
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.DatabaseRevision(ctx)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for c.db.Stats().WaitCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("observer never waited for the occupied connection pool")
		}
		time.Sleep(time.Millisecond)
	}
	// Shutdown releases the writer's connection. The waiting observer may
	// acquire it, but must notice closure and release it instead of reopening
	// observation after Close began. Neither side may strand the other.
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Close was stranded behind observer acquisition")
	}
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("acquisition racing Close = %v, want ErrClosed", err)
	}
	c.revisionMu.Lock()
	defer c.revisionMu.Unlock()
	if c.revisionConn != nil {
		t.Fatal("observer was acquired after Close")
	}
}
