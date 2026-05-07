package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// ErrClosed is returned by Cache methods when the cache has been Closed.
var ErrClosed = errors.New("cache: closed")

// writeBufferSize bounds how many pending writes can queue before the
// caller blocks (or hits ctx.Done). Apt traffic is bursty but each write
// is small, so a few hundred slots is plenty without burning memory.
const writeBufferSize = 256

// Cache owns the SQLite handle, the on-disk pool, and a single writer
// goroutine that serializes all write transactions. Reads share the
// connection pool freely. SPEC §9.4.
type Cache struct {
	dir    string
	db     *sql.DB
	logger *slog.Logger

	writes  chan writeReq
	closeCh chan struct{}
	closed  atomic.Bool
	wg      sync.WaitGroup
}

// Open initializes (or attaches to) the cache rooted at dir. Creates
// pool/, tmp/, and staging/ subdirectories, opens cache.db with WAL and
// foreign keys enabled, and migrates the schema forward to
// CurrentSchemaVersion. Caller must Close.
//
// logger sinks the SPEC §10 schema-migration and blob-write structured
// logs. Pass nil to fall back to slog.Default().
func Open(ctx context.Context, dir string, logger *slog.Logger) (*Cache, error) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", sub, err)
		}
	}

	db, err := openDB(filepath.Join(dir, "cache.db"))
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, logger); err != nil {
		_ = db.Close()
		return nil, err
	}

	c := &Cache{
		dir:     dir,
		db:      db,
		logger:  logger,
		writes:  make(chan writeReq, writeBufferSize),
		closeCh: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.writer()
	return c, nil
}

// Dir is the on-disk root of the cache.
func (c *Cache) Dir() string { return c.dir }

// Ping checks the SQLite handle responds to a round-trip query
// within ctx. Used by the admin /healthz endpoint (SPEC5 §9.7.4
// check 3) to flag "DB writer hung" cases. Returns nil on a
// successful ping, ctx.Err() on deadline, or the underlying SQL
// error otherwise.
func (c *Cache) Ping(ctx context.Context) error {
	return c.db.PingContext(ctx)
}

// Close drains and rejects any further writes, joins the writer goroutine,
// and closes the SQLite handle. Safe to call multiple times.
func (c *Cache) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.closeCh)
	c.wg.Wait()
	return c.db.Close()
}

// openDB opens cache.db with the pragmas SPEC §4.3 mandates.
func openDB(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	// AIDEV-NOTE: busy_timeout is a backstop — the writer goroutine should
	// already serialize writes — but a 5s grace prevents transient races
	// (e.g. mtime queries on the WAL) from surfacing as SQLITE_BUSY.
	q.Add("_pragma", "busy_timeout(5000)")
	// AIDEV-NOTE: build the URI through net/url so a path containing
	// metacharacters (`?`, `#`, spaces) is percent-encoded. Concatenating
	// strings here would let those bytes hijack DSN parsing — e.g. a path
	// with `?` would be treated as the start of the query, swallowing
	// our pragmas.
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	dsn := u.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return db, nil
}

// writeOp is the unit of work the writer goroutine executes. It receives
// the submitter's context (so cancelling a request cancels its DB work)
// and a *sql.Conn that is dedicated to the writer for the call.
type writeOp func(ctx context.Context, conn *sql.Conn) error

type writeReq struct {
	ctx context.Context
	op  writeOp
	res chan error
}

// submitWrite enqueues op on the writer goroutine and waits for its
// result. Returns ErrClosed if the cache is closing.
//
// AIDEV-NOTE: Both selects must watch closeCh. The race is: a sender that
// passes the closed.Load() check can still race the writer's exit. If the
// channel has buffer space and Close fires concurrently, the request can
// land in a now-orphaned buffer after the writer's drain has completed.
// Watching closeCh in the result-wait select guarantees the caller
// unblocks (ErrClosed) instead of hanging forever on req.res.
func (c *Cache) submitWrite(ctx context.Context, op writeOp) error {
	if c.closed.Load() {
		return ErrClosed
	}
	req := writeReq{ctx: ctx, op: op, res: make(chan error, 1)}
	select {
	case c.writes <- req:
	case <-c.closeCh:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.res:
		return err
	case <-c.closeCh:
		return ErrClosed
	case <-ctx.Done():
		// The writer will still run the op and write to req.res; res is
		// buffered (cap 1) so no goroutine blocks.
		return ctx.Err()
	}
}

// writer is the singleton goroutine that owns the write connection. All
// writes serialize through it. SPEC §9.4.
func (c *Cache) writer() {
	defer c.wg.Done()

	// AIDEV-NOTE: we use a dedicated *sql.Conn so PRAGMAs and any
	// transaction state stay pinned to one underlying SQLite connection.
	// If the conn is broken between requests, surface that via per-op
	// error and let the caller decide whether to retry.
	conn, err := c.db.Conn(context.Background())
	if err != nil {
		// Drain pending requests with this error so submitters don't hang.
		c.drainWrites(fmt.Errorf("cache: writer connection: %w", err))
		return
	}
	defer conn.Close()

	for {
		select {
		case req := <-c.writes:
			req.res <- req.op(req.ctx, conn)
		case <-c.closeCh:
			// Drain anything queued before close.
			c.drainWrites(ErrClosed)
			return
		}
	}
}

func (c *Cache) drainWrites(err error) {
	for {
		select {
		case req := <-c.writes:
			req.res <- err
		default:
			return
		}
	}
}

// nowUnix is the test seam for "what time is it"; default is time.Now.
var nowUnix = func() int64 { return time.Now().Unix() }
