package indexrecovery

import (
	"bufio"
	"io"

	"github.com/ulikunitz/xz"
)

// xzSourceBuffer is how much of its source NewXZReader's decoder reads at a
// time.
const xzSourceBuffer = 64 << 10

// NewXZReader returns a decoder for the xz stream r holds. Like the library's
// own reader it accepts stream padding and concatenated streams, reads r to
// its end, and fails on anything else after a stream.
//
// AIDEV-NOTE: the buffer is the point of this function. The library asks its
// source for ONE byte per Read (lzma.ByteReader wraps a source that is not an
// io.ByteReader in a one-byte reader, and the library hands it an
// io.LimitReader). Given an *os.File that is a read system call per
// compressed byte: 1,474,522 calls for a 1,474,640-byte Packages.xz. On a
// production host (2026-11-21) that was nearly all of an adoption's
// parse_indexes phase, 38-40 s for a 66,000-row suite. Every xz decoder in
// the program is built here, so a new call site cannot leave the buffer out
// (TestXZDecoderIsBuiltOnlyHere; TestNewXZReader_ReadsItsSourceInBlocks
// counts the reads).
func NewXZReader(r io.Reader) (io.Reader, error) {
	return xz.NewReader(bufio.NewReaderSize(r, xzSourceBuffer))
}
