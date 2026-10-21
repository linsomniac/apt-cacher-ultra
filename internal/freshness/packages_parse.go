package freshness

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PackageRef is one .deb entry from a Packages file. Phase 2 fields:
// Filename and SHA256 (Size for sanity). Phase 3 adds Package and
// Architecture so the SPEC3 §7.5.3 hot-set match — which keys on
// (binary package name, architecture) across snapshot transitions —
// has the identity tuple it needs. A stanza missing Package or
// Architecture still contributes a row to package_hash for hash
// validation; the hot-set query excludes empty values explicitly.
type PackageRef struct {
	Filename     string // archive-relative, e.g. "pool/main/f/foo/foo_1.2-3_amd64.deb"
	SHA256       string // 64 lowercase hex
	Size         int64  // declared bytes; 0 when the stanza omitted Size:
	Package      string // binary package name (apt's `Package:` stanza), "" when missing
	Architecture string // e.g. "amd64", "arm64", "all"; "" when missing
	Version      string // Debian Version: string; "" when missing (malformed binary stanza)
}

// MaxPackageStanzas caps how many entries parsePackages keeps: the rows
// it returns and the no_sha256 Filenames it reports, together (review fix
// round 2, Task 2: a stanza without a SHA256 costs memory too).
// Ubuntu noble main amd64 Packages is roughly 50k stanzas; 1M leaves
// room for fan-out of multiple suites in a single Release while still
// bounding the memory cost of a hostile (allowlisted) upstream feeding
// us a synthetic Packages file through a successful adoption.
const MaxPackageStanzas = 1_000_000

// ParsePackages parses an apt Packages file (uncompressed) and returns
// every stanza that declares both Filename and SHA256 (parsePackages
// without its no_sha256 report). Stanzas missing either field vouch
// nothing — SPEC2 §7.5 step 8 says "for each pkg with Filename and
// SHA256." A stanza that declares both but lists a malformed SHA256
// (wrong length, non-hex, mixed case) is an error; a corrupted-but-signed
// Packages file should fail adoption, not slip through with a bogus row.
func ParsePackages(text []byte) ([]PackageRef, error) {
	refs, _, err := parsePackages(text)
	return refs, err
}

// parsePackages is ParsePackages that also returns the Filename of every
// stanza that declares a Filename but no SHA256 (in file order, repeats
// kept). Such a stanza vouches nothing, and spec C5 makes it no silent
// drop: the adopter marks the snapshot's coverage incomplete (reason
// no_sha256, buildPackageHashes). A stanza without a Filename is not a
// package file and is not reported.
//
// AIDEV-NOTE: callers pass already-decompressed bytes. Decompression
// of Packages.gz / Packages.xz is the fetch layer's job, not parsing.
//
// Field-name matching is case-insensitive per RFC822/Debian policy.
// Multi-line continuation lines (leading space/tab) are recognized
// and skipped — none of the fields we care about (Filename, SHA256,
// Size) have multi-line values in the wild, so dropping the body of
// continuation lines is safe.
func parsePackages(text []byte) ([]PackageRef, []string, error) {
	var (
		out      []PackageRef
		noSHA256 []string
	)
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	var (
		filename string
		sha256   string
		size     int64
		sizeSet  bool
		pkg      string
		arch     string
		version  string
		lineNo   int
	)

	flush := func() error {
		// Stanzas missing either required field give no row: a
		// Packages file legitimately contains entries that don't
		// declare both (rare, but well-formed apt allows it). A
		// Filename without a SHA256 is reported (spec C5 no_sha256):
		// the caller makes coverage incomplete rather than dropping it
		// silently.
		//
		// AIDEV-NOTE: a gap Filename is deliberately NOT passed through
		// validateFilename. It becomes no row and no path: the caller
		// only counts the distinct names (buildPackageHashes logs
		// no_sha256_stanzas), never a name. Since ruling R-G1 a
		// body made only of such stanzas parses, so a signed index of
		// odd gap names (even "/etc/x") adopts with incomplete coverage;
		// that is harmless while the names stay count-only. If a gap
		// Filename ever reaches a path, a log value or a row, validate it
		// here first, fatal like a row's.
		if filename == "" || sha256 == "" {
			if filename != "" {
				if len(out)+len(noSHA256) >= MaxPackageStanzas {
					return fmt.Errorf("exceeds %d stanzas", MaxPackageStanzas)
				}
				noSHA256 = append(noSHA256, filename)
			}
			filename = ""
			sha256 = ""
			size = 0
			sizeSet = false
			pkg = ""
			arch = ""
			version = ""
			return nil
		}
		if !validHexSHA256(sha256) {
			return fmt.Errorf("invalid sha256 %q (filename %q)", sha256, filename)
		}
		if err := validateFilename(filename); err != nil {
			return fmt.Errorf("invalid filename %q: %w", filename, err)
		}
		if len(out)+len(noSHA256) >= MaxPackageStanzas {
			return fmt.Errorf("exceeds %d stanzas", MaxPackageStanzas)
		}
		var s int64
		if sizeSet {
			s = size
		}
		out = append(out, PackageRef{
			Filename:     filename,
			SHA256:       sha256,
			Size:         s,
			Package:      pkg,
			Architecture: arch,
			Version:      version,
		})
		filename = ""
		sha256 = ""
		size = 0
		sizeSet = false
		pkg = ""
		arch = ""
		version = ""
		return nil
	}

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, nil, fmt.Errorf("packages: line %d: %w", lineNo, err)
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// Continuation of the previous field (e.g. multi-line
			// Description). None of the fields we extract have
			// continuation values in real Packages files, so skip.
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			return nil, nil, fmt.Errorf("packages: line %d: malformed header %q", lineNo, line)
		}
		field := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		switch strings.ToLower(field) {
		case "filename":
			filename = value
		case "sha256":
			sha256 = value
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("packages: line %d: invalid size %q: %w", lineNo, value, err)
			}
			if n < 0 {
				return nil, nil, fmt.Errorf("packages: line %d: negative size %d", lineNo, n)
			}
			size = n
			sizeSet = true
		case "package":
			// SPEC3 §7.5.2: the binary package name. apt URL routing
			// keys on the binary package's filename, so the binary
			// `Package:` (not the optional `Source:`) is the correct
			// identity for hot-set matching across snapshots.
			pkg = value
		case "architecture":
			arch = value
		case "version":
			// Version-aware retention: the Debian version the mirror rule
			// ranks distinct versions on. Carried into package_hash.version.
			version = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("packages: scan: %w", err)
	}
	// EOF without a trailing blank line still ends the last stanza.
	if err := flush(); err != nil {
		return nil, nil, fmt.Errorf("packages: tail: %w", err)
	}
	if len(out) == 0 && len(noSHA256) == 0 && len(bytes.TrimSpace(text)) > 0 {
		// A non-blank input in which no stanza names a Filename is no
		// Packages index — a foreign or truncated file (a real HTML error
		// page usually fails earlier, at its first line: malformed
		// header). The freshness layer logs it as adoption_parse_failed
		// and the next periodic tick retries.
		//
		// AIDEV-NOTE: ruling R-G1 (the E carry-forward). A body whose
		// stanzas all name a Filename but none a SHA256 (an index that
		// declares only SHA512, SHA1 or MD5sum) parses: no rows, every
		// Filename reported, so its snapshot adopts with incomplete
		// coverage (no_sha256, buildPackageHashes) instead of failing at
		// every check. A whitespace-only body (an empty component's real,
		// empty index) was never an error and still parses to nothing.
		return nil, nil, errors.New("packages: no stanzas with a Filename")
	}
	return out, noSHA256, nil
}

// maxFilenameBytes bounds a Packages Filename (spec C5 validateFilename).
const maxFilenameBytes = 4096

// validateFilename is spec C5's check of a Packages Filename, in place of
// the Release members' validateMemberPath (which ParseRelease keeps): it
// allows "." segments — apt requests <base>/./x.deb literally, and a
// dpkg-scanpackages index (CUDA, libnvidia-container) writes ./x.deb — and
// "%" (literal bytes, re-encoded upstream by spec C1). It refuses, fatal
// as before: an empty name, an absolute one, NUL or another control byte,
// a backslash, "?" or "#", a ".." segment, an empty segment, a name longer
// than maxFilenameBytes, and a name made only of "." segments (it names no
// file, and its DotClean is empty: plan design note 5).
//
// AIDEV-NOTE: the literal Filename is the package_hash path's tail (spec
// C6: FlatBase(suite) + "/" + F, kept literally because apt requests
// exactly that), so anything that could step outside the base — "..", an
// absolute name, an empty segment that would collapse a path — stays
// refused here.
func validateFilename(f string) error {
	switch {
	case f == "":
		return errors.New("empty")
	case len(f) > maxFilenameBytes:
		return fmt.Errorf("longer than %d bytes", maxFilenameBytes)
	case strings.HasPrefix(f, "/"):
		return errors.New("absolute path not allowed")
	case strings.ContainsRune(f, '\\'):
		return errors.New("contains backslash")
	case strings.ContainsAny(f, "?#"):
		return errors.New("contains URL delimiter")
	}
	for i := 0; i < len(f); i++ {
		if c := f[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("contains control byte 0x%02x", c)
		}
	}
	named := false
	for _, seg := range strings.Split(f, "/") {
		switch seg {
		case "":
			return errors.New("contains empty segment")
		case "..":
			return errors.New("contains .. segment")
		case ".":
		default:
			named = true
		}
	}
	if !named {
		return errors.New("names no file")
	}
	return nil
}
