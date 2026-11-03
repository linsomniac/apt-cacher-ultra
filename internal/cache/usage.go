package cache

import (
	"context"
	"database/sql"
	"errors"

	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func (c *Cache) LoadUsage(ctx context.Context) (usage.Totals, error) {
	var t usage.Totals
	err := c.db.QueryRowContext(ctx, `SELECT since_unixtime, fetched_bytes,
served_bytes, cache_hit_bytes, requests, cache_hit_requests, fetch_nanoseconds,
serve_nanoseconds FROM usage_totals WHERE id = 1`).Scan(&t.SinceUnix,
		&t.FetchedBytes, &t.ServedBytes, &t.CacheHitBytes, &t.Requests,
		&t.CacheHitRequests, &t.FetchNanoseconds, &t.ServeNanoseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return usage.Totals{}, nil
	}
	return t, err
}

// SaveUsage replaces the single aggregate atomically. Retrying an absolute
// snapshot is idempotent even after an ambiguous commit acknowledgement.
func (c *Cache) SaveUsage(ctx context.Context, t usage.Totals) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO usage_totals
(id, since_unixtime, fetched_bytes, served_bytes, cache_hit_bytes, requests,
 cache_hit_requests, fetch_nanoseconds, serve_nanoseconds)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET since_unixtime=excluded.since_unixtime,
fetched_bytes=excluded.fetched_bytes, served_bytes=excluded.served_bytes,
cache_hit_bytes=excluded.cache_hit_bytes, requests=excluded.requests,
cache_hit_requests=excluded.cache_hit_requests,
fetch_nanoseconds=excluded.fetch_nanoseconds, serve_nanoseconds=excluded.serve_nanoseconds`,
			t.SinceUnix, t.FetchedBytes, t.ServedBytes, t.CacheHitBytes, t.Requests,
			t.CacheHitRequests, t.FetchNanoseconds, t.ServeNanoseconds)
		return err
	})
}
