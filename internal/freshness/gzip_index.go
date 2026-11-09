package freshness

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// readGzipPackagesBlob decompresses gzip index members, accepting only NUL
// padding after the final member (observed in ClickHouse's Packages.gz).
// Each member's CRC and size trailer must validate; concatenated members share
// one output cap. Non-NUL trailing data and padding between members fail closed.
// The caller supplies a finite cached blob whose complete bytes, including any
// padding, have already passed Release size/hash verification.
// capBytes is nonnegative and less than math.MaxInt64; production uses 256 MiB.
func readGzipPackagesBlob(r io.Reader, capBytes int64) ([]byte, error) {
	// A ByteReader keeps gzip from consuming bytes past a member trailer.
	br := bufio.NewReader(r)
	gr, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	var out bytes.Buffer
	remainingCap := capBytes + 1
	for {
		gr.Multistream(false)
		n, err := io.Copy(&out, io.LimitReader(gr, remainingCap))
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		remainingCap -= n
		if remainingCap <= 0 {
			return nil, fmt.Errorf("decompressed Packages.gz size exceeds %d-byte cap (bomb defense)",
				capBytes)
		}
		hasMore, err := hasNextGzipMember(br)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		if !hasMore {
			return out.Bytes(), nil
		}
		if err := gr.Reset(br); err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
	}
}

// hasNextGzipMember inspects the bytes following a completed gzip member.
// If the reader is at EOF or contains only 0x00 padding through EOF, it
// returns (false, nil). If a non-NUL byte is encountered before any NUL
// byte, it leaves the byte unread in br so gzip.Reader.Reset can parse the
// next member header (or return gzip.ErrHeader). If a non-NUL byte appears
// after one or more NUL padding bytes, it returns gzip.ErrHeader directly.
func hasNextGzipMember(br *bufio.Reader) (bool, error) {
	sawNul := false
	for {
		b, err := br.ReadByte()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if b == 0 {
			sawNul = true
			continue
		}
		if sawNul {
			return false, gzip.ErrHeader
		}
		if err := br.UnreadByte(); err != nil {
			return false, err
		}
		return true, nil
	}
}
