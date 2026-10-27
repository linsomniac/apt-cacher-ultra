package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sqliteClockRE matches every way production SQL could read SQLite's
// clock: a date and time function called (with or without arguments: date()
// and time() alone read it), CURRENT_DATE, CURRENT_TIME and
// CURRENT_TIMESTAMP, and the 'now' time value. SQLite's names are
// case-insensitive, so the match is too (the final review's R3 #3: the
// old case-sensitive substring list missed date(), time(), STRFTIME('%s',
// 'NOW'), current_timestamp and Unixepoch()). A name right after a dot or
// a word character is a Go method or identifier (t.Date(, ModTime(), not
// a SQL function; and the name must touch its parenthesis, as this code's
// SQL always writes it, so prose such as "retry time (2 min" is not a
// call.
var sqliteClockRE = regexp.MustCompile(
	`(?i)(?:^|[^.\w])(?:date|time|datetime|julianday|unixepoch|strftime|timediff)\(` +
		`|\bcurrent_(?:date|time|timestamp)\b` +
		`|'now'`)

// sqliteClockCalls returns the SQLite clock reads sqliteClockRE finds in
// line, leaving out JavaScript's Date constructor ("new Date(", in the
// admin page's script), which no SQL spells.
func sqliteClockCalls(line string) []string {
	var out []string
	for _, m := range sqliteClockRE.FindAllStringIndex(line, -1) {
		if strings.HasSuffix(line[:m[0]+1], "new ") {
			continue
		}
		out = append(out, strings.TrimSpace(line[m[0]:m[1]]))
	}
	return out
}

// Spec §6 / overview Global Constraints: production SQL never reads
// SQLite's clock, because the Go wall clock (internal/wallclock) is the
// only clock simulations can control.
func TestNoSQLiteClockFunctionsInProductionCode(t *testing.T) {
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if calls := sqliteClockCalls(line); len(calls) != 0 {
					t.Errorf("%s:%d reads SQLite's clock %q: %s", path, i+1, calls, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The guard's own pattern, against the final review's R3 #3 examples and
// the forms it must leave alone.
func TestSQLiteClockCalls(t *testing.T) {
	for _, s := range []string{
		"SELECT date(), time()", "SELECT STRFTIME('%s', 'NOW')", "SELECT current_timestamp, Unixepoch()",
		"datetime('now')", "SELECT julianday(x)", "timediff(a, b)", "WHERE at < unixepoch()",
		"DEFAULT CURRENT_TIMESTAMP", "CURRENT_DATE", "current_time", "(date(x))", "\tdate(x)", "Date(x)",
	} {
		if len(sqliteClockCalls(s)) == 0 {
			t.Errorf("%q not matched", s)
		}
	}
	for _, s := range []string{
		"time.Now()", "t.Date()", "info.ModTime()", "time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)",
		"releaseDate(x)", "currentTime := now", "updated_at = ?", "nowUnix()", "x.Time(",
		"at the record's retry time (2 min", "formatToParts(new Date());",
	} {
		if calls := sqliteClockCalls(s); len(calls) != 0 {
			t.Errorf("%q matched: %q", s, calls)
		}
	}
}
