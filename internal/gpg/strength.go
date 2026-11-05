package gpg

import (
	"crypto"
	"fmt"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// The signer downgrade guard's policy (the hardening plan's Task 9d, ruling
// HD-19): which keys are strong. A strong signer is a packet S2 counts whose
// key every newest-generation client policy accepts — debian:13's sqv
// (/usr/share/apt/default-sequoia.config over Sequoia's StandardPolicy),
// and ubuntu:24.04's and ubuntu:26.04's APT::Key::Assert-Pubkey-Algo — so
// that strong ⇔ debian:13, ubuntu:24.04 and ubuntu:26.04 each accept an
// anchor signed by that key alone. The adopter refuses a new anchor that
// has lost the strong signer its suite's current anchor has
// (freshness.ErrAdoptionSignerDowngrade): the on-path strip of a vendor's
// strong signature beside a weak one (the verifier lens's V-RC2, sublime's
// real InRelease), which the cache, counting what the gpgv-based apt count,
// would otherwise adopt and newer clients refuse.
//
// AIDEV-NOTE: the table is the measurement, not the plan's draft. Batch
// fx20 of the apt-shape corpus (pk_*, e2e/apt-oracle/gen_shapes.py's
// POLICY_KEYS, measured 2026-11-05) signs the corpus Release with each
// class alone; strongRules' sources name its rows. Two findings differ from
// the plan's text: debian:13's sqv refuses brainpool and secp256k1 keys
// today ("No binding signature": its crypto backend verifies neither
// curve, whatever the config's brainpool date of 2028-02-01 says), so
// neither is strong; and sqv judges a signing subkey's primary key by the
// same algorithm policy (pk_ed25519_on_dsa2048, pk_ed25519_on_rsa1024,
// pk_ed25519_on_secp256k1, pk_ed25519_on_bp256 refused; ..._on_nistp521,
// _on_nistp256, _on_nistp384, _on_rsa2048, _on_rsa3072 accepted), while
// the gpgv-based apt judge the signing key alone (pk_nistp521_on_ed25519 is
// refused by ubuntu:24.04 and 26.04, pk_ed25519_on_nistp521 by none). sqv
// also judges a key's binding at the signature's time (strongSigner's
// note). The survey's anchors measured the same way in the three images
// (Task 9d): debian:13 refuses mongodb's, newrelic's, nvidia-cuda's,
// openresty's and raspberrypi's (SHA-1 self-signatures) and ceph's (a
// self-signature newer than the signature), and the cache calls exactly
// those not strong; the plan expected openresty alone. A
// class the corpus does not measure is never strong: Ed448 (gpg 2.4 makes
// it a v5 key, which layer P does not read), Ed25519 as algorithm 27, RSA
// sign-only (3), ElGamal and anything newer. Dates compare against the
// verifier's clock (Verifier.now), never an anchor's Date (ruling G3).
// Re-measure (bash e2e/apt-oracle/run.sh) when an image's apt, gpgv or sqv
// changes, and before each rule's date.

// strongRule is one row of strongRules: a key class (keyClass) and whether
// a key of it is strong as the signing key (signer: the three images) and
// acceptable as the primary key of a signing subkey (primary: debian:13's
// sqv, the only one that judges it), before until when until is set.
type strongRule struct {
	class   string
	signer  bool
	primary bool
	until   time.Time // the rule's strong verdicts hold before it; zero: no end
	source  string
}

// strongRules is the algorithm half of the policy, one row per key class
// (keyClass's names). A class without a row is not strong.
var strongRules = []strongRule{
	{class: "ed25519", signer: true, primary: true,
		source: "pk_ed25519, pk_ed25519_on_ed25519: ok in all six images; Assert-Pubkey-Algo lists ed25519"},
	{class: "nistp256", signer: true, primary: true,
		source: "pk_nistp256, pk_ed25519_on_nistp256: ok in all six; Assert-Pubkey-Algo lists nistp256"},
	{class: "nistp384", signer: true, primary: true,
		source: "pk_nistp384, pk_ed25519_on_nistp384: ok in all six; Assert-Pubkey-Algo lists nistp384"},
	{class: "nistp521", signer: false, primary: true,
		source: "pk_nistp521, pk_nistp521_on_ed25519: REJ ubuntu:24.04 and 26.04 (their Assert-Pubkey-Algo names \"nistp512\", where gpg says nistp521); pk_ed25519_on_nistp521 ok in all six"},
	{class: "rsa3072", signer: true, primary: true,
		source: "RSA of 3072 bits or more: pk_rsa3072, pk_rsa4096, pk_ed25519_on_rsa3072 ok in all six"},
	{class: "rsa2048", signer: true, primary: true, until: time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC),
		source: "RSA of 2048 to 3071 bits: pk_rsa2048, pk_ed25519_on_rsa2048 ok in all six; debian:13's default-sequoia.config: rsa2048 = 2030-02-01"},
	{class: "rsa1024", signer: false, primary: false,
		source: "RSA under 2048 bits: pk_rsa1024, pk_ed25519_on_rsa1024 REJ debian:13 (Sequoia's StandardPolicy), pk_rsa1024 REJ ubuntu:26.04 (Assert-Pubkey-Algo >=rsa2048)"},
	{class: "dsa", signer: false, primary: false,
		source: "pk_dsa2048, pk_dsa3072, pk_ed25519_on_dsa2048: REJ debian:13 (default-sequoia.config: dsa2048/3072/4096 = 2024-02-01); the signers REJ ubuntu:24.04 and 26.04 (Assert-Pubkey-Algo lists no DSA)"},
	{class: "brainpool", signer: false, primary: false,
		source: "pk_bp256, pk_bp384, pk_bp512, pk_ed25519_on_bp256: REJ debian:13 (sqv 1.3.0: \"No binding signature\"; the config's brainpool* = 2028-02-01 is moot)"},
	{class: "secp256k1", signer: false, primary: false,
		source: "pk_secp256k1, pk_ed25519_on_secp256k1: REJ debian:13 (\"No binding signature\")"},
}

// strongHash is one row of strongHashes: a hash algorithm a binding chain
// signature may use and still be strong, before until when until is set.
type strongHash struct {
	hash   crypto.Hash
	until  time.Time
	source string
}

// strongHashes is the binding half of the policy: the hashes of the
// signatures a key's binding chain rests on (bindingChain). A hash without
// a row is not strong.
var strongHashes = []strongHash{
	{hash: crypto.SHA256, source: "every fx20 key gpg binds with SHA-256 (pk_nistp256, pk_bp256, pk_dsa2048's, ok where the algorithm is)"},
	{hash: crypto.SHA384, source: "pk_nistp384: ok in all six"},
	{hash: crypto.SHA512, source: "pk_ed25519, pk_rsa3072 and every gpg-made subkey binding: ok in all six"},
	{hash: crypto.SHA1, until: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		source: "pk_rsa3072_sha1bound (self-signature), pk_ed25519_on_ed25519_sha1self (the primary's), pk_ed25519_on_ed25519_sha1bind (binding and back-signature): REJ debian:13 (\"not bound\"; default-sequoia.config: sha1.second_preimage_resistance = 2026-02-01)"},
	{hash: crypto.SHA224, until: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		source: "pk_rsa3072_sha224bound: REJ debian:13 (default-sequoia.config: sha224 = 2026-02-01)"},
}

// keyClass names pk's algorithm as strongRules does: RSA by its size band
// (Sequoia's: under 2048 bits, 2048 to 3071, 3072 or more), ECDSA by its
// curve, EdDSA (legacy, algorithm 22) on Ed25519, DSA. Any other key gets
// a name no rule has: its algorithm, and its curve when it has one.
func keyClass(pk *packet.PublicKey) string {
	switch pk.PubKeyAlgo {
	case packet.PubKeyAlgoRSA:
		bits, err := pk.BitLength()
		switch {
		case err != nil:
		case bits >= 3072:
			return "rsa3072"
		case bits >= 2048:
			return "rsa2048"
		default:
			return "rsa1024"
		}
	case packet.PubKeyAlgoDSA:
		return "dsa"
	case packet.PubKeyAlgoECDSA:
		switch curve, _ := pk.Curve(); curve {
		case packet.CurveNistP256:
			return "nistp256"
		case packet.CurveNistP384:
			return "nistp384"
		case packet.CurveNistP521:
			return "nistp521"
		case packet.CurveBrainpoolP256, packet.CurveBrainpoolP384, packet.CurveBrainpoolP512:
			return "brainpool"
		case packet.CurveSecP256k1:
			return "secp256k1"
		}
	case packet.PubKeyAlgoEdDSA:
		if curve, _ := pk.Curve(); curve == packet.Curve25519 {
			return "ed25519"
		}
	}
	curve, _ := pk.Curve()
	return fmt.Sprintf("algorithm %d %s", pk.PubKeyAlgo, curve)
}

// ruleFor is class's row of strongRules, and whether it has one.
func ruleFor(class string) (strongRule, bool) {
	for _, r := range strongRules {
		if r.class == class {
			return r, true
		}
	}
	return strongRule{}, false
}

// hashStrong reports whether h, a binding chain signature's hash, is strong
// at now (strongHashes).
func hashStrong(h crypto.Hash, now time.Time) bool {
	for _, r := range strongHashes {
		if r.hash == h {
			return r.until.IsZero() || now.Before(r.until)
		}
	}
	return false
}

// bindingChain is the signatures k's binding rests on, as go-crypto uses
// them to check a message signature by k (checkMessageSignatureDetails,
// openpgp/read.go, v1.5.2): the self-signature in use for the primary key
// (Entity.PrimarySelfSignature: the primary identity's, a version 6 key's
// direct-key signature), and for a subkey its binding (Key.SelfSignature,
// 0x18) and that binding's embedded back-signature (0x19). A nil entry is
// a link go-crypto would find missing.
//
// AIDEV-NOTE: this must be go-crypto's own notion of the signatures in use,
// never a second one that could disagree with the key reader's (keyfile.go:
// the reader decides which signatures go-crypto keeps; go-crypto picks among
// them). Re-check against checkMessageSignatureDetails when go-crypto is
// upgraded.
func bindingChain(k openpgp.Key) []*packet.Signature {
	self, _ := k.Entity.PrimarySelfSignature()
	chain := []*packet.Signature{self}
	if k.PublicKey != k.Entity.PrimaryKey {
		var back *packet.Signature
		if k.SelfSignature != nil {
			back = k.SelfSignature.EmbeddedSignature
		}
		chain = append(chain, k.SelfSignature, back)
	}
	return chain
}

// strongSigner reports whether k, the key that made a packet S2 counts at
// made (the packet's creation time), is strong at now (the verifier's
// clock), and when it is not, why: its algorithm (strongRules, as the
// signing key), its primary key's when k is a subkey (as a primary), and
// every signature of its binding chain (bindingChain): its hash
// (strongHashes), and its time, which must not be after made.
//
// AIDEV-NOTE: the time rule is debian:13's sqv, which judges a key's
// binding at the signature's time ("No binding signature at time T"):
// ceph's real anchors, signed 2026-03-12 by a key whose file holds only a
// self-signature of 2026-07-03, are refused there (the Task 9d vendor
// measurement), as is pk_rsa3072_rebound_minimal. sqv takes the binding
// in force at T among all of them; go-crypto keeps the newest, and this
// reads go-crypto's (no second notion of "in use"), so a key whose file
// also keeps an older valid self-signature made before the packet
// (pk_rsa3072_rebound_both, accepted by every image) is called not strong:
// over-strict, aptShapeStrongOverStrict.
func strongSigner(k openpgp.Key, made, now time.Time) (bool, string) {
	class := keyClass(k.PublicKey)
	r, ok := ruleFor(class)
	switch {
	case !ok:
		return false, fmt.Sprintf("its algorithm (%s) is not measured", class)
	case !r.signer:
		return false, fmt.Sprintf("its algorithm (%s) is refused: %s", class, r.source)
	case !r.until.IsZero() && !now.Before(r.until):
		return false, fmt.Sprintf("its algorithm (%s) is refused from %s: %s", class, r.until.Format(time.DateOnly), r.source)
	}
	if k.PublicKey != k.Entity.PrimaryKey {
		pclass := keyClass(k.Entity.PrimaryKey)
		p, ok := ruleFor(pclass)
		switch {
		case !ok:
			return false, fmt.Sprintf("its primary key's algorithm (%s) is not measured", pclass)
		case !p.primary:
			return false, fmt.Sprintf("its primary key's algorithm (%s) is refused: %s", pclass, p.source)
		case !p.until.IsZero() && !now.Before(p.until):
			return false, fmt.Sprintf("its primary key's algorithm (%s) is refused from %s: %s", pclass, p.until.Format(time.DateOnly), p.source)
		}
	}
	for _, s := range bindingChain(k) {
		switch {
		case s == nil:
			return false, "its binding chain lacks a signature"
		case !hashStrong(s.Hash, now):
			return false, fmt.Sprintf("its binding chain has a signature (type 0x%02x) hashed with %v", byte(s.SigType), s.Hash)
		case s.CreationTime.After(made):
			return false, fmt.Sprintf("its binding chain has a signature (type 0x%02x) made at %s, after the packet (%s)",
				byte(s.SigType), s.CreationTime.UTC().Format(time.RFC3339), made.UTC().Format(time.RFC3339))
		}
	}
	return true, ""
}
