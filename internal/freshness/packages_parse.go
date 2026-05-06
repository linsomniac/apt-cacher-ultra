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
}

// MaxPackageStanzas caps how many entries ParsePackages will return.
// Ubuntu noble main amd64 Packages is roughly 50k stanzas; 1M leaves
// room for fan-out of multiple suites in a single Release while still
// bounding the memory cost of a hostile (allowlisted) upstream feeding
// us a synthetic Packages file through a successful adoption.
const MaxPackageStanzas = 1_000_000

// ParsePackages parses an apt Packages file (uncompressed) and returns
// every stanza that declares both Filename and SHA256. Stanzas missing
// either field are silently skipped — SPEC2 §7.5 step 8 says "for each
// pkg with Filename and SHA256." A stanza that declares both but lists
// a malformed SHA256 (wrong length, non-hex, mixed case) is an error;
// a corrupted-but-signed Packages file should fail adoption, not slip
// through with a bogus row.
//
// AIDEV-NOTE: callers pass already-decompressed bytes. Decompression
// of Packages.gz / Packages.xz is the fetch layer's job, not parsing.
//
// Field-name matching is case-insensitive per RFC822/Debian policy.
// Multi-line continuation lines (leading space/tab) are recognized
// and skipped — none of the fields we care about (Filename, SHA256,
// Size) have multi-line values in the wild, so dropping the body of
// continuation lines is safe.
func ParsePackages(text []byte) ([]PackageRef, error) {
	var out []PackageRef
	scanner := bufio.NewScanner(bytes.NewReader(text))
	scanner.Buffer(make([]byte, 0, scanBufCap), scanBufCap)

	var (
		filename string
		sha256   string
		size     int64
		sizeSet  bool
		pkg      string
		arch     string
		lineNo   int
	)

	flush := func() error {
		// Stanzas missing either required field are intentionally
		// silent: a Packages file legitimately contains entries that
		// don't declare both (rare, but well-formed apt allows it).
		if filename == "" || sha256 == "" {
			filename = ""
			sha256 = ""
			size = 0
			sizeSet = false
			pkg = ""
			arch = ""
			return nil
		}
		if !validHexSHA256(sha256) {
			return fmt.Errorf("invalid sha256 %q (filename %q)", sha256, filename)
		}
		if err := validateMemberPath(filename); err != nil {
			return fmt.Errorf("invalid filename %q: %w", filename, err)
		}
		if len(out) >= MaxPackageStanzas {
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
		})
		filename = ""
		sha256 = ""
		size = 0
		sizeSet = false
		pkg = ""
		arch = ""
		return nil
	}

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, fmt.Errorf("packages: line %d: %w", lineNo, err)
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
			return nil, fmt.Errorf("packages: line %d: malformed header %q", lineNo, line)
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
				return nil, fmt.Errorf("packages: line %d: invalid size %q: %w", lineNo, value, err)
			}
			if n < 0 {
				return nil, fmt.Errorf("packages: line %d: negative size %d", lineNo, n)
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
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("packages: scan: %w", err)
	}
	// EOF without a trailing blank line still ends the last stanza.
	if err := flush(); err != nil {
		return nil, fmt.Errorf("packages: tail: %w", err)
	}
	if len(out) == 0 {
		// A non-empty input that produced zero usable stanzas is
		// suspicious enough to flag — likely a downloaded HTML error
		// page or a truncated file. The freshness layer logs this as
		// adoption_parse_failed and the next periodic tick retries.
		if len(bytes.TrimSpace(text)) > 0 {
			return nil, errors.New("packages: no stanzas with both Filename and SHA256")
		}
	}
	return out, nil
}
