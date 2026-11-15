package freshness

import (
	"bytes"
	"io"

	"github.com/linsomniac/apt-cacher-ultra/internal/indexrecovery"
)

// readGzipPackagesBlob shares the bounded, trailer-checking decoder used
// when reconstructing an uncompressed index from its verified sibling.
func readGzipPackagesBlob(r io.Reader, capBytes int64) ([]byte, error) {
	var out bytes.Buffer
	if err := indexrecovery.CopyGzip(&out, r, capBytes); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
