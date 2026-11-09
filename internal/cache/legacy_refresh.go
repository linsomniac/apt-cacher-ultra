package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

var ErrMetadataChanged = errors.New("cache: suite metadata changed during fetch; retry")

// MetadataGeneration fences a miss against an intervening invalidation. The
// row identity also detects suite removal/recreation while a fetch is running.
type MetadataGeneration struct {
	RowID      int64
	Generation int64
}

const metadataGenerationSQL = `SELECT rowid, metadata_generation FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`

func (c *Cache) MetadataGeneration(ctx context.Context, key SuiteKey) (MetadataGeneration, error) {
	var g MetadataGeneration
	err := c.db.QueryRowContext(ctx, metadataGenerationSQL, key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&g.RowID, &g.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return g, nil
	}
	return g, err
}

func checkMetadataGenerationTx(ctx context.Context, tx *sql.Tx, key SuiteKey, want MetadataGeneration) error {
	var g MetadataGeneration
	err := tx.QueryRowContext(ctx, metadataGenerationSQL, key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&g.RowID, &g.Generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if g != want {
		return ErrMetadataChanged
	}
	return nil
}

// InvalidateLegacyMetadata is the adoption-disabled response to a changed
// anchor. Delete the mutable suite rows and fence pre-existing downloads in
// one transaction. Do not touch an adopted snapshot, another suite, packages,
// or immutable by-hash URLs. The next request fetches current upstream bytes.
func (c *Cache) InvalidateLegacyMetadata(ctx context.Context, key SuiteKey, anchorPath, oldHash string) error {
	if !aptpath.IsCanonicalSuitePath(key.SuitePath) || (anchorPath != key.SuitePath+"/InRelease" && anchorPath != key.SuitePath+"/Release") || !validBlobHash(oldHash) {
		return fmt.Errorf("invalidate metadata: invalid suite or anchor")
	}
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var snapshot *int64
		if err := tx.QueryRowContext(ctx, `SELECT current_snapshot_id FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&snapshot); err != nil {
			return err
		}
		if snapshot != nil {
			return fmt.Errorf("invalidate metadata: snapshot %d requires adoption to refresh", *snapshot)
		}
		var hash string
		if err := tx.QueryRowContext(ctx, `SELECT blob_hash FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`, key.CanonicalScheme, key.CanonicalHost, anchorPath).Scan(&hash); err != nil {
			return err
		}
		if hash != oldHash {
			return ErrMetadataChanged
		}
		// Keyset batches bound memory even for a suite with many cached
		// paths. All batches still commit atomically with the generation.
		after := key.SuitePath + "/"
		for {
			rows, err := tx.QueryContext(ctx, `SELECT path FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND is_metadata = 1 AND path > ? AND path < ?
 ORDER BY path LIMIT 256`, key.CanonicalScheme, key.CanonicalHost, after, key.SuitePath+"0")
			if err != nil {
				return err
			}
			var paths []string
			scanned := 0
			for rows.Next() {
				var path string
				if err := rows.Scan(&path); err != nil {
					_ = rows.Close()
					return err
				}
				after = path
				scanned++
				if aptpath.SuitePath(path) == key.SuitePath && !strings.Contains(path, "/by-hash/") {
					paths = append(paths, path)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			for _, path := range paths {
				if _, err := deleteCachedRowTx(ctx, tx, key.CanonicalScheme, key.CanonicalHost, path, nowUnix()); err != nil {
					return err
				}
			}
			if scanned < 256 {
				break
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE metadata_clock SET generation = generation + 1 WHERE id = 1`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE suite_freshness SET metadata_generation = (SELECT generation FROM metadata_clock WHERE id = 1)
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath); err != nil {
			return err
		}
		return tx.Commit()
	})
}
