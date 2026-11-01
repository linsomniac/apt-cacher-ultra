package gpg

// Tests of the keyring loader's merge of a key read more than once (the
// re-review's V-R2C2): one entity per primary fingerprint, every copy's
// subkeys, identities and signatures in it.

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// TestLoadKeyring_MergesALaterCopysSubkey: the re-review's V-R2C2, as it
// measured it (kr_dedup_trusted, kr_dedup_onefile, kr_dedup_onefile_newfirst,
// kr3_s1_dedup_trusted, kr3_s1_dedup_onefile). The corpus' S signs with a
// subkey; "S without its subkey" is S's packets before it, an older export.
// The loader kept the first copy of a key and dropped the rest, so the copy
// without the subkey read first — in a file named first, first in one
// concatenated file, or in a directory read first — hid the subkey: the
// cache refused x3i_subkey_good, which every apt accepts (untrusted_signer;
// gpgv finds the keyblock holding the subkey), and adopted
// x3i_subkey_bad_plus_good, which the five gpgv-based apt refuse (S1 knew no
// subkey to check). In every layout and both orders the subkey is S's now:
// in the fingerprint set and the admin entry, for S2 (broad trust and a pin
// naming the subkey) and for S1.
func TestLoadKeyring_MergesALaterCopysSubkey(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(aptShapeDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	keys := rawKeyBlocks(t, read("keys.gpg"))
	if len(keys) != 4 || !bytes.Contains(keys[0], []byte("Apt-shape A")) || !bytes.Contains(keys[2], []byte("Apt-shape S")) {
		t.Fatalf("test premise: keys.gpg holds %d keys, want A, E, S and O", len(keys))
	}
	keyA, full := keys[0], keys[2]
	tags, packets := rawPacketList(t, full)
	var noSub []byte
	for i, pkt := range packets {
		if tags[i] == 14 {
			break
		}
		noSub = append(noSub, pkt...)
	}
	ents, err := openpgp.ReadKeyRing(bytes.NewReader(full))
	if err != nil || len(ents) != 1 || len(ents[0].Subkeys) != 1 || len(noSub) == len(full) {
		t.Fatalf("test premise: S holds one subkey (%d entities, %v)", len(ents), err)
	}
	sFP, subFP := upperFP(ents[0].PrimaryKey.Fingerprint), upperFP(ents[0].Subkeys[0].PublicKey.Fingerprint)
	pin := SignerPin{HostRegex: regexp.MustCompile(`^archive\.example\.com$`), Fingerprints: map[string]struct{}{subFP: {}}}
	for _, tc := range []struct {
		name  string
		files map[string][]byte // "1/" in the directory read first, "2/" in the second
	}{
		{"two files, the older copy named first (kr_dedup_trusted)", map[string][]byte{"1/10-s-nosub.gpg": noSub, "1/20-s-full.gpg": full}},
		{"two files, the newer copy named first", map[string][]byte{"1/10-s-full.gpg": full, "1/20-s-nosub.gpg": noSub}},
		{"one file, the older copy first (kr_dedup_onefile)", map[string][]byte{"1/s.gpg": joined(noSub, full)}},
		{"one file, the newer copy first (kr_dedup_onefile_newfirst)", map[string][]byte{"1/s.gpg": joined(full, noSub)}},
		{"two directories, the older copy in the first", map[string][]byte{"1/s.gpg": noSub, "2/s.gpg": full}},
		{"two directories, the newer copy in the first", map[string][]byte{"1/s.gpg": full, "2/s.gpg": noSub}},
		{"one armored file, the older copy's block first", map[string][]byte{"1/s.asc": joined(armorOf(t, openpgp.PublicKeyType, noSub), armorOf(t, openpgp.PublicKeyType, full))}},
	} {
		dirs := []string{makeTestDir(t), makeTestDir(t)}
		writeFile(t, filepath.Join(dirs[0], "00-a.gpg"), keyA)
		for name, body := range tc.files {
			writeFile(t, filepath.Join(dirs[name[0]-'1'], name[2:]), body)
		}
		keyring, err := LoadKeyring(dirs, silentLogger())
		if err != nil || keyring.Size() != 2 {
			t.Errorf("%s: LoadKeyring: %d keys, %v; want A and S, S once", tc.name, keyring.Size(), err)
			continue
		}
		if !keyring.HasFingerprint(subFP) {
			t.Errorf("%s: the subkey's fingerprint is not in the keyring", tc.name)
		}
		for _, en := range keyring.Entries() {
			if en.PrimaryFingerprint == sFP && !slices.Contains(en.SubkeyFingerprints, subFP) {
				t.Errorf("%s: S's entry lists subkeys %v, want the subkey", tc.name, en.SubkeyFingerprints)
			}
		}
		broad, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := broad.VerifyInline(context.Background(), newSuite(), read("x3i_subkey_good.InRelease")); err != nil {
			t.Errorf("%s: x3i_subkey_good: %v; want it adopted, as every apt", tc.name, err)
		}
		if _, err := broad.VerifyInline(context.Background(), newSuite(), read("x3i_subkey_bad_plus_good.InRelease")); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: x3i_subkey_bad_plus_good: %v; want ErrBadSignature (the subkey's, as gpgv's BADSIG)", tc.name, err)
		}
		pinned, err := NewVerifier(VerifierConfig{Keyring: keyring, Pins: []SignerPin{pin}, RequireSignature: true, RequirePinned: true, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pinned.VerifyInline(context.Background(), newSuite(), read("x3i_subkey_good.InRelease")); err != nil {
			t.Errorf("%s: x3i_subkey_good, pinned to the subkey: %v; want it adopted", tc.name, err)
		}
	}
}

// TestVerifier_ARevocationInEitherCopyCounts: a key read twice, one copy
// carrying its signing subkey's revocation (a vendor's newer export) and the
// other not (an older one left behind). The merged key holds the
// revocation whichever copy is read first, so an anchor signed by the
// revoked subkey is refused (ErrKeyRevoked), as sqv, which merges the
// certificates it is given, and gpg's import refuse it. Kept apart, a copy
// without the revocation read first still verified it.
func TestVerifier_ARevocationInEitherCopyCounts(t *testing.T) {
	x := newTestEntity(t, "X", "x@example.com")
	if err := x.AddSigningSubkey(&packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   2048,
		Time:      func() time.Time { return time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("AddSigningSubkey: %v", err)
	}
	sub := &x.Subkeys[len(x.Subkeys)-1]
	signed := clearsignWithConfig(t, &openpgp.Entity{PrivateKey: sub.PrivateKey}, []byte(fakeReleasePlaintext),
		&packet.Config{DefaultHash: crypto.SHA256, Time: time.Now})
	before := binaryKey(t, x)
	if err := x.RevokeSubkey(sub, packet.KeyCompromised, "compromised", &packet.Config{
		Time: func() time.Time { return time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC) },
	}); err != nil {
		t.Fatalf("RevokeSubkey: %v", err)
	}
	after := binaryKey(t, x)
	for _, tc := range []struct {
		name  string
		files map[string][]byte
		want  error // nil: adopted
	}{
		{"the copy without the revocation alone (control)", map[string][]byte{"x.gpg": before}, nil},
		{"the revoked copy alone (control)", map[string][]byte{"x.gpg": after}, pgperrors.ErrKeyRevoked},
		{"two files, the copy without the revocation first", map[string][]byte{"10-old.gpg": before, "20-new.gpg": after}, pgperrors.ErrKeyRevoked},
		{"two files, the revoked copy first", map[string][]byte{"10-new.gpg": after, "20-old.gpg": before}, pgperrors.ErrKeyRevoked},
		{"one file, the copy without the revocation first", map[string][]byte{"x.gpg": joined(before, after)}, pgperrors.ErrKeyRevoked},
		{"one file, the revoked copy first", map[string][]byte{"x.gpg": joined(after, before)}, pgperrors.ErrKeyRevoked},
	} {
		dir := makeTestDir(t)
		for name, body := range tc.files {
			writeFile(t, filepath.Join(dir, name), body)
		}
		keyring, err := LoadKeyring([]string{dir}, silentLogger())
		if err != nil || keyring.Size() != 1 {
			t.Errorf("%s: LoadKeyring: %d keys, %v; want X once", tc.name, keyring.Size(), err)
			continue
		}
		v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		_, err = v.VerifyInline(context.Background(), newSuite(), signed)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v; want it adopted", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v; want %v", tc.name, err, tc.want)
		}
	}
}

// TestMergeEntity_FoldsALaterCopyIn: mergeEntity, rule by rule, in both
// orders: the union of the copies' subkeys (by fingerprint), identities (by
// name) and every signature and revocation, a subkey's binding and an
// identity's or the key's own self-signature being the newest of the
// copies' (go-crypto's rule within one copy: of two made at once, the
// first copy's).
func TestMergeEntity_FoldsALaterCopyIn(t *testing.T) {
	at := func(day int) *packet.Signature {
		return &packet.Signature{CreationTime: time.Date(2025, 1, day, 0, 0, 0, 0, time.UTC)}
	}
	pk := &packet.PublicKey{Fingerprint: bytes.Repeat([]byte{1}, 20)}
	sk1 := &packet.PublicKey{Fingerprint: bytes.Repeat([]byte{2}, 20)}
	sk2 := &packet.PublicKey{Fingerprint: bytes.Repeat([]byte{3}, 20)}
	sk3 := &packet.PublicKey{Fingerprint: bytes.Repeat([]byte{4}, 20)}
	keyRev, dkOld, dkNew := at(9), at(1), at(2)
	uOld, uNew, uRev, vSelf := at(1), at(3), at(8), at(4)
	bindOld, bindNew, subRev, sk2Bind := at(1), at(5), at(7), at(6)
	tieOld, tieNew := at(2), at(2) // two bindings made at once
	older := func() *openpgp.Entity {
		return &openpgp.Entity{
			PrimaryKey:    pk,
			SelfSignature: dkOld,
			Signatures:    []*packet.Signature{dkOld},
			Identities: map[string]*openpgp.Identity{
				"U": {Name: "U", SelfSignature: uOld, Signatures: []*packet.Signature{uOld}},
			},
			Subkeys: []openpgp.Subkey{{PublicKey: sk1, Sig: bindOld}, {PublicKey: sk3, Sig: tieOld}},
		}
	}
	newer := func() *openpgp.Entity {
		return &openpgp.Entity{
			PrimaryKey:    pk,
			Revocations:   []*packet.Signature{keyRev},
			SelfSignature: dkNew,
			Signatures:    []*packet.Signature{dkNew},
			Identities: map[string]*openpgp.Identity{
				"U": {Name: "U", SelfSignature: uNew, Signatures: []*packet.Signature{uNew, uRev}, Revocations: []*packet.Signature{uRev}},
				"V": {Name: "V", SelfSignature: vSelf, Signatures: []*packet.Signature{vSelf}},
			},
			Subkeys: []openpgp.Subkey{{PublicKey: sk1, Sig: bindNew, Revocations: []*packet.Signature{subRev}}, {PublicKey: sk2, Sig: sk2Bind}, {PublicKey: sk3, Sig: tieNew}},
		}
	}
	for name, tc := range map[string]struct {
		copies [2]func() *openpgp.Entity
		tie    *packet.Signature // sk3's binding: the first copy's
	}{
		"the older copy first": {[2]func() *openpgp.Entity{older, newer}, tieOld},
		"the newer copy first": {[2]func() *openpgp.Entity{newer, older}, tieNew},
	} {
		copies := tc.copies
		e := copies[0]()
		mergeEntity(e, copies[1]())
		if !slices.Contains(e.Revocations, keyRev) {
			t.Errorf("%s: the key's revocations %v, want the newer copy's", name, e.Revocations)
		}
		if e.SelfSignature != dkNew || !slices.Contains(e.Signatures, dkOld) || !slices.Contains(e.Signatures, dkNew) {
			t.Errorf("%s: the key's self-signature %v of %v, want the newest of both copies'", name, e.SelfSignature, e.Signatures)
		}
		u, v := e.Identities["U"], e.Identities["V"]
		switch {
		case len(e.Identities) != 2 || u == nil || v == nil:
			t.Errorf("%s: identities %v, want U and V", name, e.Identities)
		case u.SelfSignature != uNew || v.SelfSignature != vSelf:
			t.Errorf("%s: U's self-signature %v, V's %v; want the newest", name, u.SelfSignature, v.SelfSignature)
		case !slices.Contains(u.Revocations, uRev) || !slices.Contains(u.Signatures, uOld) || !slices.Contains(u.Signatures, uNew):
			t.Errorf("%s: U's revocations %v, signatures %v; want both copies'", name, u.Revocations, u.Signatures)
		}
		if len(e.Subkeys) != 3 {
			t.Fatalf("%s: %d subkeys, want 3", name, len(e.Subkeys))
		}
		for _, s := range e.Subkeys {
			switch s.PublicKey {
			case sk1:
				if s.Sig != bindNew || !slices.Contains(s.Revocations, subRev) {
					t.Errorf("%s: subkey 1's binding %v, revocations %v; want the newest binding and the revocation", name, s.Sig, s.Revocations)
				}
			case sk3:
				if s.Sig != tc.tie {
					t.Errorf("%s: subkey 3's binding %p, want the first copy's (%p), made at the same time", name, s.Sig, tc.tie)
				}
			case sk2:
				if s.Sig != sk2Bind {
					t.Errorf("%s: subkey 2's binding %v, want its own", name, s.Sig)
				}
			default:
				t.Errorf("%s: a subkey %X neither copy holds", name, s.PublicKey.Fingerprint)
			}
		}
	}
}
