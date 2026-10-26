package freshness

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// maxReleaseDateSkew is apt's Acquire::Max-FutureTime default: a Release
// dated further ahead than this is "not valid yet" (spec C5).
const maxReleaseDateSkew = 10 * time.Second

// aptSpace is C's isspace in the "C" locale, apt's whitespace: ASCII
// only, so a no-break space or other Unicode space is an ordinary
// character (deferred minor B-9).
const aptSpace = " \t\n\v\f\r"

// releaseDate returns the Date field of verified Release text in Unix
// seconds, or 0 when the field is missing or apt would not parse it (apt
// treats both as 0: debmetaindex.cc).
//
// AIDEV-NOTE: every top-level line is read, where apt reads the first
// paragraph only (deferred minor B-13, declined in phase H): real Release
// files are one paragraph, and this package's test verifiers hand the
// parsers the whole clearsign envelope — its armor header and blank line
// first — so a first-paragraph reader finds no Date in their fixtures.
func releaseDate(text []byte) int64 {
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.Trim(key, aptSpace), "Date") {
			continue
		}
		if t, ok := parseAptDate(strings.Trim(val, aptSpace)); ok {
			return t
		}
		return 0
	}
	return 0
}

// parseAptDate ports apt's RFC1123StrToTime (apt-pkg/contrib/strutl.cc,
// apt 2.x/3.x). It accepts, in UTC only:
//
//	Sun, 06 Nov 1994 08:49:37 GMT   RFC 1123; zone GMT, UTC, Z or a zero numeric offset
//	Sun Nov  6 08:49:37 1994        asctime
//	Sunday, 06-Nov-94 08:49:37 GMT  RFC 850; the two-digit year is 19yy
//
// AIDEV-NOTE: a port, quirks included, so the cache refuses exactly what
// apt refuses: the weekday is checked only by its first three letters and
// never against the date; an unknown month name means January (MonthConv);
// text after the seconds is ignored (strptime); a day past the month's end
// rolls over (timegm, so :60/:61 "leap seconds" roll into the next minute).
// The clock fields (aptClock/num) stop consuming a second digit once doing
// so could not keep the value within range (glibc's get_number): a seconds
// value of 70-99 truncates to its ones digit (08:49:70 parses as :07) while
// 62 and above is rejected outright, and the same early stop in hours or
// minutes leaves a stray digit where ':' must follow, rejecting values like
// "30:00:00" or "08:70:00" for that reason rather than a range check.
//
// Two deliberate divergences remain, both in the day token. In RFC 1123
// and asctime form (aptDigits, unsigned, the whole token digits), apt
// reads it with istream >>, which stops at the first non-digit, so a token
// such as "6," parses there as 6, where this rejects it outright. In RFC
// 850 form (aptStoi, then parseAptDate's day > 65535 bound), apt keeps
// std::stoi's result in an unsigned short, so a day above 65535 wraps
// there and is rejected here (deferred minor B-14: not aptDigits). Real
// Release files never reach either difference.
// Year (aptSignedInt) and the RFC 850 month/year parts (aptStoi) are not
// part of this divergence: they accept an optional sign and still require
// full consumption, matching istream >> int / std::stoi (apt's own code
// checks that stoi consumed the whole token there too). TestParseAptDate
// pins the behaviour.
func parseAptDate(s string) (int64, bool) {
	f := strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(aptSpace, r) })
	if len(f) == 0 {
		return 0, false
	}
	wd := strings.ToLower(f[0])
	if len(wd) < 3 || !isAptWeekday(wd[:3]) {
		return 0, false
	}
	var (
		year, month, day int
		timespec, zone   string
		ok               bool
	)
	switch {
	case len(wd) == 3: // asctime: Sun Nov  6 08:49:37 1994
		if len(f) != 5 {
			return 0, false
		}
		month = aptMonth(f[1])
		if day, ok = aptDigits(f[2]); !ok {
			return 0, false
		}
		timespec = f[3]
		if year, ok = aptSignedInt(f[4]); !ok {
			return 0, false
		}
		zone = "UTC"
	case len(wd) == 4: // RFC 1123: Sun, 06 Nov 1994 08:49:37 GMT
		if wd[3] != ',' || len(f) != 6 {
			return 0, false
		}
		if day, ok = aptDigits(f[1]); !ok {
			return 0, false
		}
		month = aptMonth(f[2])
		if year, ok = aptSignedInt(f[3]); !ok {
			return 0, false
		}
		timespec, zone = f[4], f[5]
	default: // RFC 850: Sunday, 06-Nov-94 08:49:37 GMT
		if wd[len(wd)-1] != ',' || len(f) != 4 {
			return 0, false
		}
		parts := strings.Split(f[1], "-")
		if len(parts) != 3 {
			return 0, false
		}
		if day, ok = aptStoi(parts[0]); !ok || day < 0 || day > 65535 {
			return 0, false
		}
		var yy int
		if yy, ok = aptStoi(parts[2]); !ok {
			return 0, false
		}
		year = 1900 + yy
		month = aptMonth(parts[1])
		timespec, zone = f[2], f[3]
	}
	if !aptUTCZone(zone) {
		return 0, false
	}
	// strptime("%Y-%m-%d %H:%M:%S") over "%.4d-%.2d-%.2d <timespec>":
	// %Y takes 0..9999 and %d 1..31.
	if year < 0 || year > 9999 || day < 1 || day > 31 {
		return 0, false
	}
	h, mi, sec, ok := aptClock(timespec)
	if !ok {
		return 0, false
	}
	return time.Date(year, time.Month(month), day, h, mi, sec, 0, time.UTC).Unix(), true
}

func isAptWeekday(s string) bool {
	switch s {
	case "sun", "mon", "tue", "wed", "thu", "fri", "sat":
		return true
	}
	return false
}

// aptMonth is apt's MonthConv plus one: it looks at up to three letters
// and defaults to January.
func aptMonth(m string) int {
	at := func(i int) byte {
		if i >= len(m) {
			return 0
		}
		c := m[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		return c
	}
	switch at(0) {
	case 'a':
		if at(1) == 'p' {
			return 4
		}
		return 8
	case 'd':
		return 12
	case 'f':
		return 2
	case 'j':
		if at(1) == 'a' {
			return 1
		}
		if at(2) == 'n' {
			return 6
		}
		return 7
	case 'm':
		if at(2) == 'r' {
			return 3
		}
		return 5
	case 'n':
		return 11
	case 'o':
		return 10
	case 's':
		return 9
	}
	return 1
}

// aptDigits reads an unsigned short token: digits only, at most 65535.
func aptDigits(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v > 65535 {
		return 0, false
	}
	return v, true
}

// aptSignedInt reads a signed int token (optional sign, digits).
func aptSignedInt(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// aptStoi is std::stoi with "the whole string was consumed": optional
// sign, at least one digit, nothing else.
func aptStoi(s string) (int, bool) {
	return aptSignedInt(s)
}

// aptUTCZone accepts GMT, UTC, Z, or any numeric zone equal to zero.
func aptUTCZone(z string) bool {
	switch z {
	case "GMT", "UTC", "Z":
		return true
	}
	v, ok := aptStoi(z)
	return ok && v == 0
}

// aptClock parses strptime's "%H:%M:%S": one or two digits each, hour
// 0..23, minute 0..59, second 0..61; anything after the seconds is
// ignored, as strptime ignores it.
func aptClock(ts string) (h, m, s int, ok bool) {
	i := 0
	// num mirrors glibc's get_number, which strptime's %H/%M/%S each call
	// through: it reads a first digit unconditionally, then reads a
	// second digit only if doing so could not push the value past max.
	// A first digit of 7, 8 or 9 therefore ends a 0..61 field (%S) on one
	// digit, leaving the next character as trailing text — accepted here
	// (seconds 70-99 truncate to their ones digit) or, for %H/%M, left as
	// a stray digit where ':' must follow next, which fails colon() and
	// rejects the whole clock.
	num := func(max int) (int, bool) {
		if i >= len(ts) || ts[i] < '0' || ts[i] > '9' {
			return 0, false
		}
		v := int(ts[i] - '0')
		i++
		if v*10 <= max && i < len(ts) && ts[i] >= '0' && ts[i] <= '9' {
			v = v*10 + int(ts[i]-'0')
			i++
		}
		return v, v <= max
	}
	colon := func() bool {
		if i < len(ts) && ts[i] == ':' {
			i++
			return true
		}
		return false
	}
	var good bool
	if h, good = num(23); !good || !colon() {
		return 0, 0, 0, false
	}
	if m, good = num(59); !good || !colon() {
		return 0, 0, 0, false
	}
	if s, good = num(61); !good {
		return 0, 0, 0, false
	}
	return h, m, s, true
}
