package freshness

// Spec C5 "Hash blocks": one verifier for every member fetch — adoption,
// reconcile and the skipped-member repair all go through adoptMember —
// checks the bytes against every SHA-256 and SHA-512 the Release declares
// for the member, returns the SHA-256 of the verified bytes, and names
// the member's by-hash aliases for every declared algorithm.

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// memberWriter is the fetch destination of a member: the cache's
// BlobWriter (which hashes SHA-256 as it writes) with a SHA-512 hasher
// beside it, so a SHA-512 declaration is checked before the bytes reach
// pool/.
type memberWriter struct {
	*cache.BlobWriter
	sha512 hash.Hash
}

func newMemberWriter(c *cache.Cache) (*memberWriter, error) {
	w, err := c.NewTempBlob()
	if err != nil {
		return nil, err
	}
	return &memberWriter{BlobWriter: w, sha512: sha512.New()}, nil
}

// Write writes p to the temp blob and feeds both hashers.
func (w *memberWriter) Write(p []byte) (int, error) {
	n, err := w.BlobWriter.Write(p)
	w.sha512.Write(p[:n])
	return n, err
}

// Truncate restarts the blob and both hashers (a resumed fetch whose
// validator changed starts over; fetch.FetchDst).
func (w *memberWriter) Truncate() error {
	if err := w.BlobWriter.Truncate(); err != nil {
		return err
	}
	w.sha512.Reset()
	return nil
}

// memberHashMismatch is verifyMember's error for bytes whose hash is not
// the declared one: algo is "SHA256" or "SHA512".
type memberHashMismatch struct {
	algo, declared, actual string
}

func (e *memberHashMismatch) Error() string {
	return fmt.Sprintf("%s mismatch: declared %s, got %s", e.algo, e.declared, e.actual)
}

// verifyMember checks the bytes a fetch wrote into w against every hash m
// declares and moves them into pool/ (spec C5): the SHA-512 first, from
// the tee, so mismatched bytes never reach pool/; then the SHA-256 through
// FinalizeExpectingHash, or — for a SHA512-only member — Finalize. size is
// the length to check (0 skips it). It returns the SHA-256 of the
// verified bytes: the declared one, or for a SHA512-only member the
// derived one (controlling it needs a SHA-512 preimage). A hash mismatch
// is a *memberHashMismatch; the temp is removed on every failure.
//
// rehashPooled is the rehash on reuse (SPEC2 §7.5 step 5) for a
// SHA512-only member: called with the derived SHA-256 once the SHA-512
// matched and before Finalize, it must leave no pool/<derived> whose bytes
// are not <derived> (the adopter's rehashPooledBlob re-hashes, logs, counts
// and evicts). An error from it fails the member with the temp removed.
// Production callers always pass it; nil (tests only) skips the step.
//
// AIDEV-NOTE: never Finalize before every declared hash matched: Finalize
// renames the temp into pool/ under its own hash, so a forged member would
// otherwise sit in the pool as a stray blob. And never Finalize a
// SHA512-only member before rehashPooled ran: Finalize's dedup branch
// keeps an existing pool/<hash> unread and drops the verified temp, so a
// file corrupted at rest would be bound to the snapshot and its Packages
// would vouch the corrupt hashes (review fix round 1, Task 2). A member
// that declares a SHA-256 is re-hashed in the pool before its fetch
// (adoptMember), so it needs no hook. verifyMember itself never logs.
func verifyMember(w *memberWriter, m ReleaseMember, size int64, rehashPooled func(derived string) error) (string, error) {
	if m.SHA256 == "" && m.SHA512 == "" {
		_ = w.Abort()
		return "", fmt.Errorf("member %s declares no SHA256 or SHA512 hash", m.Path)
	}
	if m.SHA512 != "" {
		if got := hex.EncodeToString(w.sha512.Sum(nil)); got != m.SHA512 {
			_ = w.Abort()
			return "", &memberHashMismatch{algo: "SHA512", declared: m.SHA512, actual: got}
		}
	}
	if m.SHA256 == "" {
		if rehashPooled != nil {
			if err := rehashPooled(w.HashSum()); err != nil {
				_ = w.Abort()
				return "", err
			}
		}
		return w.Finalize(size)
	}
	got, err := w.FinalizeExpectingHash(m.SHA256, size)
	if errors.Is(err, cache.ErrHashMismatch) {
		return "", &memberHashMismatch{algo: "SHA256", declared: m.SHA256, actual: got}
	}
	return got, err
}

// hashFileDigests streams a pool file through SHA-256 and SHA-512 (the
// rehash-on-reuse defence for a member that declares both).
func hashFileDigests(p string) (sum256, sum512 string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	h256, h512 := sha256.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(h256, h512), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h512.Sum(nil)), nil
}

// byHashPath returns the by-hash path of a member for one algorithm
// ("SHA256" or "SHA512"): <dir>/by-hash/<algo>/<hex>, or by-hash/<algo>/<hex>
// for a member at the suite root (see byHashAliasPath's AIDEV-NOTE: apt
// requests root members by hash too).
func byHashPath(declaredPath, algo, hexHash string) string {
	dir := path.Dir(declaredPath)
	if dir == "." || dir == "" {
		return "by-hash/" + algo + "/" + hexHash
	}
	return dir + "/by-hash/" + algo + "/" + hexHash
}

// byHashAliases returns m's by-hash alias paths, one per algorithm its
// Release declares (spec C5: apt requests the strongest, and a client
// configured for another hash may ask for the other one).
func byHashAliases(m ReleaseMember) []string {
	var out []string
	if m.SHA256 != "" {
		out = append(out, byHashAliasPath(m.Path, m.SHA256))
	}
	if m.SHA512 != "" {
		out = append(out, byHashPath(m.Path, "SHA512", m.SHA512))
	}
	return out
}

// byHashProbePath is the by-hash URL path adoption probes for m: apt's
// rule, the strongest declared hash (spec §3: by-hash/SHA512 whenever the
// Release declares SHA512).
func byHashProbePath(m ReleaseMember) string {
	if m.SHA512 != "" {
		return byHashPath(m.Path, "SHA512", m.SHA512)
	}
	return byHashAliasPath(m.Path, m.SHA256)
}

// aliasRows returns m's by-hash alias rows, each bound to blob — the
// SHA-256 of m's verified bytes (verifyMember).
func aliasRows(snapshotID int64, m ReleaseMember, blob string) []cache.SnapshotMember {
	var out []cache.SnapshotMember
	for _, p := range byHashAliases(m) {
		out = append(out, cache.SnapshotMember{SnapshotID: snapshotID, Path: p, BlobHash: blob, DeclaredSHA256: blob})
	}
	return out
}

// memberRows returns a verified member's snapshot_member rows: its path
// and its alias rows (aliasRows).
func memberRows(snapshotID int64, m ReleaseMember, blob string) []cache.SnapshotMember {
	return append([]cache.SnapshotMember{{SnapshotID: snapshotID, Path: m.Path, BlobHash: blob, DeclaredSHA256: blob}},
		aliasRows(snapshotID, m, blob)...)
}

// verifiedMember is m once its bytes verified as blob: from here on its
// SHA256 is the verified bytes' hash, declared or derived — what the
// Packages/Sources/pdiff builders read the pool by.
func verifiedMember(m ReleaseMember, blob string) ReleaseMember {
	m.SHA256 = blob
	return m
}
