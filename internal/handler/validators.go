package handler

import (
	"io"
	"net/http"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// serveContent keeps the descriptive upstream date separate from validation.
// A date cannot distinguish different bodies published in the same second,
// rolled-back dates, or dates fabricated by older versions of this proxy.
// Zero modtime makes ServeContent ignore date preconditions and reject date
// If-Range, while its normal ETag checks and range handling remain available.
func (h *Handler) serveContent(w http.ResponseWriter, r *http.Request, req *proxy.Request, hash string, lastmod *string, body io.ReadSeeker) {
	var date string
	if lastmod != nil {
		date = cache.HTTPDate(*lastmod)
	} else {
		var err error
		date, err = h.cache.RepresentationLastModified(r.Context(), req.CanonicalScheme, req.CanonicalHost, req.Path, hash)
		if err != nil {
			h.logger.Warn("response_metadata_lookup_failed", "err", err, "path", req.Path)
		}
	}
	if date != "" {
		w.Header().Set("Last-Modified", date)
	}
	w.Header().Set("ETag", `"sha256-`+hash+`"`)
	http.ServeContent(w, r, req.Path, time.Time{}, body)
}

// URL rows describe a completed fetch even when the origin supplied no date.
// Avoid a second database read on every such hit. Prefetch publication copies
// its representation date into the URL row in the adoption transaction.
func urlLastModified(row *cache.URLPath) *string {
	if row.UpstreamLastMod != nil {
		return row.UpstreamLastMod
	}
	return new(string)
}
