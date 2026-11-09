package freshness

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func TestReadGzipPackagesBlob(t *testing.T) {
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	first := gzipBytes([]byte("first\n"))
	second := gzipBytes([]byte("second\n"))
	empty := gzipBytes(nil)
	badCRC := bytes.Clone(second)
	badCRC[len(badCRC)-8] ^= 1
	badSize := bytes.Clone(second)
	badSize[len(badSize)-4] ^= 1
	padding := make([]byte, 8193) // Cross several bufio buffer boundaries.

	for _, tc := range []struct {
		name string
		body []byte
		want string
		err  error
	}{
		{"single", first, "first\n", nil},
		{"one_nul", join(first, []byte{0}), "first\n", nil},
		{"long_padding", join(first, padding), "first\n", nil},
		{"concatenated", join(first, second), "first\nsecond\n", nil},
		{"concatenated_padded", join(first, second, padding), "first\nsecond\n", nil},
		{"empty_member", empty, "", nil},
		{"empty_member_padded", join(empty, padding), "", nil},
		{"empty_members", join(empty, first, empty, second, empty, padding), "first\nsecond\n", nil},
		{"no_member", nil, "", io.EOF},
		{"only_nuls", padding, "", gzip.ErrHeader},
		{"leading_nul", join([]byte{0}, first), "", gzip.ErrHeader},
		{"garbage", join(first, []byte("not a gzip member")), "", gzip.ErrHeader},
		{"garbage_after_padding", join(first, padding, []byte{1}), "", gzip.ErrHeader},
		{"member_after_padding", join(first, padding, second), "", gzip.ErrHeader},
		{"short_garbage", join(first, []byte{1}), "", io.ErrUnexpectedEOF},
		{"partial_next_header", join(first, second[:5]), "", io.ErrUnexpectedEOF},
		{"partial_deflate", second[:12], "", io.ErrUnexpectedEOF},
		{"partial_trailer", second[:len(second)-1], "", io.ErrUnexpectedEOF},
		{"partial_second_trailer", join(first, second[:len(second)-1]), "", io.ErrUnexpectedEOF},
		{"bad_crc", join(badCRC, padding), "", gzip.ErrChecksum},
		{"bad_size", join(badSize, padding), "", gzip.ErrChecksum},
		{"bad_second_crc", join(first, badCRC, padding), "", gzip.ErrChecksum},
		{"bad_second_size", join(first, badSize, padding), "", gzip.ErrChecksum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"buffered", "one_byte", "data_with_eof"} {
				t.Run(mode, func(t *testing.T) {
					var r io.Reader = bytes.NewReader(tc.body)
					switch mode {
					case "one_byte":
						r = iotest.OneByteReader(r)
					case "data_with_eof":
						r = iotest.DataErrReader(r)
					}
					got, err := readGzipPackagesBlob(r, 1024)
					if !errors.Is(err, tc.err) {
						t.Fatalf("error = %v, want %v", err, tc.err)
					}
					if err != nil && got != nil {
						t.Fatal("returned partial output after a decoding failure")
					}
					if string(got) != tc.want {
						t.Fatalf("body = %q, want %q", got, tc.want)
					}
				})
			}
		})
	}
}

func TestReadGzipPackagesBlobCap(t *testing.T) {
	// The cap applies to total output, even when another empty member or
	// padding follows. Test the decoder directly, without a mutable Adopter cap.
	for _, members := range [][][]byte{
		{nil},
		{[]byte("abcdef")},
		{[]byte("abc"), []byte("def")},
		{nil, []byte("abc"), nil, []byte("def"), nil},
	} {
		var compressed, plain []byte
		for _, member := range members {
			compressed = append(compressed, gzipBytes(member)...)
			plain = append(plain, member...)
		}
		compressed = append(compressed, make([]byte, 4097)...)
		for _, delta := range []int{-1, 0, 1} {
			capBytes := int64(len(plain) + delta)
			if capBytes < 0 {
				continue
			}
			got, err := readGzipPackagesBlob(bytes.NewReader(compressed), capBytes)
			if delta < 0 {
				if err == nil || !strings.Contains(err.Error(), "bomb defense") || got != nil {
					t.Fatalf("%d members, cap %d: body %q, error %v; want cap rejection", len(members), capBytes, got, err)
				}
			} else if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("%d members, cap %d: body %q, error %v", len(members), capBytes, got, err)
			}
		}
	}

	// The old bomb-defense test only asserted a Packages parser failure on a
	// below-cap NUL body. This proves the actual decompression cap rejects it.
	bomb := gzipBytes(make([]byte, 1<<20))
	got, err := readGzipPackagesBlob(bytes.NewReader(bomb), 64<<10)
	if err == nil || !strings.Contains(err.Error(), "bomb defense") || got != nil {
		t.Fatalf("bomb: body length %d, error %v; want cap rejection", len(got), err)
	}

	// Reaching exactly the cap must still validate the trailer and later members.
	member := gzipBytes([]byte("abc"))
	member[len(member)-8] ^= 1
	for _, body := range [][]byte{member, append(gzipBytes(nil), member...)} {
		if got, err := readGzipPackagesBlob(bytes.NewReader(body), 3); !errors.Is(err, gzip.ErrChecksum) || got != nil {
			t.Fatalf("checksum at cap: body %q, error %v", got, err)
		}
	}
}

func TestReadGzipPackagesBlobReadErrors(t *testing.T) {
	member := gzipBytes([]byte("packages\n"))
	wantErr := errors.New("index read failed")
	for _, prefix := range [][]byte{
		nil, member[:5], member[:12], member[:len(member)-4], member,
		append(bytes.Clone(member), make([]byte, 8193)...),
	} {
		r := io.MultiReader(bytes.NewReader(prefix), iotest.ErrReader(wantErr))
		got, err := readGzipPackagesBlob(r, 1024)
		if !errors.Is(err, wantErr) || got != nil {
			t.Fatalf("read failure after %d bytes: body %q, error %v", len(prefix), got, err)
		}
	}
}

func TestAdopter_GzipFailurePreservesSnapshot(t *testing.T) {
	const memberPath = "main/binary-amd64/Packages.gz"
	const memberURL = "http://archive.ubuntu.com/ubuntu/dists/noble/" + memberPath
	for _, failure := range []string{"crc", "truncated", "padding_hash", "padding_size"} {
		t.Run(failure, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			ctx := context.Background()
			original := gzipBytes(fakePackagesStanzas(map[string]string{"pool/old.deb": strings.Repeat("a", 64)}))
			release, _ := makeRelease(map[string][]byte{memberPath: original})
			env.fetcher.put(memberURL, original)
			if err := env.adopter.Run(ctx, env.suite, release, "", ""); err != nil {
				t.Fatal(err)
			}
			before, err := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
			if err != nil || before.CurrentSnapshotID == nil {
				t.Fatalf("original snapshot: %+v, %v", before, err)
			}
			next := gzipBytes(fakePackagesStanzas(map[string]string{"pool/new.deb": strings.Repeat("b", 64)}))
			wantErr := ErrAdoptionParseFailed
			switch failure {
			case "crc":
				next[len(next)-8] ^= 1
			case "truncated":
				next = next[:len(next)-1]
			}
			if failure != "truncated" {
				next = append(next, make([]byte, 3057)...)
			}
			release, _ = makeRelease(map[string][]byte{memberPath: next})
			switch failure {
			case "padding_hash":
				// Metadata authenticates different bytes of the SAME length.
				// The served bytes would decompress successfully if hashes were
				// checked only after discarding padding.
				declared := bytes.Clone(next)
				declared[len(declared)-1] = 1
				release, _ = makeRelease(map[string][]byte{memberPath: declared})
				wantErr = ErrAdoptionMemberMismatch
			case "padding_size":
				next = append(next, 0)
				wantErr = ErrAdoptionMemberFetchFailed
			}
			env.fetcher.put(memberURL, next)
			if err := env.adopter.Run(ctx, env.suite, release, "", ""); !errors.Is(err, wantErr) {
				t.Fatalf("Run: %v, want %v", err, wantErr)
			}
			after, err := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
			if err != nil || after.CurrentSnapshotID == nil || *after.CurrentSnapshotID != *before.CurrentSnapshotID {
				t.Fatalf("failed adoption changed snapshot: %+v, %v", after, err)
			}
			if _, err := env.cache.GetPackageHash(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost,
				"/ubuntu/pool/new.deb", *after.CurrentSnapshotID); !errors.Is(err, cache.ErrNotFound) {
				t.Fatalf("failed adoption vouched for new.deb: %v", err)
			}
		})
	}
}
