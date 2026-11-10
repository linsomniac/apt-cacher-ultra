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
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
	_ "modernc.org/sqlite"
)

// ErrClosed is returned by Cache methods when the cache has been Closed.
var ErrClosed = errors.New("cache: closed")

// ErrWriteNotStarted is returned by a write whose start deadline
// (WithWriteStartDeadline) passed before the writer goroutine started it.
// The op never ran and never will: nothing it would have written was
// written. It means "deadline reached", not a failure.
var ErrWriteNotStarted = errors.New("cache: write not started before its start deadline")

// writeStartDeadlineKey is the context key of a write's start deadline.
type writeStartDeadlineKey struct{}

// WithWriteStartDeadline returns a ctx whose writes must start by t: a
// write still queued behind the writer goroutine at t gives up with
// ErrWriteNotStarted, and the writer then skips it. It gates only
// starting. A write the writer has started runs to completion under its
// ctx exactly as without a start deadline; t never cancels it.
func WithWriteStartDeadline(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, writeStartDeadlineKey{}, t)
}

// writeStartDeadline returns ctx's start deadline, if it has one.
func writeStartDeadline(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(writeStartDeadlineKey{}).(time.Time)
	return t, ok
}

// writeBufferSize bounds how many pending writes can queue before the
// caller blocks (or hits ctx.Done). Apt traffic is bursty but each write
// is small, so a few hundred slots is plenty without burning memory.
const writeBufferSize = 256

// minIdleConns is the floor of maxIdleConns.
const minIdleConns = 16

// maxIdleConns is how many spare read connections the pool keeps: 2 per
// GOMAXPROCS, at least minIdleConns. openDB reads it once; a GOMAXPROCS
// that changes later (a container's CPU quota) does not move it, and on
// small hosts the floor decides anyway.
func maxIdleConns() int {
	return max(minIdleConns, 2*runtime.GOMAXPROCS(0))
}

// Cache owns the SQLite handle, the on-disk pool, and a single writer
// goroutine that serializes all write transactions. Reads share the
// connection pool freely. SPEC §9.4.
type Cache struct {
	dir    string
	dbDir  string
	db     *sql.DB
	logger *slog.Logger

	writes  chan writeReq
	closeCh chan struct{}
	closed  atomic.Bool
	wg      sync.WaitGroup

	// SQLite data_version values are comparable only on one connection.
	// This lazy observer stays pinned between polls, without holding a
	// transaction. revisionMu serializes polling, recovery, and shutdown.
	revisionMu    sync.Mutex
	revisionConn  *sql.Conn
	revisionEpoch uint64

	// touches coalesces TouchSuite writes (spec C3).
	touches suiteTouches

	// flatRows is the "flat rows may exist" flag (ruling PG-31): false
	// while package_hash holds no row with a non-empty flat_filename, so
	// DeclaredHashesForPath skips the suffix fallback, which only such a
	// row can answer. Open primes it (flatRowsExistSQL); noteFlatRows sets
	// it; nothing clears it at run time.
	//
	// AIDEV-NOTE: false is never stale. Every writer that inserts a flat
	// row (CommitAdoption, InsertReconciledMembers) calls noteFlatRows
	// before it submits its transaction, so the flag is true before such a
	// row can commit, and it is never cleared while the process runs (a
	// stale true — the suite reaped, its rows gone — costs only the
	// fallback's query, until the next Open primes it again). A stale false
	// would skip the fallback for a flat .deb: strict mode would refuse it
	// (fail closed) and a non-strict serve would go unhashed — the
	// vouching lost silently. A new package_hash writer must call
	// noteFlatRows too.
	flatRows atomic.Bool

	// heldHook, when set (tests only), runs on the creating goroutine right
	// after creationAttempt holds an eviction candidate, before the writer
	// operation: a panic there stands in for a bug between the hold and the
	// settle (TestCreateSuite_APanicReleasesTheHeldCandidate).
	heldHook func(SuiteKey)

	// committedHook, when set (tests only), runs on the writer goroutine
	// right after the transaction of a creation attempt, of ReapIdleSuite
	// or of ForgetSuite commits, before its result is handed back: it lets
	// a test end the caller's context there, so a submit fails although its
	// eviction or delete committed
	// (TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges, deferred
	// minor D-20; TestReapAndForget_AFailedSubmitWhoseDeleteCommittedIsGone,
	// design review M-2).
	committedHook func()
}

// Open initializes (or attaches to) the cache rooted at dir. Creates
// pool/, tmp/, and staging/ subdirectories, opens cache.db with WAL and
// foreign keys enabled, and migrates the schema forward to
// CurrentSchemaVersion. Caller must Close.
//
// logger sinks the SPEC §10 schema-migration and blob-write structured
// logs. Pass nil to fall back to slog.Default().
func Open(ctx context.Context, dir string, logger *slog.Logger) (*Cache, error) {
	return OpenWithDBDir(ctx, dir, "", logger)
}

// OpenWithDBDir is Open with a separate directory for cache.db and its WAL/SHM
// files. An empty dbDir uses dir. All blob and scratch files remain under dir
// so their hard links and renames stay on the same filesystem. Caller must Close.
func OpenWithDBDir(ctx context.Context, dir, dbDir string, logger *slog.Logger) (*Cache, error) {
	if dbDir == "" {
		dbDir = dir
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, sub := range []string{"pool", "tmp", "staging"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", sub, err)
		}
	}

	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		return nil, fmt.Errorf("create database directory %q: %w", dbDir, err)
	}
	db, err := openDB(filepath.Join(dbDir, "cache.db"))
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, logger); err != nil {
		_ = db.Close()
		return nil, err
	}

	c := &Cache{
		dir:     dir,
		dbDir:   dbDir,
		db:      db,
		logger:  logger,
		writes:  make(chan writeReq, writeBufferSize),
		closeCh: make(chan struct{}),
	}
	var flat bool
	if err := db.QueryRowContext(ctx, flatRowsExistSQL).Scan(&flat); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read whether flat package_hash rows exist: %w", err)
	}
	c.flatRows.Store(flat)
	c.wg.Add(1)
	go c.writer()
	return c, nil
}

// Dir is the on-disk root of the cache.
func (c *Cache) Dir() string { return c.dir }

// DBDir is the directory containing cache.db and its WAL/SHM files.
func (c *Cache) DBDir() string { return c.dbDir }

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
	c.revisionMu.Lock()
	var revisionErr error
	if c.revisionConn != nil {
		revisionErr = c.revisionConn.Close()
		c.revisionConn = nil
	}
	c.revisionMu.Unlock()
	return errors.Join(revisionErr, c.db.Close())
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
	// AIDEV-NOTE: security review E-5/I-9. database/sql keeps only 2 idle
	// connections by default, so every concurrent read beyond the second
	// opened a SQLite connection (running the pragmas above) and closed it
	// again: in the review's hit flood that churn starved the writer (77% of
	// url_path writes hit their timeout) and let the WAL grow to 183 MB for
	// a 3.5 MB database. The idle cap only keeps connections a burst already
	// opened, so it does not raise the peak connection count, but it keeps
	// their memory after the burst: up to about 4 MB each with a warm page
	// cache (32 measured at +122 MB RSS), so at most about cap x 4 MB (64 MB
	// at the floor). It is less in practice, since a connection drops its
	// cache at its next read after a write. There is deliberately no
	// SetConnMaxIdleTime or SetConnMaxLifetime (the maintainer's decision,
	// 2026-10-24): a 5-minute idle timeout made every quiet gap reopen each
	// connection, doubling the 90-day simulation under -race, to free memory
	// the cap already bounds. Do not add a tight SetMaxOpenConns: it trades
	// the churn for readers waiting on a connection. The writer's and the
	// revision observer's pinned connections are in use, not idle, so they
	// do not count against the cap.
	db.SetMaxIdleConns(maxIdleConns())
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

// writeReq states, for a request with a start deadline. The writer moves
// pending to running just before it calls the op; the submitter moves
// pending to abandoned when it gives up. Exactly one of the two CASes
// wins, so the op runs at most once and never after its submitter gave up.
const (
	writePending int32 = iota
	writeRunning
	writeAbandoned
)

type writeReq struct {
	ctx context.Context
	op  writeOp
	res chan error
	// state is nil for a request with no start deadline (no allocation,
	// and the writer runs it unconditionally, as before issue #14). It is
	// a pointer because writeReq travels through c.writes by value and
	// both sides must CAS the same word.
	state *atomic.Int32
}

// start claims req for running: true unless its submitter abandoned it.
func (req *writeReq) start() bool {
	return req.state == nil || req.state.CompareAndSwap(writePending, writeRunning)
}

// abandon marks req abandoned if the writer has not started it: true
// means the op will never run.
func (req *writeReq) abandon() bool {
	return req.state != nil && req.state.CompareAndSwap(writePending, writeAbandoned)
}

// submitWrite enqueues op on the writer goroutine and waits for its
// result. Returns ErrClosed if the cache is closing, and
// ErrWriteNotStarted if ctx carries a start deadline
// (WithWriteStartDeadline) that passes before the writer starts op.
//
// AIDEV-NOTE: Both selects must watch closeCh. The race is: a sender that
// passes the closed.Load() check can still race the writer's exit. If the
// channel has buffer space and Close fires concurrently, the request can
// land in a now-orphaned buffer after the writer's drain has completed.
// Watching closeCh in the result-wait select guarantees the caller
// unblocks (ErrClosed) instead of hanging forever on req.res.
//
// AIDEV-NOTE: the start deadline (issue #14 Tier 1a, G2-RR1-M1, added
// 2026-11-09) gates only *starting* an op; it never cancels or
// interrupts one the writer has started: op keeps the submitter's ctx,
// and the deadline is not on that ctx. Why: a GC batch queued behind
// adoption commits must give up at its pass deadline instead of running
// late, but a GC transaction interrupted midway is wasted work, and a
// large one would never complete. Invariant: the op runs at most once,
// and the submitter's answer is the truth. The timer and the writer's
// dequeue race, so both sides CAS req.state from pending: the writer to
// running (start), the submitter to abandoned (abandon). If abandon wins,
// the op never runs and ErrWriteNotStarted is truthful; if it loses, the
// op is running and the submitter keeps waiting for its real result.
// The writer answers a skipped request with ErrWriteNotStarted on res,
// which never blocks it: res is buffered (cap 1) and only the writer (or
// drainWrites, never both) sends on it, at most once, since a request is
// received from c.writes at most once; nobody need read it after the
// submitter has gone.
// A ctx cancelled while waiting (shutdown) returns ctx.Err() as before,
// never ErrWriteNotStarted, so a shutdown still reads as a shutdown,
// even when the start deadline is due at the same time: Go's select picks
// at random among ready cases, so the entry check and both deadline cases
// check ctx.Err() first (notStartedErr; the final review's M-1,
// 2026-11-09: a GC pass must never log a shutdown as its deadline). The
// op still never runs there: those paths return only for a request never
// enqueued or one whose abandon won the CAS. For a request with a start
// deadline the cancel also abandons a still-pending request, so the
// writer skips an op whose submitter has gone instead of starting it on a
// cancelled ctx. That can only stop an op that has not started, so the
// "never interrupt a started op" rule holds, and a running op still sees
// the cancel through its own ctx, as before. A
// request with no start deadline is untouched: nil state, no timer (a nil
// channel in the selects), and the writer still calls its op after a
// cancel, exactly as before issue #14.
func (c *Cache) submitWrite(ctx context.Context, op writeOp) error {
	if c.closed.Load() {
		return ErrClosed
	}
	req := writeReq{ctx: ctx, op: op, res: make(chan error, 1)}
	var deadlineC <-chan time.Time // nil (never ready) without a start deadline
	if deadline, ok := writeStartDeadline(ctx); ok {
		wait := time.Until(deadline)
		if wait <= 0 {
			// Not enqueued. A shutdown reads as a shutdown (ctx.Err()).
			return notStartedErr(ctx)
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		deadlineC = timer.C
		req.state = new(atomic.Int32) // writePending
	}
	select {
	case c.writes <- req:
	case <-c.closeCh:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-deadlineC:
		// Not enqueued, so the writer can never see it. A ctx cancelled
		// too (both ready, the select chose the deadline) is ctx.Err().
		return notStartedErr(ctx)
	}
	for {
		select {
		case err := <-req.res:
			return err
		case <-c.closeCh:
			return ErrClosed
		case <-ctx.Done():
			// The writer will still dequeue the request and write to
			// req.res; res is buffered (cap 1) so no goroutine blocks.
			// With a start deadline, a still-pending op is skipped.
			req.abandon()
			return ctx.Err()
		case <-deadlineC:
			if req.abandon() {
				// Never runs. A ctx cancelled too (both ready, the
				// select chose the deadline) is ctx.Err().
				return notStartedErr(ctx)
			}
			// The writer started the op first: it runs to completion.
			// Stop selecting on the spent timer and wait for its result.
			deadlineC = nil
		}
	}
}

// notStartedErr is submitWrite's answer for a request it gave up before the
// writer started it: ctx.Err() if ctx has ended (a shutdown reads as a
// shutdown, even when the start deadline was due at the same time), else
// ErrWriteNotStarted. Either way the op never runs.
func notStartedErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrWriteNotStarted
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
	defer func() { _ = conn.Close() }()

	// AIDEV-NOTE: no recover in this goroutine, by the maintainer's
	// decision of 2026-10-26 (docs/open-decisions.md, *Decided*: "The
	// cache's writer stays fail-fast on a panic"). A panic inside a write
	// op ends the process and systemd restarts it: a panic mid-transaction
	// leaves this one write connection in an unknown state, and the
	// restart rolls the transaction back. The rejected alternative —
	// recover, roll back, fail that one write and reopen the connection —
	// would avoid the crash loop of a panic that persisted data triggers
	// at every start; change that entry before adding one here.
	for {
		select {
		case req := <-c.writes:
			// A request whose submitter gave up at its start deadline
			// (or was cancelled while it was still pending) is skipped,
			// never started. See submitWrite's AIDEV-NOTE.
			if req.start() {
				req.res <- req.op(req.ctx, conn)
			} else {
				req.res <- ErrWriteNotStarted
			}
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

// nowUnix is the test seam for "what time is it". Production reads the
// shared wall clock (internal/wallclock) so simulations can step it.
var nowUnix = func() int64 { return wallclock.Unix() }
