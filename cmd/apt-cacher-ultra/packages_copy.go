// `apt-cacher-ultra packages copy <filename> <destination>` — extract a
// cached .deb back out of the pool.
//
// filename must match a url_path.path basename exactly. destination is
// either an existing directory (in which case <destination>/<filename>
// is the target) or a filename (used verbatim). Existing destinations
// are overwritten silently, matching standard `cp` semantics.
//
// When the same .deb basename is cached across multiple hosts AND those
// rows resolve to the SAME blob hash (mirrors with identical content),
// the copy succeeds — there's only one pool blob to extract. When they
// resolve to DIFFERENT blob hashes (rare upstream divergence), the copy
// fails with an ambiguity error naming each candidate so the operator
// can pick the right one (e.g. by changing the cache key, not by this
// subcommand).
//
// Exit codes:
//   - 0: copy succeeded
//   - 1: bad usage / package not found
//   - 2: I/O error / blob missing from pool / ambiguous match
//   - 3: config file unreadable

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

func runPackagesCopy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("packages copy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/apt-cacher-ultra/config.toml", "path to TOML config file")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: apt-cacher-ultra packages copy [-config <path>] <filename> <destination>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 2 {
		_, _ = fmt.Fprintf(stderr, "packages copy: want <filename> <destination>, got %d positional argument(s)\n", fs.NArg())
		return 1
	}
	filename := fs.Arg(0)
	destination := fs.Arg(1)
	if filename == "" {
		_, _ = fmt.Fprintln(stderr, "packages copy: empty <filename>")
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "packages copy: load config %q: %v\n", *configPath, err)
		return 3
	}

	ctx := context.Background()
	c, err := openReadOnlyCache(ctx, cfg.Cache.Dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "packages copy: open cache %q: %v\n", cfg.Cache.Dir, err)
		return 2
	}
	defer func() { _ = c.Close() }()

	matches, err := c.LookupCachedDebByName(ctx, filename)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "packages copy: query: %v\n", err)
		return 2
	}
	switch len(matches) {
	case 0:
		_, _ = fmt.Fprintf(stderr, "packages copy: no cached .deb matches filename %q\n", filename)
		return 1
	case 1:
		// happy path — fall through
	default:
		// Same filename, different blob hashes — upstream divergence.
		_, _ = fmt.Fprintf(stderr, "packages copy: filename %q is ambiguous; %d distinct blobs cached:\n", filename, len(matches))
		for _, m := range matches {
			_, _ = fmt.Fprintf(stderr, "  blob=%s size=%d host(s)=%v\n", m.BlobHash, m.Size, m.Hosts)
		}
		return 2
	}
	match := matches[0]

	target := destination
	if st, err := os.Stat(destination); err == nil && st.IsDir() {
		target = filepath.Join(destination, filename)
	}

	srcPath := c.BlobPath(match.BlobHash)
	if err := copyBlobToFile(srcPath, target); err != nil {
		_, _ = fmt.Fprintf(stderr, "packages copy: %v\n", err)
		return 2
	}

	_, _ = fmt.Fprintf(stderr, "packages copy: wrote %s (%d bytes, blob %s)\n", target, match.Size, match.BlobHash)
	return 0
}

// copyBlobToFile streams the pool blob into target, truncating any
// existing destination. The destination is opened with the same mode
// bits (0644) the rest of the codebase uses for user-facing files;
// pool/<hash> blobs are 0640 inside cache_dir but operators typically
// expect 0644 once a file leaves the cache.
func copyBlobToFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("blob missing from pool (%s); cache may be mid-GC or inconsistent", src)
		}
		return fmt.Errorf("open blob %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open destination %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		// Best-effort: leave the partial file behind so the operator
		// can see what happened. A partial .deb is obviously bad to
		// use, but silently unlinking would mask the failure.
		return fmt.Errorf("copy to %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination %s: %w", dst, err)
	}
	return nil
}
