package cache

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
)

// DatabaseRevision is an opaque, comparable token for the committed database
// state observed by DatabaseRevision. Equal tokens mean no intervening commit
// was observed. Tokens are process-local and must not be persisted or ordered.
// A replaced observer always produces a different token, even if SQLite starts
// its connection-local data_version at the same number.
type DatabaseRevision struct {
	epoch   uint64
	version int64
}

// Use a process-wide epoch so tokens from different Cache instances cannot
// accidentally compare equal. Epoch zero is reserved for the invalid token.
var databaseRevisionEpoch atomic.Uint64

// DatabaseRevision observes commits from every other SQLite connection,
// including the production writer and external tools. The dedicated observer
// performs no writes itself: PRAGMA data_version does not report a connection's
// own commits, and its values cannot be compared across different connections.
// See https://www.sqlite.org/pragma.html#pragma_data_version.
//
// The connection is acquired lazily and kept out of the general read pool until
// Close or a query error. Each poll finishes its statement before returning;
// no transaction or read snapshot is retained between polls. Callers caching
// query results should compare revisions before and after recomputation and
// reuse results only when both polls succeed and their tokens match.
func (c *Cache) DatabaseRevision(ctx context.Context) (DatabaseRevision, error) {
	if c.closed.Load() {
		return DatabaseRevision{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return DatabaseRevision{}, err
	}
	c.revisionMu.Lock()
	defer c.revisionMu.Unlock()
	// Close or cancellation may have occurred while waiting for another poll.
	if c.closed.Load() {
		return DatabaseRevision{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return DatabaseRevision{}, err
	}
	if c.revisionConn == nil {
		conn, err := c.db.Conn(ctx)
		if err != nil {
			return DatabaseRevision{}, fmt.Errorf("DatabaseRevision: connect: %w", err)
		}
		// Acquiring a pooled connection can block. Never retain a new observer
		// after shutdown starts, even if this call began before Close.
		if c.closed.Load() {
			_ = conn.Close()
			return DatabaseRevision{}, ErrClosed
		}
		c.revisionConn = conn
		c.revisionEpoch = databaseRevisionEpoch.Add(1)
	}

	var version int64
	if err := c.revisionConn.QueryRowContext(ctx, "PRAGMA main.data_version").Scan(&version); err != nil {
		// Do not return an ambiguous token after an interrupted/failed read.
		// ErrBadConn removes the underlying connection from the pool; the
		// replacement's fresh epoch prevents comparison with the old observer.
		_ = c.revisionConn.Raw(func(any) error { return driver.ErrBadConn })
		_ = c.revisionConn.Close()
		c.revisionConn = nil
		return DatabaseRevision{}, fmt.Errorf("DatabaseRevision: data_version: %w", err)
	}
	if c.closed.Load() {
		return DatabaseRevision{}, ErrClosed
	}
	return DatabaseRevision{epoch: c.revisionEpoch, version: version}, nil
}
