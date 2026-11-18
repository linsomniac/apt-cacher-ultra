package freshness

import (
	"bytes"
	"io"
	"path"

	"github.com/linsomniac/apt-cacher-ultra/internal/indexrecovery"
)

// Callers pass fetched members, whose SHA-256 was verified or derived from
// the verified bytes for SHA-512-only releases.
func isVerifiedEmptyMember(m ReleaseMember) bool {
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	return m.Size == 0 && m.SHA256 == emptySHA256
}

// Build the lookup once, only after encountering a zero-byte Packages.gz.
// A declared plain sibling that could not be fetched cannot serve clients.
func verifiedEmptyPackagesPaths(fetched []ReleaseMember) map[string]bool {
	paths := make(map[string]bool)
	for _, m := range fetched {
		if isVerifiedEmptyMember(m) && path.Base(m.Path) == "Packages" {
			paths[m.Path] = true
		}
	}
	return paths
}

// readGzipPackagesBlob shares the bounded, trailer-checking decoder used
// when reconstructing an uncompressed index from its verified sibling.
func readGzipPackagesBlob(r io.Reader, capBytes int64) ([]byte, error) {
	var out bytes.Buffer
	if err := indexrecovery.CopyGzip(&out, r, capBytes); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
