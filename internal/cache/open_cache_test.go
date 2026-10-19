package cache

import (
	"bytes"
	"log"
	"log/slog"
	"testing"
)

// captureDefaultLog points the process-wide slog default at buf for the
// rest of t and restores it in t.Cleanup — the log package too:
// slog.SetDefault also redirects log's output and clears its flags, and
// setting the stock default logger back undoes neither, so without the
// explicit restore every later slog.Default and log.Print line of the
// package run would land in buf.
func captureDefaultLog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev, w, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(w)
		log.SetFlags(flags)
	})
}

// Deferred minor A-18: the package's cache helper logs nothing (test output
// stays pristine), and checking that leaves the process loggers as they
// were (Task 12 review: the capture used to swallow every later line).
func TestOpenCache_LogsNothing(t *testing.T) {
	w, flags := log.Writer(), log.Flags()
	t.Run("open", func(t *testing.T) {
		var buf bytes.Buffer
		captureDefaultLog(t, &buf)
		openCache(t)
		if buf.Len() != 0 {
			t.Fatalf("openCache logged:\n%s", buf.String())
		}
	})
	if log.Writer() != w || log.Flags() != flags {
		t.Fatalf("log package left redirected (writer %T, flags %d; want %T, %d)", log.Writer(), log.Flags(), w, flags)
	}
}
