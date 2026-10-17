package freshness

import (
	"testing"
	"time"
)

// rfcExample is RFC 7231's example instant, Sun, 06 Nov 1994 08:49:37 GMT.
const rfcExample = int64(784111777)

func utc(y int, mo time.Month, d, h, mi, s int) int64 {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC).Unix()
}

// The cases pin apt's RFC1123StrToTime (apt-pkg/contrib/strutl.cc),
// including its leniencies, so the cache refuses exactly what apt refuses.
func TestParseAptDate(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"Sun, 06 Nov 1994 08:49:37 GMT", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 UTC", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 Z", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 +0000", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 -0000", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37 0", rfcExample, true},
		{"Sun Nov  6 08:49:37 1994", rfcExample, true},       // asctime
		{"Sunday, 06-Nov-94 08:49:37 GMT", rfcExample, true}, // RFC 850
		{"sun, 06 nov 1994 08:49:37 GMT", rfcExample, true},  // weekday and month case-insensitive
		{"Mon, 06 Nov 1994 08:49:37 GMT", rfcExample, true},  // weekday never checked against the date
		{"Sun, 6 Nov 1994 8:49:37 GMT", rfcExample, true},
		{"Sun, 06 Nov 1994 08:49:37.5 GMT", rfcExample, true}, // strptime ignores text after %S
		{"Sat, 17 Oct 2026 10:00:00 UTC", utc(2026, time.October, 17, 10, 0, 0), true},
		{"Sun, 06 Xyz 1994 08:49:37 GMT", utc(1994, time.January, 6, 8, 49, 37), true}, // unknown month: January
		{"Sun, 31 Feb 2026 00:00:00 UTC", utc(2026, time.March, 3, 0, 0, 0), true},     // timegm rolls over
		{"Sunday, 06-Nov-26 08:49:37 GMT", utc(1926, time.November, 6, 8, 49, 37), true},
		// strptime's get_number (via glibc) consumes a second digit of a
		// field only when doing so could not push it past the field's
		// max; for %S (0..61) a first digit of 7, 8 or 9 ends the field
		// on one digit, so the second digit is trailing text (ignored).
		{"Sun, 06 Nov 1994 08:49:70 GMT", utc(1994, time.November, 6, 8, 49, 7), true},
		{"Sun, 06 Nov 1994 08:49:99 GMT", utc(1994, time.November, 6, 8, 49, 9), true},
		{"Sun, 06 Nov 1994 08:49:62 GMT", 0, false},
		// :60 and :61 ("leap seconds") are in range for %S and roll over
		// via time.Date exactly as timegm would.
		{"Sun, 06 Nov 1994 08:49:60 GMT", utc(1994, time.November, 6, 8, 50, 0), true},
		{"Sun, 06 Nov 1994 08:49:61 GMT", utc(1994, time.November, 6, 8, 50, 1), true},
		{"Sun, 06 Nov 1994 08:49:37 +0100", 0, false}, // only UTC
		{"Sun, 06 Nov 1994 08:49:37 +00:00", 0, false},
		{"Sun, 06 Nov 1994 08:49:37 gmt", 0, false}, // zone is case-sensitive
		{"Sun, 06 Nov 1994 08:49:37 CET", 0, false},
		{"Sun, 06 Nov 1994 08:49:37 GMT extra", 0, false},
		{"Sun, 06 Nov 1994 08:49:37", 0, false},
		{"Sun, 06 Nov 1994 24:00:00 GMT", 0, false},
		{"Sun, 06 Nov 1994 08:60:00 GMT", 0, false},
		{"Sun, 00 Nov 1994 08:49:37 GMT", 0, false},
		{"Sun, 32 Nov 1994 08:49:37 GMT", 0, false},
		{"Sun 06 Nov 1994 08:49:37 GMT", 0, false}, // three-letter weekday means asctime order
		{"Sunday 06-Nov-94 08:49:37 GMT", 0, false},
		{"Xyz, 06 Nov 1994 08:49:37 GMT", 0, false},
		{"Su, 06 Nov 1994 08:49:37 GMT", 0, false},
		{"Sunday, 06-Nov 08:49:37 GMT", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseAptDate(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseAptDate(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReleaseDate(t *testing.T) {
	cases := map[string]int64{
		"Origin: X\nDate: Sun, 06 Nov 1994 08:49:37 GMT\nSHA256:\n x 1 a\n": rfcExample,
		"Origin: X\r\ndate:   Sun, 06 Nov 1994 08:49:37 UTC  \r\n":          rfcExample, // key case, padding, CRLF
		"Origin: X\nSHA256:\n x 1 a\n":                                      0,          // missing
		"Date: yesterday\n":                                                 0,          // unparseable
		"Origin: X\n Date: Sun, 06 Nov 1994 08:49:37 GMT\n":                 0,          // continuation line, not a field
	}
	for text, want := range cases {
		if got := releaseDate([]byte(text)); got != want {
			t.Errorf("releaseDate(%q) = %d, want %d", text, got, want)
		}
	}
	if maxReleaseDateSkew != 10*time.Second {
		t.Fatalf("maxReleaseDateSkew = %v, want apt's Acquire::Max-FutureTime default (10s)", maxReleaseDateSkew)
	}
}
