// `apt-cacher-ultra packages list [substring]` — list cached .deb files.
//
// Reads url_path JOIN blob from cache.db; SQLite WAL mode lets this run
// safely alongside a live daemon. Output goes to stdout in one of three
// formats:
//
//   - columnar (default): padded table with NAME / SIZE / AGE / HOST(S)
//   - plain:              one filename per line, greppable
//   - json:               JSON array of objects with filename/size/hash/etc.
//
// Exit codes:
//   - 0: list printed
//   - 1: bad flag usage or unknown -format
//   - 2: cache open / query / write failure
//   - 3: config file unreadable

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// nowForPackagesCmd is the test seam for "what time is it" so AGE
// rendering is deterministic. Production uses time.Now.
var nowForPackagesCmd = time.Now

func runPackagesList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("packages list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/apt-cacher-ultra/config.toml", "path to TOML config file")
	format := fs.String("format", "columnar", "output format: columnar|plain|json")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: apt-cacher-ultra packages list [-config <path>] [-format columnar|plain|json] [substring]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() > 1 {
		_, _ = fmt.Fprintf(stderr, "packages list: too many arguments; want at most one substring, got %d\n", fs.NArg())
		return 1
	}
	substring := ""
	if fs.NArg() == 1 {
		substring = fs.Arg(0)
	}
	switch *format {
	case "columnar", "plain", "json":
	default:
		_, _ = fmt.Fprintf(stderr, "packages list: invalid -format %q (want columnar|plain|json)\n", *format)
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "packages list: load config %q: %v\n", *configPath, err)
		return 3
	}

	ctx := context.Background()
	c, err := openReadOnlyCache(ctx, cfg.Cache.Dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "packages list: open cache %q: %v\n", cfg.Cache.Dir, err)
		return 2
	}
	defer func() { _ = c.Close() }()

	debs, err := c.ListCachedDebs(ctx, substring)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "packages list: query: %v\n", err)
		return 2
	}

	switch *format {
	case "plain":
		return renderPackagesPlain(stdout, stderr, debs)
	case "json":
		return renderPackagesJSON(stdout, stderr, debs)
	default:
		return renderPackagesColumnar(stdout, stderr, debs, nowForPackagesCmd())
	}
}

// openReadOnlyCache is shared between `packages list` and `packages
// copy`. Both subcommands only issue read queries against the SQLite
// handle and read pool/<hash> blobs from disk; cache.Open's writer
// goroutine simply sits idle and is joined on Close.
//
// A discard logger is passed to silence the schema_migrating /
// schema_migrated Info lines that fire on first contact with an older
// DB — the subcommand prints its own stderr diagnostics and operators
// scripting against it should not see migration chatter spliced in.
func openReadOnlyCache(ctx context.Context, dir string) (*cache.Cache, error) {
	discard := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return cache.Open(ctx, dir, discard)
}

func renderPackagesPlain(stdout, stderr io.Writer, debs []cache.CachedDeb) int {
	for _, d := range debs {
		if _, err := fmt.Fprintln(stdout, d.Filename); err != nil {
			_, _ = fmt.Fprintf(stderr, "packages list: write stdout: %v\n", err)
			return 2
		}
	}
	return 0
}

// packageJSONRow is the JSON serialization shape — explicit so the
// CachedDeb struct can rename/extend internally without breaking
// scripts that consume this output.
type packageJSONRow struct {
	Filename  string   `json:"filename"`
	Size      int64    `json:"size"`
	BlobHash  string   `json:"blob_hash"`
	CreatedAt int64    `json:"created_at"`
	Hosts     []string `json:"hosts"`
}

func renderPackagesJSON(stdout, stderr io.Writer, debs []cache.CachedDeb) int {
	rows := make([]packageJSONRow, len(debs))
	for i, d := range debs {
		rows[i] = packageJSONRow{
			Filename:  d.Filename,
			Size:      d.Size,
			BlobHash:  d.BlobHash,
			CreatedAt: d.CreatedAt,
			Hosts:     d.Hosts,
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		_, _ = fmt.Fprintf(stderr, "packages list: encode JSON: %v\n", err)
		return 2
	}
	return 0
}

func renderPackagesColumnar(stdout, stderr io.Writer, debs []cache.CachedDeb, now time.Time) int {
	nameW := len("NAME")
	for _, d := range debs {
		if l := len(d.Filename); l > nameW {
			nameW = l
		}
	}
	// Clamp to keep terminal width sane on pathological filenames.
	if nameW > 80 {
		nameW = 80
	}
	w := newLatchedWriter(stdout)
	w.printf("%-*s  %8s  %8s  %s\n", nameW, "NAME", "SIZE", "AGE", "HOST(S)")
	for _, d := range debs {
		w.printf("%-*s  %8s  %8s  %s\n",
			nameW, d.Filename,
			humanSize(d.Size),
			humanAge(now.Sub(time.Unix(d.CreatedAt, 0))),
			strings.Join(d.Hosts, ", "),
		)
	}
	if w.err != nil {
		_, _ = fmt.Fprintf(stderr, "packages list: write stdout: %v\n", w.err)
		return 2
	}
	return 0
}

// latchedWriter swallows further writes after the first error so
// callers can fan out fmt.Fprintf calls without checking every one.
type latchedWriter struct {
	w   io.Writer
	err error
}

func newLatchedWriter(w io.Writer) *latchedWriter { return &latchedWriter{w: w} }

func (l *latchedWriter) printf(format string, a ...any) {
	if l.err != nil {
		return
	}
	_, l.err = fmt.Fprintf(l.w, format, a...)
}

// humanSize formats a byte count as a short SI-suffixed string:
//
//	999 → "999B", 1023 → "1023B", 1024 → "1.0K", 1.2e6 → "1.2M", etc.
//
// Units use 1024 multiples (KiB semantics) but with K/M/G/T labels —
// matching `ls -h` rather than `--si`.
func humanSize(n int64) string {
	if n < 0 {
		return fmt.Sprintf("%d", n)
	}
	const k = 1024
	if n < k {
		return fmt.Sprintf("%dB", n)
	}
	v := float64(n)
	units := []string{"K", "M", "G", "T", "P"}
	for _, u := range units {
		v /= k
		if v < k {
			return fmt.Sprintf("%.1f%s", v, u)
		}
	}
	return fmt.Sprintf("%.1fE", v/k)
}

// humanAge formats a duration as a compact age string suitable for a
// columnar log: "12s", "5m", "3h", "2d", "6w". Negative durations
// surface as "0s" rather than a misleading negative.
func humanAge(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	s := int64(d / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh", s/3600)
	case s < 7*86400:
		return fmt.Sprintf("%dd", s/86400)
	default:
		return fmt.Sprintf("%dw", s/(7*86400))
	}
}
