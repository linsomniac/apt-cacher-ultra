// Package debversion implements Debian version-string comparison
// compatible with dpkg's verrevcmp / `dpkg --compare-versions`.
//
// A Debian version is `[epoch:]upstream_version[-debian_revision]`. The
// only consumer in this project is the version-aware cache retention /
// prefetch logic (SPEC: version-aware retention design), which must rank
// the versions of a (package, architecture) the way apt would so it keeps
// the newest N. Getting this wrong deletes the versions apt would install,
// so the comparator is covered by an extensive truth table in the test.
package debversion

import (
	"strings"
)

// Compare returns -1 if a sorts before b, +1 if a sorts after b, and 0 if
// they are equal, using dpkg version-ordering semantics: epoch (numeric)
// dominates, then upstream_version, then debian_revision, each compared by
// verrevcmp.
func Compare(a, b string) int {
	ea, ua, ra := parse(a)
	eb, ub, rb := parse(b)
	if c := compareNumericString(ea, eb); c != 0 {
		return c
	}
	if c := verrevcmp(ua, ub); c != 0 {
		return c
	}
	return verrevcmp(ra, rb)
}

// parse splits a version into (epoch, upstream, revision). A leading
// `<digits>:` is the epoch as a raw digit string (empty == 0 when absent
// or non-numeric); the debian_revision is everything after the LAST hyphen
// (empty when absent, which compares equal to "0" via verrevcmp).
func parse(v string) (epoch, upstream, revision string) {
	rest := v
	if i := strings.IndexByte(rest, ':'); i > 0 {
		if isAllDigits(rest[:i]) {
			epoch = rest[:i]
			rest = rest[i+1:]
		}
	}
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		upstream = rest[:i]
		revision = rest[i+1:]
	} else {
		upstream = rest
	}
	return epoch, upstream, revision
}

// compareNumericString compares two non-negative integers given as digit
// strings without converting to a fixed-width int — epochs are unbounded in
// principle and strconv.Atoi would overflow and silently misorder a huge
// epoch. Empty and all-zero strings are both 0. Strips leading zeros, then
// compares by length and lexically.
func compareNumericString(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// verrevcmp is the dpkg core string comparison, applied independently to
// the upstream and revision parts. It alternates non-digit and digit runs:
// non-digit runs compare via order() (where '~' sorts before everything,
// letters before other punctuation), digit runs compare numerically with
// leading zeros stripped.
func verrevcmp(a, b string) int {
	ai, bi := 0, 0
	la, lb := len(a), len(b)
	for ai < la || bi < lb {
		firstDiff := 0
		// Non-digit run: continue while either side is at a non-digit
		// char (end-of-string is treated as the 0 char, which order()
		// maps to 0). Two digits are never compared here — if both sides
		// are at a digit the loop condition is false.
		for (ai < la && !isDigit(a[ai])) || (bi < lb && !isDigit(b[bi])) {
			ac := order(byteAt(a, ai))
			bc := order(byteAt(b, bi))
			if ac != bc {
				return clamp(ac - bc)
			}
			ai++
			bi++
		}
		// Strip leading zeros so digit runs compare numerically.
		for ai < la && a[ai] == '0' {
			ai++
		}
		for bi < lb && b[bi] == '0' {
			bi++
		}
		// Digit run: equal-length numeric comparison; the first differing
		// digit decides only if the runs are the same length.
		for ai < la && isDigit(a[ai]) && bi < lb && isDigit(b[bi]) {
			if firstDiff == 0 {
				firstDiff = int(a[ai]) - int(b[bi])
			}
			ai++
			bi++
		}
		if ai < la && isDigit(a[ai]) {
			return 1 // a's digit run is longer ⇒ larger number
		}
		if bi < lb && isDigit(b[bi]) {
			return -1
		}
		if firstDiff != 0 {
			return clamp(firstDiff)
		}
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// byteAt returns the byte at i, or 0 (end-of-string sentinel) when out of
// range — matching dpkg's reliance on the NUL terminator.
func byteAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// order maps a character to its dpkg sort weight: digits 0 (handled
// separately), letters their ASCII value, '~' before everything (-1), the
// NUL/end 0, and any other punctuation its ASCII value + 256 (so it sorts
// after letters but after '~').
func order(c byte) int {
	switch {
	case isDigit(c):
		return 0
	case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		return int(c)
	case c == '~':
		return -1
	case c == 0:
		return 0
	default:
		return int(c) + 256
	}
}

func clamp(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
