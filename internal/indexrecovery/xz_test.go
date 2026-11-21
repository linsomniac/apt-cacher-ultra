package indexrecovery

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// xzLibrary is the decoder's import path.
const xzLibrary = "github.com/ulikunitz/xz"

// Every xz decoder in the program is built by NewXZReader, which puts the
// buffer in front that the decoder needs. A file that imports the library
// itself can build one without it, and nothing else would notice: the bytes
// decode the same, only slower by a system call per compressed byte.
func TestXZDecoderIsBuiltOnlyHere(t *testing.T) {
	const only = "internal/indexrecovery/xz.go"
	fset := token.NewFileSet()
	found := false
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join("..", "..", root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(filepath.Join("..", ".."), p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for _, imp := range f.Imports {
				ip, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if ip != xzLibrary {
					continue
				}
				if rel == only {
					found = true
					continue
				}
				t.Errorf("%s imports %s: build the decoder with indexrecovery.NewXZReader", rel, xzLibrary)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Errorf("%s does not import %s: the test no longer looks at the file that builds the decoder", only, xzLibrary)
	}
}

// The decoder asks its source for one byte at a time. Behind NewXZReader the
// source is read in blocks, so a file costs a system call per block and not
// one per compressed byte.
func TestNewXZReader_ReadsItsSourceInBlocks(t *testing.T) {
	plain := packagesLikeText(12000)
	packed := xzOf(t, plain)
	if len(packed) < 4*xzSourceBuffer {
		t.Fatalf("the stream is %d bytes: it must span several buffers", len(packed))
	}
	src := &readCounter{r: bytes.NewReader(packed)}
	r, err := NewXZReader(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decoded %d bytes; they differ from the %d that were compressed", len(got), len(plain))
	}
	if src.bytes != len(packed) {
		t.Errorf("the decoder read %d of the source's %d bytes: callers count on it reading to the end", src.bytes, len(packed))
	}
	// A full buffer per read, and a few reads to find the end.
	if most := len(packed)/xzSourceBuffer + 4; src.calls > most {
		t.Errorf("%d reads of the source for %d bytes (%.1f bytes a read), want at most %d reads",
			src.calls, len(packed), float64(src.bytes)/float64(src.calls), most)
	}
}

// readCounter counts the Read calls its source receives. It is deliberately
// not an io.ByteReader, like the *os.File an adoption hands the decoder.
type readCounter struct {
	r     io.Reader
	calls int
	bytes int
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.calls++
	c.bytes += n
	return n, err
}

// packagesLikeText is text that compresses the way an index does: far from
// nothing, so the compressed stream is long enough to count reads on.
func packagesLikeText(stanzas int) []byte {
	rng := rand.New(rand.NewSource(1))
	var b bytes.Buffer
	for i := 0; i < stanzas; i++ {
		fmt.Fprintf(&b, "Package: pkg%d\nVersion: %d.%d-%d\nArchitecture: amd64\nFilename: pool/main/p/pkg%d/pkg%d_%d_amd64.deb\nSize: %d\nSHA256: %064x\n\n",
			i, rng.Intn(9), rng.Intn(99), rng.Intn(9), i, i, rng.Intn(999), rng.Intn(1<<24), rng.Uint64())
	}
	return b.Bytes()
}

func xzOf(t *testing.T, plain []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := xz.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
