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
// a SQL function.
//
// AIDEV-NOTE: SQLite also takes white space, line breaks included, between
// a function's name and its parenthesis ("date ()", or "strftime" at the
// end of one line of a raw string and "('%s')" on the next), so the guard
// scans each file whole. Prose puts a space there too ("the record's retry
// time (2 min", "CPU time (user+system)"), so with white space only the
// forms that read the clock without 'now' count: no argument, or a string
// first (strftime ('%s') implies 'now'); a column argument ("stored date
// (NULL)") is prose there. The final review's R3 re-review.
var sqliteClockRE = regexp.MustCompile(
	`(?i)(?:^|[^.\w])(?:date|time|datetime|julianday|unixepoch|strftime|timediff)(?:\(|\s+\(\s*(?:\)|'))` +
		`|\bcurrent_(?:date|time|timestamp)\b` +
		`|'now'`)

// clockCall is one SQLite clock read found in a source text: the line it
// starts on (1-based) and the text matched, its white space collapsed.
type clockCall struct {
	line int
	text string
}

// sqliteClockCalls returns the SQLite clock reads sqliteClockRE finds in
// src (a whole file, or a line), leaving out JavaScript's Date constructor
// ("new Date(", in the admin page's script), which no SQL spells.
func sqliteClockCalls(src string) []clockCall {
	var out []clockCall
	for _, m := range sqliteClockRE.FindAllStringIndex(src, -1) {
		if strings.HasSuffix(src[:m[0]+1], "new ") {
			continue
		}
		out = append(out, clockCall{
			line: strings.Count(src[:m[0]+1], "\n") + 1,
			text: strings.Join(strings.Fields(src[m[0]:m[1]]), " "),
		})
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
			lines := strings.Split(string(b), "\n")
			for _, c := range sqliteClockCalls(string(b)) {
				t.Errorf("%s:%d reads SQLite's clock %q: %s", path, c.line, c.text, strings.TrimSpace(lines[c.line-1]))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The guard's own pattern, against the final review's R3 #3 examples, its
// re-review's white space, and the forms it must leave alone.
func TestSQLiteClockCalls(t *testing.T) {
	for _, s := range []string{
		"SELECT date(), time()", "SELECT STRFTIME('%s', 'NOW')", "SELECT current_timestamp, Unixepoch()",
		"datetime('now')", "SELECT julianday(x)", "timediff(a, b)", "WHERE at < unixepoch()",
		"DEFAULT CURRENT_TIMESTAMP", "CURRENT_DATE", "current_time", "(date(x))", "\tdate(x)", "Date(x)",
		"SELECT date ()", "WHERE at < unixepoch ( )", "SELECT strftime ('%s')", "SELECT strftime\n\t\t('%s')",
		"SELECT DATETIME\t()", "julianday (x, 'now')",
	} {
		if len(sqliteClockCalls(s)) == 0 {
			t.Errorf("%q not matched", s)
		}
	}
	for _, s := range []string{
		"time.Now()", "t.Date()", "info.ModTime()", "time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)",
		"releaseDate(x)", "currentTime := now", "updated_at = ?", "nowUnix()", "x.Time(",
		"at the record's retry time (2 min", "formatToParts(new Date());", "new Date ()",
		`"Total CPU time (user+system) consumed by the process."`, "// no stored date (NULL): no rule",
		"// the retry time\n// ('2 min')", "t.Date ()",
	} {
		if calls := sqliteClockCalls(s); len(calls) != 0 {
			t.Errorf("%q matched: %v", s, calls)
		}
	}
	if c := sqliteClockCalls("x := 1\nconst q = `SELECT a\n\tFROM t WHERE b < strftime\n\t\t('%s')`"); len(c) != 1 || c[0].line != 3 || c[0].text != "strftime ('" {
		t.Errorf("a call split over two lines: %v, want one on line 3, \"strftime ('\"", c)
	}
}
