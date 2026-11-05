package gpg

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// The apt-shape table test (the hardening plan's Tasks 6, 7, 9b and 9e,
// docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md, DN-2 to
// DN-6, DN-11 and HD-19): the verifier, run on every shape of the committed
// corpus (testdata/aptshapes, e2e/apt-oracle/README.md), refuses a shape when
// any measured apt refuses it, the six current images' (verdicts.txt) or the
// five older ones' (verdicts-old.txt), and accepts a shape every measured apt
// accepts, but for the strict deviations listed below, each in its family,
// and the older images' refusals of crypto policy and of real signers'
// layouts (aptShapeOldExempt), which the cache accepts as the six do.
//
// AIDEV-NOTE: the corpus, verdicts.txt and classes.txt are one measurement
// (never edit a fixture by hand; e2e/apt-oracle/README.md, "Adding a
// shape"). A row this test fails is the cache disagreeing with apt: fix the
// verifier, or — only with a ruling — list the shape below in its family.
// Never force a row. No row is ever skipped: the shapes that once made
// go-crypto allocate gibibytes (vc2_*, the final review's V-C2) run here
// like every other, and TestVerifier_BoundsAllocationOnEveryRow fails any
// row that allocates more than a mebibyte.

// aptShapeColumns is the number of verdicts on each row of verdicts.txt, one
// per client image in DN-2's order (debian:11, ubuntu:22.04, ubuntu:24.04,
// debian:12, debian:13, ubuntu:26.04: COLUMNS in e2e/apt-oracle/run.sh).
// Another set of images is another set of columns: change it with them.
const aptShapeColumns = 6

// aptShapeOldColumns is the number of verdicts on each row of
// verdicts-old.txt, one per older client image (debian:10, ubuntu:20.04,
// ubuntu:18.04, ubuntu:16.04, ubuntu:14.04: OLD_COLUMNS in
// e2e/apt-oracle/run.sh; the hardening plan's Task 9e).
const aptShapeOldColumns = 5

// aptShapeDeviations are the strict deviations: shapes every measured apt
// accepts, the six current images and the five older ones, and the cache
// refuses, each with its family (DN-11: one reason a family, rulings HD-16
// and HD-17). Every one is an edit no surveyed signer makes; widening the
// grammar to accept one is a code change with its row. (Task 9e took three
// out, which gpgv 1.4 refuses: q_good_plus_bigunh_dup and the two
// v6_*_good_plus_v6unknown, ubuntu:16.04 and 14.04's NODATA.)
var aptShapeDeviations = map[string]string{
	// aptShapeOneArmorBlock (6)
	"a_d_twoblocks_same":        aptShapeOneArmorBlock,
	"a_twoblocks_same":          aptShapeOneArmorBlock,
	"xd_twoblocks_good_unknown": aptShapeOneArmorBlock,
	"xd_twoblocks_goodgood":     aptShapeOneArmorBlock,
	"xi_twoblocks_good_unknown": aptShapeOneArmorBlock,
	"xi_twoblocks_goodgood":     aptShapeOneArmorBlock,
	// aptShapeNonCanonicalArmor (74)
	"a_aftercrc_cr_sp":         aptShapeNonCanonicalArmor,
	"a_aftercrc_sp_tab_cr":     aptShapeNonCanonicalArmor,
	"a_b64_cr_mid":             aptShapeNonCanonicalArmor,
	"a_b64_cr_sp_line":         aptShapeNonCanonicalArmor,
	"a_b64_ff_line":            aptShapeNonCanonicalArmor,
	"a_b64_irregular":          aptShapeNonCanonicalArmor,
	"a_b64_lead_ff":            aptShapeNonCanonicalArmor,
	"a_b64_lead_tab":           aptShapeNonCanonicalArmor,
	"a_b64_sp_mid":             aptShapeNonCanonicalArmor,
	"a_b64_tab_mid":            aptShapeNonCanonicalArmor,
	"a_b64_width_100":          aptShapeNonCanonicalArmor,
	"a_b64_width_200":          aptShapeNonCanonicalArmor,
	"a_b64_width_80":           aptShapeNonCanonicalArmor,
	"a_b64_width_96":           aptShapeNonCanonicalArmor,
	"a_b64_width_97":           aptShapeNonCanonicalArmor,
	"a_crc_lead_ff":            aptShapeNonCanonicalArmor,
	"a_crc_lead_tab":           aptShapeNonCanonicalArmor,
	"a_crc_on_last_line":       aptShapeNonCanonicalArmor,
	"a_crc_trail_cr_sp":        aptShapeNonCanonicalArmor,
	"a_d_aftercrc_cr_sp":       aptShapeNonCanonicalArmor,
	"a_d_aftercrc_sp_tab_cr":   aptShapeNonCanonicalArmor,
	"a_d_b64_cr_mid":           aptShapeNonCanonicalArmor,
	"a_d_b64_sp_mid":           aptShapeNonCanonicalArmor,
	"a_d_b64_tab_mid":          aptShapeNonCanonicalArmor,
	"a_d_b64_width_97":         aptShapeNonCanonicalArmor,
	"a_d_hdr_cr_mid":           aptShapeNonCanonicalArmor,
	"a_d_hdr_trailing_space":   aptShapeNonCanonicalArmor,
	"a_d_hdr_vt_lead":          aptShapeNonCanonicalArmor,
	"a_d_sighdrterm_cr_sp":     aptShapeNonCanonicalArmor,
	"a_d_sighdrterm_ff":        aptShapeNonCanonicalArmor,
	"a_hdr_cr_mid":             aptShapeNonCanonicalArmor,
	"a_hdr_folded_tab":         aptShapeNonCanonicalArmor,
	"a_hdr_long_colon_late":    aptShapeNonCanonicalArmor,
	"a_hdr_nbsp_lead":          aptShapeNonCanonicalArmor,
	"a_hdr_nul_value":          aptShapeNonCanonicalArmor,
	"a_hdr_vt_lead":            aptShapeNonCanonicalArmor,
	"a_lf_cr_in_body":          aptShapeNonCanonicalArmor,
	"a_sighdrterm_cr_sp":       aptShapeNonCanonicalArmor,
	"a_sighdrterm_ff":          aptShapeNonCanonicalArmor,
	"d_blank_in_base64":        aptShapeNonCanonicalArmor,
	"h_hdr_key_underscore":     aptShapeNonCanonicalArmor,
	"h_hdr_val_del":            aptShapeNonCanonicalArmor,
	"i_blank_in_base64":        aptShapeNonCanonicalArmor,
	"i_crc_lowercase_eq_ws":    aptShapeNonCanonicalArmor,
	"l_hdr_17":                 aptShapeNonCanonicalArmor,
	"l_hdr_key_65":             aptShapeNonCanonicalArmor,
	"l_hdr_val_1025":           aptShapeNonCanonicalArmor,
	"n_clearhdr_nul":           aptShapeNonCanonicalArmor,
	"q_b64_ff_mid":             aptShapeNonCanonicalArmor,
	"q_d_b64_lead_ff":          aptShapeNonCanonicalArmor,
	"q_sighdrterm_vt":          aptShapeNonCanonicalArmor,
	"s_d_hdr_colon_space_only": aptShapeNonCanonicalArmor,
	"s_d_hdr_cr_in_key":        aptShapeNonCanonicalArmor,
	"s_d_hdr_invalid_utf8_key": aptShapeNonCanonicalArmor,
	"s_d_hdr_lead_tab":         aptShapeNonCanonicalArmor,
	"s_d_hdr_nonascii_key":     aptShapeNonCanonicalArmor,
	"s_d_hdr_value_trail_vt":   aptShapeNonCanonicalArmor,
	"s_hdr_cr_in_key":          aptShapeNonCanonicalArmor,
	"s_hdr_invalid_utf8_key":   aptShapeNonCanonicalArmor,
	"s_hdr_lead_tab":           aptShapeNonCanonicalArmor,
	"s_hdr_nonascii_key":       aptShapeNonCanonicalArmor,
	"s_hdr_value_trail_vt":     aptShapeNonCanonicalArmor,
	"w_d_b64_cr_sp":            aptShapeNonCanonicalArmor,
	"w_d_crc_cr_sp":            aptShapeNonCanonicalArmor,
	"w_d_term_cr_sp":           aptShapeNonCanonicalArmor,
	"x3d_armor_no_blank":       aptShapeNonCanonicalArmor,
	"x3i_armor_no_blank":       aptShapeNonCanonicalArmor,
	"x4i_b64_leading_space":    aptShapeNonCanonicalArmor,
	"x4i_hdr_leading_space":    aptShapeNonCanonicalArmor,
	"x5i_hdr_ctrl":             aptShapeNonCanonicalArmor,
	"x5i_hdr_no_key":           aptShapeNonCanonicalArmor,
	"x5i_hdr_space_in_key":     aptShapeNonCanonicalArmor,
	"xd_blank_after_crc":       aptShapeNonCanonicalArmor,
	"xi_blank_after_crc":       aptShapeNonCanonicalArmor,
	// aptShapeNonCanonicalPacket (40)
	"gd_p1_inner_marker":                          aptShapeNonCanonicalPacket,
	"gd_p2_inner_unknowntag":                      aptShapeNonCanonicalPacket,
	"gd_p3_inner_zero_bytes":                      aptShapeNonCanonicalPacket,
	"gi_p1_inner_marker":                          aptShapeNonCanonicalPacket,
	"gi_p2_inner_unknowntag":                      aptShapeNonCanonicalPacket,
	"gi_p3_inner_zero_bytes":                      aptShapeNonCanonicalPacket,
	"k_good_plus_unknownB_hashed_crit12":          aptShapeNonCanonicalPacket,
	"k_good_plus_unknownB_hashed_crit7":           aptShapeNonCanonicalPacket,
	"k_good_plus_unknownB_nocreated":              aptShapeNonCanonicalPacket,
	"k_good_plus_unknownB_type13":                 aptShapeNonCanonicalPacket,
	"k_unknownB_hashed_crit7_first":               aptShapeNonCanonicalPacket,
	"l_pkts_9":                                    aptShapeNonCanonicalPacket,
	"p_good_unh_16_dup":                           aptShapeNonCanonicalPacket,
	"p_good_unh_16_fiveoctet_len":                 aptShapeNonCanonicalPacket,
	"p_good_unh_crit_issuer16":                    aptShapeNonCanonicalPacket,
	"p_good_unh_fpr_v0form":                       aptShapeNonCanonicalPacket,
	"p_good_unh_fpr_v3form":                       aptShapeNonCanonicalPacket,
	"p_good_unh_fpr_v4_dup":                       aptShapeNonCanonicalPacket,
	"q10_d_good_plus_unknownB_hashed_t4_empty":    aptShapeNonCanonicalPacket,
	"q10_good_plus_unknownB_hashed_crit_t4_empty": aptShapeNonCanonicalPacket,
	"q10_good_plus_unknownB_hashed_t4_empty":      aptShapeNonCanonicalPacket,
	"q10_unknownB_hashed_t4_empty_plus_good":      aptShapeNonCanonicalPacket,
	"q9_unh_33_v5form_5":                          aptShapeNonCanonicalPacket,
	"q9_unh_33_v6form_33":                         aptShapeNonCanonicalPacket,
	"q_good_plus_unknownB_critunh":                aptShapeNonCanonicalPacket,
	"q_good_unh_16A_16B":                          aptShapeNonCanonicalPacket,
	"q_good_unh_16A_33A":                          aptShapeNonCanonicalPacket,
	"q_good_unh_33B_16A":                          aptShapeNonCanonicalPacket,
	"q_unknownB_critunh_plus_good":                aptShapeNonCanonicalPacket,
	"r_dup_mpi_low_plus_good":                     aptShapeNonCanonicalPacket,
	"r_good_plus_dup_mpi_low":                     aptShapeNonCanonicalPacket,
	"sx_d_straddle_unknownB":                      aptShapeNonCanonicalPacket,
	"sx_i_straddle_unknownB":                      aptShapeNonCanonicalPacket,
	"t7g3b_hashed16B_16A":                         aptShapeNonCanonicalPacket,
	"t7g7b_33junk_33A":                            aptShapeNonCanonicalPacket,
	"v4_i_ed25519unknown_plus_good":               aptShapeNonCanonicalPacket,
	"v4_i_ed448unknown_plus_good":                 aptShapeNonCanonicalPacket,
	"v_good_plus_unknownB_hashed_regexp_nonul_go": aptShapeNonCanonicalPacket,
	"x3i_goodsig_unhashed_fpr_added":              aptShapeNonCanonicalPacket,
	"z_good_unh_embedded_sig":                     aptShapeNonCanonicalPacket,
	// aptShapeUnverifiableAlgorithm (7)
	"e8_good_plus_badA_sha3":        aptShapeUnverifiableAlgorithm,
	"e8_good_then_sha224A":          aptShapeUnverifiableAlgorithm,
	"e8_good_then_sha3A":            aptShapeUnverifiableAlgorithm,
	"e8_sha224A_then_good":          aptShapeUnverifiableAlgorithm,
	"k_good_plus_unknownB_sha224":   aptShapeUnverifiableAlgorithm,
	"k_good_plus_unknownB_sha3_256": aptShapeUnverifiableAlgorithm,
	"xi_good_plus_unknownpk_A":      aptShapeUnverifiableAlgorithm,
}

// The deviation families (DN-11). TestAptShapeDeviations_EveryFamilyHasARow
// requires a row in each.
const (
	aptShapeOneArmorBlock         = "one armor block: the cache reads one block (layer T), apt reads every one; no surveyed vendor publishes two (2026-10-28)"
	aptShapeNonCanonicalArmor     = "non-canonical armor layout: outside layer A's whitelist (armor_canon.go), or a NUL byte in the envelope's headers (layer T's T5: a_hdr_nul_value, n_clearhdr_nul), which gpgv and sqv both accept and every surveyed signer writes; the edit no signer makes, the cache refuses even where every apt reads it"
	aptShapeNonCanonicalPacket    = "non-canonical packet layout: outside layer P's v4 grammar (sigpacket.go: exact consumption, one issuer, minimal MPIs, bounded areas, subpackets go-crypto can parse), which every surveyed signer writes; the cache refuses even where every apt reads it"
	aptShapeUnverifiableAlgorithm = "unverifiable algorithm: a public-key or hash algorithm layer P does not read, so the packet cannot be told from garbage; a vendor adding one is refused until the corpus measures it and the cache reads it"
)

// aptShapeFamilies lists the families, for the guards below.
var aptShapeFamilies = []string{aptShapeOneArmorBlock, aptShapeNonCanonicalArmor, aptShapeNonCanonicalPacket, aptShapeUnverifiableAlgorithm}

// aptShapeKnownUnsafe would hold shapes every measured apt refuses and the
// cache accepts. It is empty (ruling HD-18: x3i_sha1_only, DN-6's one entry,
// is refused now, as S2 never counts a SHA-1 signature), and
// TestAptShapeKnownUnsafe_IsEmpty keeps it so: adding to it needs a ruling,
// not an edit.
var aptShapeKnownUnsafe = map[string]string{}

// aptShapeOldExempt are the rows an older image refuses and the six current
// images all accept that the cache accepts all the same (the hardening
// plan's Task 9e, rulings HD-19 and E2), each with its class and reason: (a)
// crypto policy, which is not intersected across client generations, or (b)
// a layout a real signer emits (a surveyed vendor's anchor or a vendor
// fixture has it), which an older apt's refusal never makes the cache refuse.
// Every other row an older image refuses, the cache refuses (class (c): an
// attacker's edit, folded into layers T, A or P).
// TestAptShapeOldExempt_AreOlderImagesRefusals guards the list.
var aptShapeOldExempt = map[string]string{
	// aptShapeOldECC (14)
	"pk_ed25519":                                  aptShapeOldECC,
	"pk_ed25519_on_ed25519":                       aptShapeOldECC,
	"pk_ed25519_on_nistp256":                      aptShapeOldECC,
	"pk_ed25519_on_nistp384":                      aptShapeOldECC,
	"pk_ed25519_on_nistp521":                      aptShapeOldECC,
	"pk_ed25519_on_rsa2048":                       aptShapeOldECC,
	"pk_ed25519_on_rsa3072":                       aptShapeOldECC,
	"pk_ed25519_plus_bp256":                       aptShapeOldECC,
	"pk_ed25519_plus_ed25519_on_ed25519_sha1bind": aptShapeOldECC,
	"pk_ed25519_plus_ed25519_on_rsa1024":          aptShapeOldECC,
	"pk_ed25519_plus_nistp521":                    aptShapeOldECC,
	"pk_ed25519_plus_secp256k1":                   aptShapeOldECC,
	"pk_nistp256":                                 aptShapeOldECC,
	"pk_nistp384":                                 aptShapeOldECC,
	// aptShapeOldCRAfterMarker (3)
	"a_text_crlf_sig_lf": aptShapeOldCRAfterMarker,
	"i_crlf_all":         aptShapeOldCRAfterMarker,
	"n_i_nginx_crlf":     aptShapeOldCRAfterMarker,
	// aptShapeOldNoCRCUnpadded (2)
	"d_crc_missing": aptShapeOldNoCRCUnpadded,
	"i_crc_missing": aptShapeOldNoCRCUnpadded,
	// aptShapeOldTwoUnknownKeys (1)
	"us_good_plus_two_unknown": aptShapeOldTwoUnknownKeys,
}

// The classes of aptShapeOldExempt (the hardening plan's Task 9e): "(a) "
// crypto policy, "(b) " a real signer's layout, each one reason a family.
// TestAptShapeOldExempt_AreOlderImagesRefusals requires a row in each, and
// holds each row to its class's measured refusal (aptShapeOldRefusals).
// (t7g5_s_primary, once listed pending a ruling, is folded: S1 checks every
// known key whatever its usage, Task 9e's fix round.)
const (
	aptShapeOldECC            = "(a) crypto policy: an EdDSA or ECDSA signing key, which gpgv 1.4 (ubuntu:16.04, ubuntu:14.04) cannot use (\"can't handle public key algorithm\"); not intersected (HD-19: the survey's yarn signs with EdDSA)"
	aptShapeOldCRAfterMarker  = "(b) a real signer's layout: a CR after the clearsign marker on line 1, nginx's CRLF InRelease (PE/vendor/nginx, testdata/vendors/nginx), which apt 1.2 (ubuntu:16.04) reads as not clearsigned (\"Clearsigned file isn't valid, got 'NOSPLIT'\")"
	aptShapeOldNoCRCUnpadded  = "(b) a real signer's layout: no CRC line, the base64 happening to need no padding (its packets' length a multiple of 3): syncthing's InRelease (PE/vendor/syncthing, OpenPGP.js), and any CRC-less signer's (ms-*, signal, tailscale: RFC 9580 drops the CRC) whenever its length lands there; gpgv 1.4 (ubuntu:16.04, ubuntu:14.04) then reads on into the END line (\"invalid radix64 character\", NODATA)"
	aptShapeOldTwoUnknownKeys = "(b) a real signer's layout: a good signature beside two by keys the client lacks, debian-buster-archive's InRelease and Release.gpg (PE/vendor), which apt 1.2 (ubuntu:16.04) refuses (\"Invalid message from method gpgv: NO_PUBKEY ...\"); the trigger is the client's keyring, which no byte layout tells"
)

// aptShapeOldClasses lists aptShapeOldExempt's classes, for its guard.
var aptShapeOldClasses = []string{aptShapeOldECC, aptShapeOldCRAfterMarker, aptShapeOldNoCRCUnpadded, aptShapeOldTwoUnknownKeys}

// aptShapeOldRefusal is a class's measured refusal: the older columns that
// refuse each of its rows (verdicts-old.txt's, from 0), every other one
// accepting, and the error class each refusal has in classes-old.txt.
type aptShapeOldRefusal struct {
	cols  []int
	class string
}

// aptShapeOldRefusals pins each aptShapeOldExempt class to its measurement
// (the Task 9e review's 9e-M4), so that an older apt refusing such a row
// for another reason, or another image refusing it, shows.
var aptShapeOldRefusals = map[string]aptShapeOldRefusal{
	aptShapeOldECC:            {cols: []int{3, 4}, class: "other"},
	aptShapeOldCRAfterMarker:  {cols: []int{3}, class: "split"},
	aptShapeOldNoCRCUnpadded:  {cols: []int{3, 4}, class: "nodata"},
	aptShapeOldTwoUnknownKeys: {cols: []int{3}, class: "other"},
}

// aptShapeOldImages are verdicts-old.txt's columns in their order (OLD_COLUMNS
// in e2e/apt-oracle/run.sh), aptShapeImages verdicts.txt's: the tests that
// read a column by its index check them against the files' headers
// (TestAptShapeVerdicts_ColumnsAreTheImages).
var (
	aptShapeImages    = []string{"debian:11", "ubuntu:22.04", "ubuntu:24.04", "debian:12", "debian:13", "ubuntu:26.04"}
	aptShapeOldImages = []string{"debian:10", "ubuntu:20.04", "ubuntu:18.04", "ubuntu:16.04", "ubuntu:14.04"}
)

// aptShapeKeyPolicy are the rows only a newest-generation client's key
// policy refuses: debian:13's sqv, ubuntu:24.04's or ubuntu:26.04's
// APT::Key::Assert-Pubkey-Algo (batch fx20, the hardening plan's Task 9d).
// Crypto policy is not intersected across client generations (ruling
// HD-19): S2 counts what the gpgv-based apt count, so the cache accepts
// each, and reports no strong signer for it (strongSigner), which is what
// the adopter's signer downgrade guard reads. Every gpgv 2.2 image
// (debian:11, ubuntu:22.04, debian:12) accepts each, and some newest image
// refuses it (TestAptShapeKeyPolicy_AreTheNewestImagesRefusals).
var aptShapeKeyPolicy = map[string]bool{
	"pk_bp256":                       true,
	"pk_bp384":                       true,
	"pk_bp512":                       true,
	"pk_dsa2048":                     true,
	"pk_dsa2048_on_ed25519":          true,
	"pk_dsa3072":                     true,
	"pk_ed25519_on_bp256":            true,
	"pk_ed25519_on_dsa2048":          true,
	"pk_ed25519_on_ed25519_sha1bind": true,
	"pk_ed25519_on_ed25519_sha1self": true,
	"pk_ed25519_on_rsa1024":          true,
	"pk_ed25519_on_secp256k1":        true,
	"pk_nistp521":                    true,
	"pk_nistp521_on_ed25519":         true,
	"pk_rsa1024":                     true,
	"pk_rsa1024_on_ed25519":          true,
	"pk_rsa3072_rebound_minimal":     true,
	"pk_rsa3072_sha1bound":           true,
	"pk_rsa3072_sha224bound":         true,
	"pk_secp256k1":                   true,
}

// aptShapeStrongOverStrict are the rows every image accepts whose signer
// the cache calls not strong, each with why: strongSigner reads go-crypto's
// self-signature in use (the newest), never a second one, where debian:13's
// sqv takes the binding in force at the signature's time (strength.go's
// note). The cost: the downgrade guard stands down for such a current
// anchor, and could refuse such a candidate after a strong current anchor
// signed later than it.
var aptShapeStrongOverStrict = map[string]string{
	"pk_rsa3072_rebound_both": "its key file keeps the self-signature in force at the packet's time (sqv's) and a newer one (go-crypto's), made after the packet",
}

// aptShapeNewest are the columns of the newest-generation images, whose
// policies define a strong signer (ruling HD-19): ubuntu:24.04, debian:13
// and ubuntu:26.04; aptShapeGPGV22 those of the gpgv 2.2 images.
var (
	aptShapeNewest = []int{2, 4, 5}
	aptShapeGPGV22 = []int{0, 1, 3}
)

// anyRejectsIn reads a row of apt's verdicts in the given columns.
func anyRejectsIn(rejected []bool, cols []int) bool {
	for _, c := range cols {
		if rejected[c] {
			return true
		}
	}
	return false
}

// pendingS1 held the twelve shapes only S1 refuses (Appendix A's rule
// column marks them REJ(S1): a bad signature from a known key beside a good
// one) while the table test waited for S1. The hardening plan's Task 7 added
// S1 (requireKnownSignaturesVerify) and emptied it, so every row runs; a
// shape goes back in it only with a ruling, like the exception maps above.
// (Task 9b deleted its twin pendingHybrid, emptied by the hybrid verifier.)
var pendingS1 = map[string]bool{}

const aptShapeDir = "testdata/aptshapes"

// aptShape is one corpus shape: an InRelease (inline) or a Release.gpg
// (detached), with the Release it signs.
type aptShape struct {
	name     string
	inline   bool
	body     []byte // the InRelease, or the Release.gpg
	release  []byte // detached: the shape's own <name>.Release, else the shared Release
	keys     []byte // the shape's own signed-by, <name>.keys.asc (an armored keyring); nil: keys.gpg
	rejected []bool // apt's verdicts, one per column: true is REJ
	// oldRejected are the older images' verdicts (verdicts-old.txt), one
	// per aptShapeOldColumns column: true is REJ.
	oldRejected []bool
}

// loadAptShapeVerdicts parses verdicts.txt (its format: the AIDEV-NOTE in
// e2e/apt-oracle/run.sh): "#" lines are comments, every other line is a
// shape's name and aptShapeColumns verdicts, ok or REJ, single spaces apart.
func loadAptShapeVerdicts(t *testing.T) map[string][]bool {
	t.Helper()
	return loadVerdictFile(t, "verdicts.txt", aptShapeColumns)
}

// loadAptShapeOldVerdicts parses verdicts-old.txt, the older images'
// verdicts, in verdicts.txt's format with aptShapeOldColumns verdicts a row.
func loadAptShapeOldVerdicts(t *testing.T) map[string][]bool {
	t.Helper()
	return loadVerdictFile(t, "verdicts-old.txt", aptShapeOldColumns)
}

// loadVerdictFile parses the corpus's verdict file file, cols verdicts a row.
func loadVerdictFile(t *testing.T, file string, cols int) map[string][]bool {
	t.Helper()
	f, err := os.Open(filepath.Join(aptShapeDir, file))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := make(map[string][]bool)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) != 1+cols {
			t.Fatalf("%s:%d: %q: want a name and %d verdicts", file, n, line, cols)
		}
		name := fields[0]
		if _, dup := out[name]; dup {
			t.Fatalf("%s:%d: %s has two rows", file, n, name)
		}
		rejected := make([]bool, cols)
		for i, v := range fields[1:] {
			switch v {
			case "ok":
			case "REJ":
				rejected[i] = true
			default:
				t.Fatalf("%s:%d: %s: verdict %q is neither ok nor REJ", file, n, name, v)
			}
		}
		out[name] = rejected
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no row", file)
	}
	return out
}

// loadAptShapes reads the corpus: every <name>.InRelease and
// <name>.Release.gpg (with <name>.Release and <name>.keys.asc when
// present), each with its rows of verdicts.txt and verdicts-old.txt. A shape
// without a row, a row without a shape, or a file the corpus does not define
// fails the test.
func loadAptShapes(t *testing.T) []aptShape {
	t.Helper()
	verdicts := loadAptShapeVerdicts(t)
	oldVerdicts := loadAptShapeOldVerdicts(t)
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(aptShapeDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	shared := read("Release")
	entries, err := os.ReadDir(aptShapeDir)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*aptShape)
	ownRelease := make(map[string][]byte)
	ownKeys := make(map[string][]byte)
	for _, e := range entries {
		file := e.Name()
		switch {
		case file == "Release" || file == "keys.gpg" || file == "verdicts.txt" || file == "classes.txt" ||
			file == "verdicts-old.txt" || file == "classes-old.txt":
			continue
		case e.IsDir():
			t.Fatalf("%s: unexpected directory in the corpus", file)
		case strings.HasSuffix(file, ".InRelease"), strings.HasSuffix(file, ".Release.gpg"):
			inline := strings.HasSuffix(file, ".InRelease")
			name := strings.TrimSuffix(strings.TrimSuffix(file, ".InRelease"), ".Release.gpg")
			if _, dup := byName[name]; dup {
				t.Fatalf("%s: two files for one shape", name)
			}
			byName[name] = &aptShape{name: name, inline: inline, body: read(file)}
		case strings.HasSuffix(file, ".Release"):
			ownRelease[strings.TrimSuffix(file, ".Release")] = read(file)
		case strings.HasSuffix(file, ".keys.asc"):
			ownKeys[strings.TrimSuffix(file, ".keys.asc")] = read(file)
		default:
			t.Fatalf("%s: a file the corpus does not define", file)
		}
	}
	for name, rel := range ownRelease {
		s, ok := byName[name]
		if !ok || s.inline {
			t.Fatalf("%s.Release: no detached shape %s signs it", name, name)
		}
		s.release = rel
	}
	for name, keys := range ownKeys {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("%s.keys.asc: no shape %s", name, name)
		}
		s.keys = keys
	}
	var out []aptShape
	for name, s := range byName {
		rejected, ok := verdicts[name]
		if !ok {
			t.Fatalf("%s: no row in verdicts.txt", name)
		}
		s.rejected = rejected
		if s.oldRejected, ok = oldVerdicts[name]; !ok {
			t.Fatalf("%s: no row in verdicts-old.txt", name)
		}
		if !s.inline && s.release == nil {
			s.release = shared
		}
		out = append(out, *s)
	}
	for name := range verdicts {
		if _, ok := byName[name]; !ok {
			t.Fatalf("verdicts.txt: %s has no shape file", name)
		}
	}
	for name := range oldVerdicts {
		if _, ok := byName[name]; !ok {
			t.Fatalf("verdicts-old.txt: %s has no shape file", name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// anyRejects and allReject read a row of apt's verdicts.
func anyRejects(rejected []bool) bool {
	for _, r := range rejected {
		if r {
			return true
		}
	}
	return false
}

func allReject(rejected []bool) bool {
	for _, r := range rejected {
		if !r {
			return false
		}
	}
	return true
}

// aptShapeVerifier is the verifier the table holds to apt: the host keyring
// is keys.gpg alone (A, E, S and O: the clients' signed-by; O signs batch
// fx13, e2e/apt-oracle/gen_shapes.py), with the defaults —
// require_signature = true, no [[trusted_signer]] pin, allow_short_keyid =
// true, no accept_any_signer: every key in it is trusted for every suite.
// With acceptAnySigner, the same keyring under accept_any_signer's bypass
// (TestVerifier_BypassRefusesEveryMalformedRow). keys.gpg is loaded as an
// embedded source, not with LoadKeyring on the corpus directory, which would
// also try every <name>.Release.gpg as a keyring and warn for each.
func aptShapeVerifier(t *testing.T, acceptAnySigner ...bool) *Verifier {
	t.Helper()
	keys, err := os.ReadFile(filepath.Join(aptShapeDir, "keys.gpg"))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := LoadKeyringWithEmbedded(nil, []EmbeddedSource{{Name: "keys.gpg", Data: keys}}, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if keyring.Size() != 4 {
		t.Fatalf("keys.gpg holds %d keys, want 4 (A, E, S and O)", keyring.Size())
	}
	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AllowShortKeyID:  true,
		AcceptAnySigner:  len(acceptAnySigner) == 1 && acceptAnySigner[0],
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// verifyShape runs v on s, in its form, and returns the error. A shape
// with its own keys.asc (its signed-by in the oracle) is run by a verifier
// like v whose host keyring is that file, read as LoadKeyring reads an
// armored key file (every block: the re-review's V-RC1).
func verifyShape(v *Verifier, s aptShape) error {
	v, err := shapeVerifier(v, s)
	if err != nil {
		return err
	}
	_, err = verifyShapeSigners(v, s)
	return err
}

// shapeVerifier is the verifier verifyShape runs s with: v, or, for a shape
// with its own keys.asc, a verifier like v (its clock too) whose host
// keyring is that file.
func shapeVerifier(v *Verifier, s aptShape) (*Verifier, error) {
	if s.keys == nil {
		return v, nil
	}
	keyring, err := LoadKeyringWithEmbedded(nil, []EmbeddedSource{{Name: s.name + ".keys.asc", Data: s.keys}}, silentLogger())
	if err != nil {
		return nil, err
	}
	own, err := NewVerifier(VerifierConfig{
		Keyring: keyring, Pins: v.pins, RequireSignature: true, RequirePinned: v.requirePinned,
		AllowShortKeyID: v.allowShortKeyID, AcceptAnySigner: v.acceptAnySigner, Logger: v.logger,
	})
	if err != nil {
		return nil, err
	}
	own.now = v.now
	return own, nil
}

// verifyShapeSigners runs v on s, in its form, and returns the signers the
// verifier reports (freshness.Signers) and the error.
func verifyShapeSigners(v *Verifier, s aptShape) (freshness.Signers, error) {
	if s.inline {
		_, signers, err := v.VerifyInlineSigners(context.Background(), newSuite(), s.body)
		return signers, err
	}
	_, signers, err := v.VerifyDetachedSigners(context.Background(), newSuite(), s.release, s.body)
	return signers, err
}

// TestVerifier_FollowsTheAptShapeTable holds the verifier to DN-3's rule over
// the corpus: rejected iff any measured apt rejects, the current images' or
// the older ones' (Task 9e, ruling HD-19), but for a strict deviation
// (rejected, every apt accepts) and an older image's exempt refusal
// (aptShapeOldExempt: accepted, as the six current images accept it). No row
// is skipped (pendingS1 is empty).
func TestVerifier_FollowsTheAptShapeTable(t *testing.T) {
	v := aptShapeVerifier(t)
	for _, s := range loadAptShapes(t) {
		t.Run(s.name, func(t *testing.T) {
			if pendingS1[s.name] {
				t.Skipf("%s: only S1 refuses it (the hardening plan's Task 7)", s.name)
			}
			wantRejected, why := anyRejects(s.rejected), "apt's verdicts"
			if !wantRejected && anyRejects(s.oldRejected) {
				wantRejected, why = true, "an older image's refusal, not exempt (HD-19)"
				if class, ok := aptShapeOldExempt[s.name]; ok {
					wantRejected, why = false, "an older image's refusal, exempt: "+class
				}
			}
			if family, ok := aptShapeDeviations[s.name]; ok {
				if anyRejects(s.rejected) || anyRejects(s.oldRejected) || family == "" {
					t.Fatalf("listed as a strict deviation (%q), but apt's verdicts are %v, the older images' %v: a deviation is a shape every apt accepts, in a family", family, s.rejected, s.oldRejected)
				}
				wantRejected, why = true, "a strict deviation: "+family
			}
			if reason, ok := aptShapeKnownUnsafe[s.name]; ok {
				if !allReject(s.rejected) || reason == "" {
					t.Fatalf("listed as known unsafe (reason %q), but apt's verdicts are %v: a known-unsafe shape is one every apt rejects, with a reason", reason, s.rejected)
				}
				wantRejected, why = false, "known unsafe: "+reason
			}
			if aptShapeKeyPolicy[s.name] {
				wantRejected, why = false, "a newest client's key policy (ruling HD-19: not intersected; no strong signer)"
			}
			err := verifyShape(v, s)
			switch {
			case wantRejected && err == nil:
				t.Errorf("accepted; want rejected (%s; apt REJ per column: %v, older images: %v)", why, s.rejected, s.oldRejected)
			case !wantRejected && err != nil:
				t.Errorf("rejected: %v (reason %s); want accepted (%s; apt REJ per column: %v, older images: %v)", err, ClassifyVerifyErr(err), why, s.rejected, s.oldRejected)
			case err != nil:
				t.Logf("rejected: %v (reason %s)", err, ClassifyVerifyErr(err))
			}
		})
	}
}

// aptShapeMeasuredAt is when verdicts.txt was measured, from its header
// ("Measured <date> <time> UTC"), a minute later (the header's time is cut
// to the minute): every corpus key was made before it, and the strong
// flag's dated rules are judged at it, so the flag follows the measurement
// whatever today's date (ruling G3).
func aptShapeMeasuredAt(t *testing.T) time.Time {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aptShapeDir, "verdicts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`Measured (\d{4}-\d\d-\d\d \d\d:\d\d) UTC`).FindSubmatch(b)
	if m == nil {
		t.Fatal("verdicts.txt: no \"Measured <date> <time> UTC\" in its header")
	}
	at, err := time.Parse("2006-01-02 15:04", string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return at.Add(time.Minute)
}

// TestVerifier_StrongSignerFollowsTheAptShapeTable holds the strong flag
// (freshness.Signers.Strong, strongSigner) to its definition over the
// corpus, at the measurement's time: for every row the cache accepts, it
// reports a strong signer exactly when debian:13, ubuntu:24.04 and
// ubuntu:26.04 all accept the row (the hardening plan's Task 9d), and a
// strong signer it reports is one of the signers it counted.
func TestVerifier_StrongSignerFollowsTheAptShapeTable(t *testing.T) {
	v := aptShapeVerifier(t)
	at := aptShapeMeasuredAt(t)
	v.now = func() time.Time { return at }
	strong, weak := 0, 0
	for _, s := range loadAptShapes(t) {
		sv, err := shapeVerifier(v, s)
		if err != nil {
			t.Fatalf("%s: its keyring: %v", s.name, err)
		}
		signers, err := verifyShapeSigners(sv, s)
		if err != nil {
			continue
		}
		want := !anyRejectsIn(s.rejected, aptShapeNewest)
		if why, ok := aptShapeStrongOverStrict[s.name]; ok {
			if !want || why == "" {
				t.Errorf("%s: listed as over-strict (%q), but apt REJ per column %v", s.name, why, s.rejected)
			}
			want = false
		}
		if got := signers.Strong != ""; got != want {
			t.Errorf("%s: strong signer %q (counted %v), want strong %v (apt REJ per column: %v)",
				s.name, signers.Strong, signers.Counted, want, s.rejected)
		}
		if signers.Strong != "" && (len(signers.Counted) == 0 || signers.Counted[len(signers.Counted)-1] != signers.Strong) {
			t.Errorf("%s: strong signer %q is not the last counted of %v", s.name, signers.Strong, signers.Counted)
		}
		if len(signers.Counted) == 0 {
			t.Errorf("%s: accepted with no counted signer", s.name)
		}
		if want {
			strong++
		} else {
			weak++
		}
	}
	if strong == 0 || weak < len(aptShapeKeyPolicy) {
		t.Fatalf("test premise: %d accepted rows strong, %d not; want both, and every key-policy row among the latter", strong, weak)
	}
}

// TestAptShapeKeyPolicy_AreTheNewestImagesRefusals: every aptShapeKeyPolicy
// row is accepted by the three gpgv 2.2 images and refused by one of the
// newest, every one of whose refusals is its verifier's (class other: no
// split, no bad signature), the older images' refusals too (the override
// accepts the row whatever verdicts-old.txt says, so it must hide no other
// refusal: the Task 9e review's 9e-M3), and every row so measured is listed.
func TestAptShapeKeyPolicy_AreTheNewestImagesRefusals(t *testing.T) {
	verdicts := loadAptShapeVerdicts(t)
	classes := loadAptShapeClasses(t)
	oldVerdicts := loadAptShapeOldVerdicts(t)
	oldClasses := loadAptShapeOldClasses(t)
	for name := range aptShapeKeyPolicy {
		rejected, ok := verdicts[name]
		if !ok {
			t.Errorf("%s: no row in verdicts.txt", name)
			continue
		}
		if anyRejectsIn(rejected, aptShapeGPGV22) || !anyRejectsIn(rejected, aptShapeNewest) {
			t.Errorf("%s: apt REJ per column %v; want a refusal by a newest image alone", name, rejected)
		}
		for i, c := range classes[name] {
			if rejected[i] && c != "other" {
				t.Errorf("%s, column %d: class %q, want other (a key policy)", name, i+1, c)
			}
		}
		oldRow, ok := oldClasses[name]
		if !ok || len(oldVerdicts[name]) != aptShapeOldColumns {
			t.Errorf("%s: no row in verdicts-old.txt or classes-old.txt", name)
			continue
		}
		for i, c := range oldRow {
			if oldVerdicts[name][i] && c != "other" {
				t.Errorf("%s, older column %d: class %q, want other (a key policy, or crypto policy the older verifier cannot use)", name, i+1, c)
			}
		}
	}
	for name, rejected := range verdicts {
		if strings.HasPrefix(name, "pk_") && anyRejects(rejected) && !aptShapeKeyPolicy[name] {
			t.Errorf("%s: a batch fx20 row some apt refuses, not in aptShapeKeyPolicy", name)
		}
	}
}

// TestAptShapeDeviations_NameShapesInTheTable: no stale entry — every shape
// the exception maps and pendingS1 name has a row in verdicts.txt, no shape
// is in two of them, each deviation's value is one of the families, and each
// known-unsafe reason is non-empty.
func TestAptShapeDeviations_NameShapesInTheTable(t *testing.T) {
	verdicts := loadAptShapeVerdicts(t)
	families := make(map[string]bool)
	for _, f := range aptShapeFamilies {
		families[f] = true
	}
	seen := make(map[string]string)
	check := func(set, name, reason string) {
		t.Helper()
		if _, ok := verdicts[name]; !ok {
			t.Errorf("%s: %s names no shape of verdicts.txt", set, name)
		}
		if reason == "" {
			t.Errorf("%s: %s has no reason", set, name)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%s is in both %s and %s", name, other, set)
		}
		seen[name] = set
	}
	for name, family := range aptShapeDeviations {
		check("aptShapeDeviations", name, family)
		if !families[family] {
			t.Errorf("aptShapeDeviations: %s: %q is none of the families", name, family)
		}
	}
	for name, reason := range aptShapeKnownUnsafe {
		check("aptShapeKnownUnsafe", name, reason)
	}
	for name := range aptShapeKeyPolicy {
		check("aptShapeKeyPolicy", name, "HD-19")
	}
	for name, class := range aptShapeOldExempt {
		check("aptShapeOldExempt", name, class)
	}
	for name, pending := range pendingS1 {
		if !pending {
			t.Errorf("pendingS1: %s is listed false: delete it instead", name)
		}
		check("pendingS1", name, "S1")
		if rejected, ok := verdicts[name]; ok && !anyRejects(rejected) {
			t.Errorf("pendingS1: every apt accepts %s, so no rule should refuse it", name)
		}
	}
}

// TestAptShapeDeviations_EveryFamilyHasARow: DN-11 records deviations as
// families, each with one reason; a family no row is in is a stale reason.
func TestAptShapeDeviations_EveryFamilyHasARow(t *testing.T) {
	rows := make(map[string]int)
	for _, family := range aptShapeDeviations {
		rows[family]++
	}
	for _, family := range aptShapeFamilies {
		if rows[family] == 0 {
			t.Errorf("no deviation in the family %q", family)
		}
	}
}

// TestAptShapeOldExempt_AreOlderImagesRefusals guards aptShapeOldExempt
// (Task 9e, ruling E2): every entry is in one of the classes, each class has
// an entry, and each row is accepted by all six current images and refused
// by an older one (an entry that is neither is stale); a crypto-policy row
// (a) is refused by the verifier, never by apt's text layer (class other).
// And every row an older image refuses while the six accept is either listed
// or refused by the cache (TestVerifier_FollowsTheAptShapeTable).
func TestAptShapeOldExempt_AreOlderImagesRefusals(t *testing.T) {
	verdicts := loadAptShapeVerdicts(t)
	oldVerdicts := loadAptShapeOldVerdicts(t)
	oldClasses := loadAptShapeOldClasses(t)
	known := make(map[string]int)
	for _, c := range aptShapeOldClasses {
		known[c] = 0
	}
	for name, class := range aptShapeOldExempt {
		n, ok := known[class]
		if !ok {
			t.Errorf("%s: %q is none of aptShapeOldExempt's classes", name, class)
			continue
		}
		known[class] = n + 1
		if anyRejects(verdicts[name]) || !anyRejects(oldVerdicts[name]) {
			t.Errorf("%s: apt REJ per column %v, older images %v; want accepted by the six, refused by an older image", name, verdicts[name], oldVerdicts[name])
		}
		if !strings.HasPrefix(class, "(a) ") && !strings.HasPrefix(class, "(b) ") {
			t.Errorf("%s: class %q is neither (a) nor (b)", name, class)
		}
		want, ok := aptShapeOldRefusals[class]
		row := oldClasses[name]
		if !ok || len(row) != aptShapeOldColumns || len(oldVerdicts[name]) != aptShapeOldColumns {
			t.Errorf("%s: no measured refusal pinned for its class, or no row in classes-old.txt", name)
			continue
		}
		refusing := make(map[int]bool)
		for _, c := range want.cols {
			refusing[c] = true
		}
		for i := range aptShapeOldColumns {
			switch {
			case refusing[i] && (!oldVerdicts[name][i] || row[i] != want.class):
				t.Errorf("%s, %s: %v (class %q); its class's measurement is a refusal of class %q", name, aptShapeOldImages[i], oldVerdicts[name][i], row[i], want.class)
			case !refusing[i] && oldVerdicts[name][i]:
				t.Errorf("%s, %s: refused (class %q); its class's measurement is that only %v refuse", name, aptShapeOldImages[i], row[i], want.cols)
			}
		}
	}
	for class, n := range known {
		if n == 0 {
			t.Errorf("no row in aptShapeOldExempt's class %q", class)
		}
	}
}

// The error classes of classes.txt (e2e/apt-oracle/README.md, "Error
// classes"): "-" where apt accepted the shape; where it refused it, split
// (apt's own checks of the file refused it before its verifier ran),
// nodata, badsig or other (what the verifier, gpgv or sqv, said).
var aptShapeClasses = map[string]bool{"split": true, "nodata": true, "badsig": true, "other": true}

// loadAptShapeClasses parses classes.txt, whose rows and columns are
// verdicts.txt's (its format: the AIDEV-NOTE in e2e/apt-oracle/run.sh):
// "#" lines are comments, every other line a shape's name and
// aptShapeColumns classes.
func loadAptShapeClasses(t *testing.T) map[string][]string {
	t.Helper()
	return loadClassFile(t, "classes.txt", aptShapeColumns)
}

// loadAptShapeOldClasses parses classes-old.txt, the older images' error
// classes, whose rows and columns are verdicts-old.txt's.
func loadAptShapeOldClasses(t *testing.T) map[string][]string {
	t.Helper()
	return loadClassFile(t, "classes-old.txt", aptShapeOldColumns)
}

// loadClassFile parses the corpus's class file file, cols classes a row.
func loadClassFile(t *testing.T, file string, cols int) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aptShapeDir, file))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]string)
	for n, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) != 1+cols {
			t.Fatalf("%s:%d: %q: want a name and %d classes", file, n+1, line, cols)
		}
		if _, dup := out[fields[0]]; dup {
			t.Fatalf("%s:%d: %s has two rows", file, n+1, fields[0])
		}
		out[fields[0]] = fields[1:]
	}
	return out
}

// TestAptShapeClasses_AgreeWithTheVerdicts: classes.txt names exactly
// verdicts.txt's shapes; a cell is "-" exactly where apt accepted the shape,
// else one of the classes; and a row is split in every column or in none —
// apt's text layer (LineBuffer, SplitClearSignedFile,
// VerifyDetachedSignatureFile) is the same in the six versions, so a split
// refusal is every image's (the hardening plan's DN-11, layer T;
// TestAptTextLayer_RefusesExactlyTheSplitRows holds the port of that layer
// to these rows).
func TestAptShapeClasses_AgreeWithTheVerdicts(t *testing.T) {
	verdicts := loadAptShapeVerdicts(t)
	classes := loadAptShapeClasses(t)
	for name := range verdicts {
		if _, ok := classes[name]; !ok {
			t.Errorf("%s: a row in verdicts.txt, none in classes.txt", name)
		}
	}
	for name, row := range classes {
		rejected, ok := verdicts[name]
		if !ok {
			t.Errorf("%s: a row in classes.txt, none in verdicts.txt", name)
			continue
		}
		split := 0
		for i, class := range row {
			switch {
			case !rejected[i] && class != "-":
				t.Errorf("%s, column %d: apt accepted it, but its class is %q", name, i+1, class)
			case rejected[i] && !aptShapeClasses[class]:
				t.Errorf("%s, column %d: apt refused it, but its class %q is none of split, nodata, badsig and other", name, i+1, class)
			case class == "split":
				split++
			}
		}
		if split != 0 && split != aptShapeColumns {
			t.Errorf("%s: split in %d of %d columns (%v): apt's text layer is the same in every image", name, split, aptShapeColumns, row)
		}
	}
}

// TestAptShapeOldClasses_AgreeWithTheVerdicts: classes-old.txt and
// verdicts-old.txt name exactly verdicts.txt's shapes; a cell is "-" exactly
// where the older image accepted the shape, else one of the classes. apt's
// text layer is not the same in the older images (apt 1.6 and before split
// otherwise: e2e/apt-oracle/README.md, "The older images"), but debian:10's
// and ubuntu:20.04's (apt 1.8 and 2.0) is the six current images': a row is
// split in those two columns exactly when it is split in classes.txt.
func TestAptShapeOldClasses_AgreeWithTheVerdicts(t *testing.T) {
	verdicts := loadAptShapeVerdicts(t)
	classes := loadAptShapeClasses(t)
	oldVerdicts := loadAptShapeOldVerdicts(t)
	oldClasses := loadAptShapeOldClasses(t)
	for name := range verdicts {
		if _, ok := oldVerdicts[name]; !ok {
			t.Errorf("%s: a row in verdicts.txt, none in verdicts-old.txt", name)
		}
		if _, ok := oldClasses[name]; !ok {
			t.Errorf("%s: a row in verdicts.txt, none in classes-old.txt", name)
		}
	}
	splits := 0
	for name, row := range oldClasses {
		rejected, ok := oldVerdicts[name]
		if !ok {
			t.Errorf("%s: a row in classes-old.txt, none in verdicts-old.txt", name)
			continue
		}
		if _, ok := verdicts[name]; !ok {
			t.Errorf("%s: a row in classes-old.txt, none in verdicts.txt", name)
			continue
		}
		for i, class := range row {
			switch {
			case !rejected[i] && class != "-":
				t.Errorf("%s, older column %d: apt accepted it, but its class is %q", name, i+1, class)
			case rejected[i] && !aptShapeClasses[class]:
				t.Errorf("%s, older column %d: apt refused it, but its class %q is none of split, nodata, badsig and other", name, i+1, class)
			}
		}
		cur, ok := classes[name]
		if !ok || len(cur) == 0 {
			t.Errorf("%s: a row in classes-old.txt, none in classes.txt", name)
			continue
		}
		split := cur[0] == "split"
		if split {
			splits++
		}
		for i := range 2 {
			if (row[i] == "split") != split {
				t.Errorf("%s, older column %d: class %q, classes.txt's %v: apt 1.8 and 2.0 split as the current images do", name, i+1, row[i], cur)
			}
		}
	}
	if splits < 60 {
		t.Fatalf("test premise: %d split rows, want the corpus's 64", splits)
	}
}

// aptShapeHeaderImages reads the images a verdict file's header names, in
// order: its "#   <image>  apt <version> ..." lines (run.sh --record writes
// one per column).
func aptShapeHeaderImages(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aptShapeDir, file))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^#   (\S+) +apt `).FindAllSubmatch(b, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// TestAptShapeVerdicts_ColumnsAreTheImages: verdicts.txt's and
// verdicts-old.txt's headers name the images in the order the tests index
// them (aptShapeImages, aptShapeOldImages: aptShapeNewest, the split check
// of debian:10 and ubuntu:20.04, aptShapeOldRefusals), so that a reordered
// COLUMNS or OLD_COLUMNS re-recorded fails here, not by pointing a check at
// another image (the Task 9e review's 9e-M8). classes.txt and
// classes-old.txt share their verdict file's columns (run.sh writes both).
func TestAptShapeVerdicts_ColumnsAreTheImages(t *testing.T) {
	for file, want := range map[string][]string{
		"verdicts.txt":     aptShapeImages,
		"verdicts-old.txt": aptShapeOldImages,
	} {
		if got := aptShapeHeaderImages(t, file); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: its header names the columns %v, the tests index %v", file, got, want)
		}
	}
	if len(aptShapeImages) != aptShapeColumns || len(aptShapeOldImages) != aptShapeOldColumns {
		t.Errorf("aptShapeImages or aptShapeOldImages: not one image per column")
	}
}

// TestCRC24_RFC4880CheckValue: A5's checksum is RFC 4880 §6.1's CRC-24,
// whose check value (the CRC of "123456789") is 0x21CF02.
func TestCRC24_RFC4880CheckValue(t *testing.T) {
	if got := crc24([]byte("123456789")); got != 0x21CF02 {
		t.Fatalf("crc24(\"123456789\") = %#06x, want 0x21cf02", got)
	}
}

// TestAptShapeKnownUnsafe_IsEmpty: the cache adopts no shape every apt
// refuses (ruling HD-18 emptied the map: x3i_sha1_only, the security
// review's C-3, is refused now). Adding to it needs a ruling, not an edit.
func TestAptShapeKnownUnsafe_IsEmpty(t *testing.T) {
	if len(aptShapeKnownUnsafe) != 0 {
		t.Fatalf("aptShapeKnownUnsafe = %v, want it empty (ruling HD-18)", aptShapeKnownUnsafe)
	}
}
