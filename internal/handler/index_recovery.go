package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/indexrecovery"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

func (h *Handler) runSnapshotMemberFetch(ctx context.Context, req *proxy.Request, mem *cache.SnapshotMember, snapshotID int64) sfResult {
	var maxBytes int64
	if indexrecovery.PlainIndex(mem.Path) {
		if b, err := h.cache.GetBlob(ctx, mem.BlobHash); err == nil {
			maxBytes = max(b.Size, 1)
		}
	}
	res := h.fetchSnapshotMember(ctx, req, mem, snapshotID, maxBytes)
	if res.err == nil || ctx.Err() != nil {
		return res
	}
	var se *fetch.StatusError
	missing := errors.As(res.err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusGone)
	if !missing && !errors.Is(res.err, ErrSnapshotMemberMismatch) && !errors.Is(res.err, fetch.ErrBodyTooLarge) {
		return res
	}
	if recovered, ok := h.reconstructSnapshotIndex(ctx, req, mem, snapshotID); ok {
		return recovered
	}
	return res
}

// reconstructSnapshotIndex uses only rows from the snapshot selected for this
// request, even if it is displaced while recovery runs. These SHA-256
// content addresses pin the exact bytes that passed Release SHA-256
// verification at adoption. A current upstream Release or a cached URL-path
// response must never supply replacement declarations.
func (h *Handler) reconstructSnapshotIndex(ctx context.Context, req *proxy.Request, requested *cache.SnapshotMember, snapshotID int64) (sfResult, bool) {
	if !indexrecovery.PlainIndex(requested.Path) && !strings.Contains("/"+requested.Path, "/by-hash/") {
		return sfResult{}, false
	}
	snapshot, err := h.cache.GetSuiteSnapshot(ctx, snapshotID)
	if err != nil || snapshot.CanonicalScheme != req.CanonicalScheme || snapshot.CanonicalHost != req.CanonicalHost || snapshot.SuitePath != req.SuitePath {
		return sfResult{}, false
	}
	rows, err := h.cache.ListSnapshotMembers(ctx, snapshotID)
	if err != nil {
		return sfResult{}, false
	}
	members := make(map[string]cache.SnapshotMember, len(rows))
	for _, m := range rows {
		members[m.Path] = m
	}
	u, err := url.Parse(req.UpstreamURL)
	if err != nil {
		return sfResult{}, false
	}
	for _, plain := range rows {
		if !indexrecovery.PlainIndex(plain.Path) || plain.BlobHash != requested.DeclaredSHA256 || plain.DeclaredSHA256 != requested.DeclaredSHA256 {
			continue
		}
		plainBlob, err := h.cache.GetBlob(ctx, plain.BlobHash)
		if err != nil || plainBlob.Size > indexrecovery.MaxBytes {
			continue
		}
		for _, suffix := range []string{".gz", ".xz"} {
			sibling, ok := members[plain.Path+suffix]
			if !ok || sibling.BlobHash != sibling.DeclaredSHA256 {
				continue
			}
			compressedBlob, err := h.cache.GetBlob(ctx, sibling.BlobHash)
			if err != nil || compressedBlob.Size > indexrecovery.MaxBytes {
				continue
			}
			exists, err := h.cache.BlobExists(sibling.BlobHash)
			if err != nil {
				return sfResult{err: err}, true
			}
			if !exists {
				siblingReq := *req
				siblingReq.Path = strings.TrimSuffix(req.SuitePath, "/") + "/" + sibling.Path
				siblingReq.UpstreamURL = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: siblingReq.Path}).String()
				res := h.fetchSnapshotMember(ctx, &siblingReq, &sibling, snapshotID, max(compressedBlob.Size, 1))
				if res.err != nil {
					continue
				}
			}
			p := indexrecovery.Member{Path: plain.Path, SHA256: plain.DeclaredSHA256, Size: plainBlob.Size}
			c := indexrecovery.Member{Path: sibling.Path, SHA256: sibling.DeclaredSHA256, Size: compressedBlob.Size}
			hash, err := indexrecovery.Reconstruct(ctx, h.cache, p, c, sibling.BlobHash)
			if err != nil {
				h.logger.Debug("index_reconstruction_failed", "canonical_host", req.CanonicalHost,
					"path", req.Path, "compressed_path", sibling.Path, "snapshot_id", snapshotID, "err", err)
				if errors.Is(err, indexrecovery.ErrStorage) || ctx.Err() != nil {
					return sfResult{err: err}, true
				}
				continue
			}
			h.logger.Info("index_reconstructed", "canonical_host", req.CanonicalHost,
				"path", req.Path, "compressed_path", sibling.Path, "snapshot_id", snapshotID)
			return sfResult{blobHash: hash, size: plainBlob.Size}, true
		}
	}
	return sfResult{}, false
}
