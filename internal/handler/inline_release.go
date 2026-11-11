package handler

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// Above the freshness checker's 4 MiB adoption limit, but bounded even if a
// local pool file is replaced with a much larger file after adoption.
const maxInlineReleaseBytes = 8 << 20

// tryInlineRelease serves compatibility representations of the selected
// snapshot's InRelease (issue #30). The installer asks for Release directly;
// some clients also need its detached signatures. Inline snapshots have
// neither member, including snapshots made before this fix.
//
// Derive both from the same snapshot's anchor, never from upstream sidecars
// or url_path: either could belong to a newer or older repository state.
// Nothing is persisted, so this also repairs existing snapshots offline,
// without a migration, extra blob references, or changes to adoption form.
func (h *Handler) tryInlineRelease(w http.ResponseWriter, r *http.Request, req *proxy.Request,
	snapshotID int64, memberPath string, start time.Time,
) (served bool, status int, n int64) {
	if memberPath != "Release" && memberPath != "Release.gpg" {
		return false, 0, 0
	}
	anchor, err := h.cache.GetSnapshotMember(r.Context(), snapshotID, "InRelease")
	if errors.Is(err, cache.ErrNotFound) {
		return false, 0, 0 // detached/unsigned snapshots retain their normal rules
	}
	fail := func(err error) (bool, int, int64) {
		h.logger.Warn("snapshot_release_compat_failed", "err", err,
			"canonical_host", req.CanonicalHost, "path", req.Path, "snapshot_id", snapshotID)
		return h.writeFailClosed(w, "snapshot Release conversion failed"), http.StatusBadGateway, 0
	}
	if err != nil {
		return fail(err)
	}
	if anchor.BlobHash != anchor.DeclaredSHA256 {
		return fail(errors.New("inconsistent InRelease member hashes"))
	}
	body, err := readInlineRelease(h.cache.BlobPath(anchor.BlobHash))
	cacheState := cacheHit
	if errors.Is(err, os.ErrNotExist) {
		// Use normal hash-checked snapshot recovery, but fetch InRelease,
		// never the requested (derived) Release or Release.gpg. Coalesce
		// with direct InRelease recovery under the same snapshot hash.
		anchorReq := *req
		anchorReq.Path = req.SuitePath + "/InRelease"
		u, parseErr := url.Parse(req.UpstreamURL)
		if parseErr != nil {
			return fail(parseErr)
		}
		u.Path, u.RawPath = anchorReq.Path, ""
		anchorReq.UpstreamURL = u.String()
		key := "snapshot:" + req.CanonicalScheme + "|" + req.CanonicalHost + "|" + anchorReq.Path + "|" + anchor.DeclaredSHA256
		res, _, _ := h.sf.Do(key, func() sfResult {
			return h.runSnapshotMemberFetch(h.lifecycleCtx, &anchorReq, anchor, snapshotID)
		})
		if res.err != nil {
			h.respondRecoveryError(w, r, req, res, start)
			return true, http.StatusBadGateway, 0
		}
		body, err = readInlineRelease(h.cache.BlobPath(anchor.BlobHash))
		cacheState = cacheMiss
	}
	if err != nil {
		return fail(err)
	}
	// Derived plaintext must never hide corruption of the signed source.
	// Hash the bytes read, rather than trusting the pool filename or a stat.
	if fmt.Sprintf("%x", sha256.Sum256(body)) != anchor.DeclaredSHA256 {
		return fail(errors.New("InRelease blob hash mismatch"))
	}
	release, signature, err := apttext.DetachedFromInline(body)
	if err != nil {
		return fail(err)
	}
	derived := release
	if memberPath == "Release.gpg" {
		derived = signature
	}
	w.Header().Set(hdrXCache, cacheState)
	w.Header().Set(hdrXCacheSnapshot, strconv.FormatInt(snapshotID, 10))
	// A derived representation has no upstream Last-Modified. Strong
	// content ETags handle changes, including flips within the same second.
	w.Header().Set("ETag", fmt.Sprintf(`"sha256-%x"`, sha256.Sum256(derived)))
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, req.Path, time.Time{}, bytes.NewReader(derived))
	return true, cw.statusCode(), cw.bytes
}

func readInlineRelease(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxInlineReleaseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInlineReleaseBytes {
		return nil, errors.New("InRelease blob exceeds compatibility read limit")
	}
	return b, nil
}
