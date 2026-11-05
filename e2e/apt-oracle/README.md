# e2e/apt-oracle: apt's verdicts on the verifier's signature shapes

The cache adopts an upstream's `InRelease` or `Release.gpg` only if its
verifier (`internal/gpg`) accepts it, and the verifier should accept what
real apt accepts: an anchor some supported apt rejects, once adopted,
breaks those clients' `apt update` for the suite, and one every apt
accepts but the cache rejects keeps the suite on its previous snapshot.
This directory holds the corpus of signature shapes that comparison rests
on, and the oracle that measures apt on it:

- `gen_shapes.py` writes the corpus, `internal/gpg/testdata/aptshapes/`;
- `run.sh`, with `in-container.sh`, runs the real apt of six client images
  on every shape and compares its verdicts, and the error class of each
  refusal, with those the corpus records in `verdicts.txt` and
  `classes.txt`; with `--old`, the apt of five older client images, with
  `verdicts-old.txt` and `classes-old.txt` (*The older images*).

The oracle is manual, like `make e2e-live`, and never runs in CI.

## Running it

```sh
make e2e-apt-oracle                                    # all six images, compared
make e2e-apt-oracle-old                                # the five older images, compared
E2E_APT_ORACLE_IMAGES=debian:13 make e2e-apt-oracle    # some of them
E2E_APT_ORACLE_IMAGES=ubuntu:14.04 make e2e-apt-oracle-old
bash e2e/apt-oracle/run.sh --record                    # all six: rewrite verdicts.txt and classes.txt
bash e2e/apt-oracle/run.sh --old --record              # all five: rewrite verdicts-old.txt and classes-old.txt
```

It needs Docker and GNU `timeout`, and the images (pulled on first use;
the containers themselves run with `--network none`). The images of a set
run in parallel, one `apt-get update` per shape in each and a second one
for each shape apt refuses (*Error classes*); with the images cached a run
of the six takes about 85 seconds on a 12-core machine, and one of the five
older ones about 95. The two sets are two runs, and two make targets: one
after the other they take about three minutes (the hardening plan's Task
9e), and a release runs both (*When to run it*). To measure today's apt,
pull the images first: `docker run` never refreshes a cached tag, and the
version lines a run prints show which apt it measured. The eleven, as
`COLUMNS` and `OLD_COLUMNS` in `run.sh` name them:

```sh
for i in debian:11 ubuntu:22.04 ubuntu:24.04 debian:12 debian:13 ubuntu:26.04 \
         debian:10 ubuntu:20.04 ubuntu:18.04 ubuntu:16.04 ubuntu:14.04; do
  docker pull "$i" || exit 1
done
```

The exit status is 0 when apt gives the recorded verdicts and error
classes (or `--record` wrote them), 1 when apt changed, a container failed
or an error class and apt's words disagree, and 2 for a usage or setup
error: an image that is not a column of the set's file (`verdicts.txt`, or
`verdicts-old.txt` with `--old`), `--record` with only some of the set's
images, a corpus file missing, or the generator's round trip failing:
before any container starts, `gen_shapes.py --from-controls` on a copy of
the corpus (no key made, python3 only) must exit 0 and give it back byte
for byte.

## The corpus

`internal/gpg/testdata/aptshapes/` holds 988 files, about 1.3 MiB:

| File | What it is |
|---|---|
| `<name>.InRelease` | an inline shape (637 of them): the whole `InRelease`, its signed text included |
| `<name>.Release.gpg` | a detached shape (259): a `Release.gpg` that signs the shared `Release`, or its own |
| `<name>.Release` | a detached shape's own `Release` (4: `a_d_release_crlf`, `a_d_release_extra_nl`, `a_d_release_no_final_nl`, `o2_textmode_detached_release_crlf`), which `in-container.sh` serves in the shared one's place |
| `<name>.keys.asc` | a shape's own `signed-by` (82: batch fx16's `ka_*`, the keys A and E in one block each; fx19's `e7_*`, key K; fx20's `pk_*`, its keys; fx21's `us_*`, key U): an armored key file, which `in-container.sh` names in `keys.gpg`'s place and the table test loads as that shape's host keyring, as `LoadKeyring` reads a `.asc` file (every block) |
| `Release` | the shared `Release`, a flat repository's: the SHA256 line of an empty `Packages`, a fixed `Date:` in the past (2026-10-01), no `Valid-Until` |
| `keys.gpg` | the public keys A, E, S and O, in binary: the clients' `signed-by` here, and the host keyring of the verifier's table test (the hardening plan's Task 6), which loads the file whole, so every key in it is a trusted signer there |
| `verdicts.txt` | apt's verdicts: a `#` header naming the measurement's date and each image's apt and verifier versions, then one row per shape, `<name>` and one verdict per image in the order of *The images* below, `ok` (`apt-get update` exited 0) or `REJ` |
| `classes.txt` | apt's error class of each refusal, the same rows and columns: `-` where the verdict is `ok`, else `split`, `nodata`, `badsig` or `other` (*Error classes*) |
| `verdicts-old.txt`, `classes-old.txt` | the same for the five older images (*The older images*), in the same format, one column per older image |

The repository's `.gitattributes` marks every file here `-text`, so no
checkout or commit converts their line ends, whatever `core.autocrlf`
says: several shapes differ from their controls only in CRs.

There are 896 shapes, in twenty-two batches. The first 135 are those the
hardening plan measured on 2026-10-28
(`docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md`: DN-2
sorts them into classes, Appendix A gives each one's verdicts), rebuilt by
`gen_shapes.py` as the plan's throwaway generators built them, from one
key set. Most need no key: they are cut from a good signed `InRelease` or
`Release.gpg` (or, for a bad signature, A's signature of another text) by
byte surgery — lines before or after the armor, armor headers, CRC and
base64 lines changed, packets added beside the good signature, a
signature's unhashed area rewritten. A few needed the signer: a critical
notation, SHA-1, a dash-escaped END line in the signed text. A name says
the shape: `i_` and `d_` are the plan's first batch (inline and detached),
`xi_`, `xd_` and `x3i_` to `x7d_` its later ones, `gi_` and `gd_` batch
fx8, `t7c_` and `t7g` batch fx9, `a_` to `s_` batch fx10, `sx_` to `vm3_`
batch fx11, `n_` batches fx12 to fx14, `o_` and `o2_` batch fx13,
`c_text_`, `e_`, `e2_`, `g5_`, `h_`, `k_`, `l_`, `m_`, `u_`, `v_`,
`w_d_`, `x_`, `y_`, `z_` and `n_clearhdr_` batch fx15, `rd_`, `c_` and
`ka_` batch fx16, `sq_` batch fx17, `q6_`, `e8_`, `q9_` and `q10_`
batch fx18, `e7_` batch fx19, `pk_` batch fx20, `us_` batch fx21, `mh_`,
`cr_` and `mi_` batch fx22;
`i_trail_lf` is a blank line after the END line, `xd_good_plus_literal` a
literal-data packet beside a good detached signature, a `*_control*` its
batch's unmodified control.

Batch fx8's 20 (ruling HD-13) were found and measured in the plan's
Task 6, beyond Appendix A, and are cut without a key from the fx7
controls. In `g1` to `g4` a line inserted before the CRC line is one that
go-crypto's armor reader stops reading at once it has trimmed it
(`" -----END X-----"`, `"\v=AAAA"`, a tab before an END marker); `c1` and
`c2` are their controls, an ordinary junk line and an untrimmed
`-----END X-----`. In `p1` to `p3` the signature packet is framed again
with a marker, an unknown-tag packet or two zero bytes inside its declared
length, which gpgv and sqv skip and go-crypto reads as the next packet;
`c3` is the packet framed again with nothing added (the control's very
bytes). Every apt refuses `g1`, `g3` and `g4`, which the cache adopted
until that ruling; every apt accepts `p3`, which the cache refuses (a
strict deviation of the table test).

Batch fx9's 13 (ruling HD-14) were found and measured in the plan's
Task 7, beyond Appendix A, and are cut without a key from committed
fixtures: A's bad signature packet (`i_badsig_only`'s) with its issuer
subpackets rewritten, before A's good one (`x7i_control_good`'s), and in
`t7g5` S's subkey's bad packet (`x3i_subkey_bad_plus_good`'s) naming S's
primary key, which only certifies. They ask which issuer subpacket gpgv
looks the key up by, where go-crypto takes the last one it parses: gpgv
reads the first hashed issuer fingerprint in its version's form (`t7g4`:
a v6-form one in a v4 signature is none; `t7g7`: a later one is not
read), else the first issuer key ID, hashed then unhashed (`t7g1`,
`t7g3`, `t7g3b`); gpgv 2.2 also falls back to that key ID when the
fingerprint names a key it lacks (`t7g6`, `t7g6b`: debian:11,
ubuntu:22.04 and debian:12 refuse them, the other three images accept
them).
`t7c_*` are controls (`t7c_good_alone` is the control's very bytes).
Until that ruling the cache adopted `t7g1`, `t7g4`, `t7g6`, `t7g6b` and
`t7g7`, which every gpgv-based apt (for `t7g6` and `t7g6b`, every 2.2
one) refuses; every apt accepts `t7g7b`, which the cache refuses (a strict
deviation of the table test).

Batches fx10 to fx13 (322 shapes, the plan's Task 9a, its addendum's
DN-11) are the final review's shapes and DN-11's new controls; the
reviewers' generators, bytes and measurements are in the plan's evidence
(`PE/final-review/`, kept outside the tree), and every one of the 309
shapes they measured has the verdicts they measured:

- **fx10**, 274 shapes (`a_`, `p_`, `q_`, `r_`, `s_`): the adversarial
  lens's key-free shapes (its batches b1, b2, b5, b6 and b8), by its names,
  cut without a key from committed fixtures exactly as its generator cut
  them (byte for byte the bytes it measured): what follows a marker on its
  line, the cleartext header and its terminator, the signed text's
  dash-escapes and line ends, armor header lines, base64 and CRC lines
  (Unicode spaces, widths, padding mid-stream), unhashed subpackets
  (critical, oversized, issuers), framings and MPIs. Its own-key shapes
  are in fx13; its vendor shapes (b4) are not in the corpus: they are
  real vendors' anchors, with the vendors' keys.
- **fx11**, 30 (`sx_`, `v6_`, `v4_`, `ht_`, `u_`, `vc2_`, `vm1_`, `vm3_`):
  the verifier lens's, cut without a key from committed fixtures as its
  probes cut them: a packet hidden in a re-framed packet's tail (V-C1), v6
  and EdDSA packets beside a good v4 one (V-C3; the probes' random salts
  and signature values are fixed filler bytes here, so that
  `--from-controls` rewrites the same bytes), a flipped hash tag, an
  unknown key's packet before a known key's bad one (V-I2), and shapes the
  lens built in Go or only described: a v6 packet claiming 4 GiB of hashed
  subpackets and a v4 packet whose embedded signature claims 1 GiB (V-C2),
  a v6 packet naming A only by an unhashed key ID (V-M1), an armor header
  holding the END marker (V-M3).
- **fx12**, 3 (`n_`): DN-11's new controls that need no key: a
  binary-mode packet from a key no client has, first in an `InRelease`
  before A's good text-mode one (`n_i_unknown00_first`: B's packet, its
  signature type set to 0x00); a detached armor in 76-character lines
  (the inline one is `x4i_b64_line_76`); A's inline signature packet as a
  detached `Release.gpg` (`n_d_textmode_twin`: gpg's clearsigning of
  `Release` hashes exactly what a text-mode signature of it hashes).
- **fx13**, 15 (`o_`, `o2_`, `n_`): the shapes that need a signer, made
  with key O: the adversarial lens's own-key batches, by its names (b3, a
  signed text's dash-escapes: an escaped `-x: y` un-escaped, an empty last
  line written `- `; b7, a binary-mode signature in an `InRelease` and
  text-mode `Release.gpg` files), and DN-11's new controls: an SHA-224 and
  an SHA3-256 signature, and nginx's layout — an `InRelease` with CRLF line
  ends throughout whose signed text ends without a newline
  (`n_i_nginx_crlf`), with its text-mode detached twin
  `o2_textmode_detached` (nginx's own `Release.gpg` is binary, which every
  apt refuses: `d_binary`). gpg 2.4 cannot sign with SHA3-256, so
  `n_i_sha3_256`'s packet is built by the generator and its hash signed by
  the temporary home's gpg-agent; the generator checks both its RSA
  signature and gpg's SHA-256 one over the same text against O's public
  key before writing it, and `debian:13`'s sqv accepts it.

Four of those measurements are new to the plan: `debian:13`'s sqv refuses
SHA-224 ("not considered secure"), every gpgv-based apt refuses SHA3-256
(`Unknown error executing apt-key`, on `ubuntu:26.04` `Unknown error
executing gpgv`; GnuPG 2.4 lists no SHA3) and the
binary-mode packet first (`n_i_unknown00_first`: gpgv then reports B's
missing key and no good signature from A), and every apt accepts an END
marker in an armor header (`vm3_*`).

Batch fx14, 8 shapes (`n_`, the plan's Task 9b), measures what the
hybrid verifier's rules rest on, cut without a key from committed
fixtures:

- text-mode and binary-mode packets in one anchor (A's good packet, B's,
  or A's good one with its signature type changed, so a bad one from A),
  inline and detached, the good one first or second: gpgv hashes the data
  as the first packet's type says and, when the types differ, checks the
  first packet only ("multiple signatures detected. Only the first will
  be checked", g10/mainproc.c), so every gpgv-based apt refuses the
  anchor when that first packet is not a good one (`n_i_badA00_then_good`,
  `n_d_badA01_then_good`: BADSIG; `n_d_unknown01_first`: NO_PUBKEY, as
  `n_i_unknown00_first`) and every apt accepts it when it is, whatever
  follows (`n_i_good_then_unknown00`, `n_i_good_then_badA00`,
  `n_d_good_then_badA01`);
- base64 whose last character before its padding has its unused bits set
  (`n_i_b64_pad_bits`, `n_d_b64_pad_bits`: `p_good_old4`'s armor and its
  detached twin): the same bytes to gpgv, refused by `debian:13`'s sqv
  ("DecodeError: Invalid last symbol").

Batches fx15 to fx17 (232 shapes) are the plan's Task 9b's fix round:
its re-review's shapes and its own probes, all cut without a key from
committed fixtures (`FROM_CONTROLS`):

- **fx15**, 193: the adversarial lens's re-review shapes (its batches
  rr1 to rr5, PE/final-review/A-report.txt), by its names, cut exactly
  as its generator cut them: a Release.gpg's trailing blanks and CRs
  line by line (`w_d_`), malformed subpackets in a good packet's
  unhashed area (`u_`, `z_`: `debian:13`'s sqv reads such a packet as
  unknown and refuses an anchor with no other good one, finding
  A-RC1), apt's erasure of a key's good signatures at its later SHA-1
  one (`e_`, `e2_`: the five gpgv-based apt refuse `e_good_then_sha1A`,
  finding A-RC2), mixed signature types (`m_`, `g5_`), M's trailing CRs
  (`c_text_`), armor header values (`h_`), the grammar's limits (`l_`),
  an unknown key's packet's edges (`k_`, `v_`), unhashed subpacket types
  sqv reads (`x_`), a malformed packet beside a good one (`y_`), and a
  NUL in a cleartext header (`n_clearhdr_`).
- **fx16**, 27: the verifier lens's (PE/final-review/V-report.txt,
  V-RM5): a Release.gpg's trailing blanks on its base64, CRC and header
  lines (`rd_`), a cleartext `Comment:` header and CRs at a signed
  line's end (`c_`; reconcile's regression shapes, V-RI1), and the
  controls and `i_expired_bad_plus_good` with a two-block `keys.asc` of
  their own, A then E and E then A (`ka_ae_`, `ka_ea_`; V-RC1): every
  apt reads every block, so each verdict is `keys.gpg`'s.
- **fx17**, 12 (`sq_`): Sequoia's subpacket rules the re-review did not
  measure, each subpacket appended to A's good packet's unhashed area
  (internal/gpg's `sqvSubpacketFault`, derived from sequoia-openpgp
  2.0.0, which `debian:13`'s sqv 1.3.0 is built with): booleans of value
  2, a revocation key without its class bit or of 23 octets, a
  notation name that is not UTF-8, a short signature-target digest,
  recipients of other versions, an empty approved-certifications list,
  a regular expression that is its NUL alone. Every verdict is the one
  the table predicts (`debian:13` refuses exactly the faults).

Batches fx18 and fx19 (93 shapes) are the plan's Task 9b's second fix
round: its second re-review's adversarial lens's shapes
(PE/final-review/A-report.txt, "Re-review 2"), by its names, every one
with the verdicts it measured:

- **fx18**, 76, cut without a key from committed fixtures exactly as
  its generator cut them (byte for byte the bytes it measured): sqv's
  subpacket rules at their boundaries, each type on both sides, in A's
  good packet's unhashed area (`q6_`; `debian:13` refuses exactly the
  faults); apt's erasure with hashes layer P does not read, key O's
  SHA-224 and SHA3-256 packets of fx13's `n_i_sha224` and
  `n_i_sha3_256` beside A's good one, and with a bad or unknown key's
  erasing packet (`e8_`); issuer fingerprints of v6 and v5 form in an
  unhashed area (`q9_`); and an exportable certification without its
  value octet in an unknown key's hashed area (`q10_`, finding A-RC3:
  go-crypto v1.4.1's parse panicked on it).
- **fx19**, 17 (`e7_`): its own-key batch rr7, made with key K: apt's
  erasure compares the signing (sub)key, so a SHA-1 signature by
  another subkey or by the primary after a good one erases nothing,
  one by the same (sub)key does (the five gpgv-based apt refuse).
  Each shape's `signed-by` is K's public key alone, its own
  `<name>.keys.asc`, as the re-review measured it.

Batch fx20, 42 shapes (`pk_`), is the plan's Task 9d's measurement for
the signer downgrade guard (ruling HD-19, `internal/gpg/strength.go`): the
shared `Release` clearsigned by a throwaway key of each class alone —
RSA-1024, 2048, 3072 and 4096, DSA-2048 and 3072, Ed25519, P-256, P-384,
P-521, brainpoolP256r1, P384r1 and P512r1, secp256k1, an RSA-3072 key whose
self-signature is SHA-1 and one whose is SHA-224, and two RSA-3072 keys
made on 2025-01-01, signing on 2025-06-01 and bound again on 2025-09-01
(`pk_rsa3072_rebound_minimal`, its key file holding the new self-signature
alone, as ceph's real key file does; `pk_rsa3072_rebound_both`, both) — and
by primary/subkey
pairs (`pk_<subkey>_on_<primary>`: an Ed25519 signing subkey under a weak
primary, a weak signing subkey under an Ed25519 primary, an SHA-1
self-signature or an SHA-1 subkey binding and back-signature), each with its
own key alone as its `keys.asc`; and by an Ed25519 key with each of nine
weak ones (`pk_ed25519_plus_<weak>`, both keys in its `keys.asc`), whose
strip is the weak key's own `pk_` shape. gpg 2.4 makes Ed448 only as a v5
key, which layer P does not read, so it is not measured. The measurement:
`ubuntu:24.04` and `ubuntu:26.04` judge the signing key alone (their
`APT::Key::Assert-Pubkey-Algo`: no DSA, no P-521, which apt names
`nistp512`, and on 26.04 no RSA under 2048 bits); `debian:13`'s sqv judges
the primary too, and refuses DSA, RSA under 2048 bits, brainpool and
secp256k1 keys (its backend verifies neither curve: "No binding
signature", whatever its config's brainpool date says) and SHA-1 or SHA-224
bindings ("not bound"), and a key whose only binding was made after the
signature (`pk_rsa3072_rebound_minimal`: "No binding signature at time …",
it judges the binding in force at the signature's time); the gpgv 2.2
images accept every one. The table test holds the cache to accept the 20
rows only a newest image refuses (crypto policy is not intersected, HD-19)
and to report no strong signer for them (`aptShapeKeyPolicy`), and a
strong signer for every other row it accepts but
`pk_rsa3072_rebound_both`, which it calls not strong though every image
accepts it (`aptShapeStrongOverStrict`: the cache reads go-crypto's
newest self-signature, made after the packet). `--policy-keys` makes the batch again alone.

Batch fx21, 17 shapes (`us_`), is the plan's Task 9e's fix round's: key U,
an RSA-3072 primary that only certifies with a signing and an encryption
subkey, signs the shared `Release` with its signing subkey (gpg's packet),
and with its primary and its encryption subkey (packets gpg will not make,
built by the generator and their hash signed by the temporary home's
gpg-agent, each checked against U's public key): a bad packet by the
primary or the encryption subkey beside the signing subkey's good one
(`us_bad_primary_plus_good`, `us_bad_enc_plus_good`,
`us_good_plus_bad_enc`, `us_d_bad_enc_plus_good`), which gpgv 2.2.4 and 1.4
(`ubuntu:18.04`, `16.04`, `14.04`) check against that key and refuse (BADSIG)
while every other image accepts it, as it does `t7g5_s_primary`; a good one
beside it (`us_good_primary_plus_good`, `us_good_enc_plus_good`,
`us_d_good_enc_plus_good`), which every image accepts; one alone
(`us_good_primary_alone`, `us_good_enc_alone`), which every current image
refuses; the control (`us_good_control`); and the good packet beside two by
keys X1 and X2, which the clients lack (`us_good_plus_two_unknown`,
debian-buster-archive's layout), which apt 1.2 (`ubuntu:16.04`) alone
refuses ("Invalid message from method gpgv: NO_PUBKEY …"). Task 8's
verifier fix (the second final review's lens V2) adds six: a good or bad
packet by the primary or the encryption subkey before the signing subkey's
SHA-512 one (`us_good_primary_then_good512`, `us_good_enc_then_good512`:
every image accepts them, gpgv 1.4 checking the first alone;
`us_bad_primary_then_good512`: 18.04, 16.04 and 14.04 refuse it), and a
signing-subkey packet in BSN's layout (a hashed creation time, an unhashed
key ID, no fingerprint: the survey's Docker and Microsoft anchors), alone
(`us_bsn_control`) and with an issuer fingerprint added to its unhashed
area, critical (`us_bsn_unh_crit33`: 16.04 and 14.04 refuse it, BADSIG) or
not (`us_bsn_unh_noncrit33`). Each shape's `keys.asc` is U's public key
alone; `--usage-keys` makes the batch again alone.

Batch fx22, 14 shapes, is Task 8's verifier fix's, cut without a key from
the committed controls (`FROM_CONTROLS`):

- `mh_` (V2-C1): B's packet, its hash octet rewritten to SHA-1, SHA-384 or
  SHA-512 (B is a key no client has, so its packet is never checked), first
  before A's good one, inline and detached, or third after both: gpgv 1.4
  (`ubuntu:16.04`, `14.04`) checks the first packet alone when the hashes
  differ ("multiple signatures detected.  Only the first will be checked")
  and refuses (NO_PUBKEY), every other image accepts; and the controls,
  B's packet after A's (`mh_good_then_unkB384`, `mh_d_good_then_unkB512`),
  which every image accepts;
- `cr_` (V2-C2): a critical reason for revocation (29) or key block of
  version 0 (38) in A's good packet's unhashed area, inline and detached:
  gpgv 1.4 refuses a critical 29, every gpgv before 2.2.27 (`debian:10`,
  `ubuntu:20.04`, `18.04`, `16.04`, `14.04`) a critical 38 (BADSIG); the
  non-critical twins every image accepts;
- `mi_` (the mirror sweep, V2-M2, below): A's good packet whose unhashed
  issuer key ID names B, its hashed fingerprint A: the gpgv 2.2 images
  (`debian:11`, `ubuntu:22.04`, `debian:12` and the five older ones) refuse
  it, the gpgv 2.4 images and sqv accept it; layer P refuses it (one
  issuer), as it refuses the `t7g` rows of the same lookup.

The keys are made in a temporary GnuPG home that the generator deletes
when the run ends, by an error, Ctrl-C, SIGTERM or SIGHUP too (only
SIGKILL leaves it, and a home it cannot delete is named on stderr); no
secret key is written anywhere else, and the generator checks that
`keys.gpg` holds public packets only, of exactly A, E, S and O, and
K's public key, fx19's `keys.asc`, public packets only, of K and its
two subkeys, each of fx20's keys, public packets only, of that key and
its subkey, and U's, of U and its two subkeys.

| Key | What it is | Why |
|---|---|---|
| A | RSA-3072, signs | the repository's key: every shape of batches fx to fx12 carries a signature by A but `i_expired_only` (E's), `x3i_subkey_good` and `r_subkey_good_*` (S's subkey's), and `a_b64_empty_body` and the `vc2_` packets (none) |
| B | RSA-3072, signs; not in `keys.gpg` | a key the clients do not know: an extra signature apt ignores (`i_unknown_*`, `d_unknown_goodfirst`, `x*_twoblocks_good_unknown`), and a garbage packet naming it (`xi_good_plus_v3garbage_B`) |
| E | RSA-3072, signs; made at 2020-01-01, valid one year (it expired at 2020-12-31); its signatures made at 2020-06-01 | a key in `signed-by` that has expired: its good or bad signature beside a good one, and its good one alone (`i_expired_*`, `i_good_plus_expired_good`) |
| S | an RSA-3072 primary that only certifies, with an RSA-3072 signing subkey | a signature by a subkey (`x3i_subkey_*`), and a forged packet naming the primary (`t7g5_s_primary`) |
| O | RSA-3072, signs | batch fx13's key: the shapes that need a signer but no other key, so that they can be made again alone (`--own-key`), the other keys' secrets being gone after every run |
| K | RSA-3072, signs, with two RSA-3072 signing subkeys; not in `keys.gpg` | batch fx19's key: signatures by its primary and by each subkey (`e7_`), each shape with K's public key as its own `keys.asc`, made again alone with `--subkeys-key` |
| U | an RSA-3072 primary that only certifies, with an RSA-3072 signing subkey and an RSA-3072 encryption subkey; not in `keys.gpg` | batch fx21's key: good and bad packets by each of its keys (`us_`), each shape with U's public key as its own `keys.asc`, made again alone with `--usage-keys` |
| X1, X2 | RSA-3072, sign; never exported | batch fx21's keys the clients lack: `us_good_plus_two_unknown` |
| fx20's | one key per class (`POLICY_KEYS` in `gen_shapes.py`); not in `keys.gpg` | batch fx20's keys: each signs the shared `Release` alone (`pk_<name>`) or beside the Ed25519 one (`pk_ed25519_plus_<name>`), each shape with its keys as its own `keys.asc`, made again alone with `--policy-keys` |

### Adding a shape

1. Write it in `gen_shapes.py`: an entry in a batch function (or a new
   function, listed in `BATCHES` or `FROM_CONTROLS`). Its name says what
   it is; the generator refuses a name used twice. A shape needs one of
   three things to be cut from:
   - **Committed fixtures, no key** (`Controls`: the files
     `CONTROL_FILES` names — the fx7 controls, A's and S's subkey's bad
     signatures, B's, E's and S's subkey's packets beside A's, the v3 and
     unknown-hash packets, S's fingerprint from `keys.gpg`, the shared
     `Release`, fx13's SHA-224 and SHA3-256 anchors): a `FROM_CONTROLS`
     batch, as fx8 to fx12, fx14 to fx18 and fx22 are; a shape that needs a
     `signed-by` of its own gets its `<name>.keys.asc` from a
     `KEYRINGS_FROM_CONTROLS` function, as fx16's `ka_` rows do.
     `python3 -I e2e/apt-oracle/gen_shapes.py --from-controls
     internal/gpg/testdata/aptshapes` writes those batches alone, from the
     committed fixtures, and every other file keeps its bytes.
   - **A signer, no other key**: batch fx13 (`batch_fx13`, from key O's
     `OwnMaterial`). `python3 -I e2e/apt-oracle/gen_shapes.py --own-key
     internal/gpg/testdata/aptshapes` makes a new key O in a temporary
     home, writes fx13 and replaces O's public key in `keys.gpg` (the
     key whose user ID is O's), every other key's packets byte for byte,
     then cuts the `FROM_CONTROLS` batches again: fx18's `e8_` shapes,
     cut from fx13's SHA-224 and SHA3-256 anchors, change with it, every
     other file keeps its bytes. The table test's keyring is
     `keys.gpg`, so it trusts the new O at once.
   - **A signer with subkeys, in no client's `keys.gpg`**: batch fx19
     (`batch_fx19`, from key K's `SubkeysMaterial`), each shape with K's
     public key as its `<name>.keys.asc`. `python3 -I
     e2e/apt-oracle/gen_shapes.py --subkeys-key
     internal/gpg/testdata/aptshapes` makes a new key K and writes fx19
     and its key files alone; every other file keeps its bytes.
   - **A key of each class**: batch fx20 (`batch_fx20`, from
     `PolicyMaterial`), each shape with its keys as its `<name>.keys.asc`.
     `python3 -I e2e/apt-oracle/gen_shapes.py --policy-keys
     internal/gpg/testdata/aptshapes` makes new keys and writes fx20 and its
     key files alone; every other file keeps its bytes.
   - **A key with keys outside its signing usage**: batch fx21
     (`batch_fx21`, from `UsageMaterial`), each shape with U's public key
     as its `<name>.keys.asc`. `python3 -I e2e/apt-oracle/gen_shapes.py
     --usage-keys internal/gpg/testdata/aptshapes` makes new keys U, X1
     and X2 and writes fx21 and its key files alone; every other file keeps
     its bytes.
   - **The keys A, B, E and S** (`Material`): a batch in `BATCHES`, made
     only by a full regeneration.
   A detached shape that signs another text than the shared `Release`
   gives that text in its batch's third dict; the generator writes it as
   `<name>.Release`.
2. Regenerate. With `--from-controls`, `--own-key`, `--subkeys-key`,
   `--policy-keys` or `--usage-keys`, only that part changes, so in step 4
   every other row must come out unchanged. A full regeneration,
   `python3 -I e2e/apt-oracle/gen_shapes.py internal/gpg/testdata/aptshapes`,
   makes new keys A, B, E, S, O and K and new signatures, so every fixture
   changes (the key-free batches are cut from the new controls, byte
   surgery the same); `verdicts.txt` and `classes.txt` are left in place,
   and are now stale. About one full run in 256 stops with "B's packet
   leaves no padding: run again" (a cut needs base64 padding that the new
   packet's length may not give): run it again. Two fx10 cuts
   (`a_crc_on_last_line`, `a_pad_split_437`) assume the length of A's
   packet that gpg 2.4 writes (438 octets) without checking it, so check
   their rows after a full regeneration with another gpg (the Task 9a
   review's 9A-M4).
3. Re-measure, both sets: `bash e2e/apt-oracle/run.sh --record` and
   `bash e2e/apt-oracle/run.sh --old --record`.
4. Review `git diff --no-ext-diff internal/gpg/testdata/aptshapes/verdicts.txt
   internal/gpg/testdata/aptshapes/classes.txt
   internal/gpg/testdata/aptshapes/verdicts-old.txt
   internal/gpg/testdata/aptshapes/classes-old.txt`: the new rows, and no
   other row changed. An old row that changed is apt changing or a broken
   builder: find out which before going on.
5. Run the verifier's table test (`go test ./internal/gpg`): it holds the
   cache to the new row, or the shape goes in its deviations, with a
   reason.
6. Commit the generator, the fixtures and the four verdict and class
   files together.

Never edit a fixture by hand: the fixtures and the four files are one
measurement.

## What the oracle checks

`run.sh` starts one container per image, all at once (`docker run --rm
--init --network none`, the corpus mounted read-only at `/fx`, 600 seconds
each at most). In each, `in-container.sh` builds every shape's flat
`file:` repository in a temporary directory — the shape's file as
`InRelease`, or as `Release.gpg` beside `Release` (the shape's own, if it
has one), and an empty `Packages` — and runs `apt-get update` on that
repository alone, with `signed-by=/fx/keys.gpg` (or the shape's own
`/fx/<name>.keys.asc`), and once more for a
refused shape, to see whether apt ran its verifier on it (*Error
classes*). `run.sh` checks that every image measured every shape once,
derives each refusal's error class, prints the verdicts as one table
(shape by image) and compares verdicts and classes with `verdicts.txt` and
`classes.txt`: each difference is printed as **apt changed**, with the
first `E:` or `W:` line apt printed for that shape, and the run fails.

It checks apt, not the cache: that the images' apt still gives the
recorded verdicts on these bytes. Holding the cache to `verdicts.txt` is
the job of the verifier's Go table test over the same files
(`TestVerifier_FollowsTheAptShapeTable`, `go test ./internal/gpg`, in
CI): it runs every shape, none skipped, and the cache refuses a shape when
any of the six apt refuses it and accepts a shape all six accept, but for
the strict deviations it lists, each in its family (127 shapes in four:
one armor block, non-canonical armor layout, non-canonical packet layout,
unverifiable algorithm; `aptShapeDeviations`), and the crypto-policy rows
only a newest image refuses, which it accepts (`aptShapeKeyPolicy`, ruling
HD-19). Its known-unsafe list is empty (`x3i_sha1_only` is refused since
ruling HD-18: SHA-1 never counts), every refused row is pinned to the rule
that refuses it (`TestVerifier_RefusesEachRowByItsRule`), and the cache's
port of apt's text layer refuses exactly the `split` rows
(`TestAptTextLayer_RefusesExactlyTheSplitRows`, *Error classes*).
`TestAptShapeClasses_AgreeWithTheVerdicts` checks `classes.txt` against
`verdicts.txt`.

That guarantee — the cache adopts no anchor a supported apt refuses, but
for crypto policy (the 20 `aptShapeKeyPolicy` rows, and 14 of
`aptShapeOldExempt`'s) and an older image's refusal of a real signer's
layout (the other 6 of `aptShapeOldExempt`'s, *The older images* below)
— is only as wide as the corpus: it holds for the 876
shapes the table test runs, not for a shape the corpus lacks: an anchor
shaped otherwise is unmeasured, and the cache may adopt it while some apt
refuses it (the hybrid verifier's canonical grammar makes that unlikely:
the residual is in `docs/open-decisions.md`, *Open*). When a new shape
turns up (a vendor's anchor, a go-crypto change, a review), add it.

Since the plan's Task 9e the table test reads `verdicts-old.txt` too
(ruling HD-19, *The older images*): the cache refuses a shape an older
image refuses, the format rules (layers T, A and P) holding over every
supported generation, but for the rows `aptShapeOldExempt` lists, each with
its class and the refusal its class was measured with
(`aptShapeOldRefusals`), which the cache accepts as the six do: (a) crypto
policy, not intersected across generations (the `pk_` rows of EdDSA and
ECDSA keys, which gpgv 1.4 cannot use); (b) a layout a real signer emits (a
CR after the clearsign marker, nginx's, which apt 1.2 refuses; no CRC line,
the base64 happening to need no padding, syncthing's today and any CRC-less
signer's when its packets' length lands on a multiple of 3, which gpgv 1.4
misreads; a good signature beside two by keys the client lacks,
debian-buster-archive's, which apt 1.2 refuses). Of the 41 rows an older
image refuses while the six accept, the cache refuses 21: layer P three
already (gpgv 1.4's v6 and 10 kB unhashed-area refusals: no longer strict
deviations) and five more since Task 8's verifier fix (critical 29, 33 and
38, V2-C2); T6 three (blanks after the clearsign marker, which apt 1.2
refuses: `a_msgbegin_sp_cr`, `q_msgbegin_sp_tab_crcr`,
`i_begin_trailing_ws`); S1 six (a bad packet by a known key outside its
signing usage, which gpgv 2.2.4 and 1.4 check: `t7g5_s_primary` and fx21's
`us_bad_*` and `us_good_plus_bad_enc`; the controller's ruling for Task
9e's fix round: S1 checks every known key, an integrity rule, not crypto
policy); and the first-only rule four (V2-C1: when the packets' hashes
differ, gpgv 1.4 checks the first alone, which must be by a key the cache
holds: fx22's `mh_` gaps). Where an older image accepts what the cache
refuses, the cache stays strict.

`xi_good_plus_marker` (a marker packet after a good signature) is refused
by `ubuntu:26.04` alone, and not as a policy: its gpgv 2.4.8 crashes on it
(`Sub-process /usr/bin/gpgv exited unexpectedly`, the same in five reruns
during planning), while gpgv 2.2 and 2.4.4 and `debian:13`'s sqv accept
it. The hardening plan has the cache refuse it too (its Task 6), by the
rule that what any supported apt refuses is refused (DN-3). If a later
image's gpgv stops crashing, the oracle reports **apt changed** for that
cell: all six apt then accept the shape, and the cache's refusal becomes a
strict deviation, to be listed in the table test with its reason.

## Error classes

`classes.txt` gives each refusal one class, so that a test can ask which
part of apt refused a shape, not only whether it did. The hardening plan's
Task 9b holds the cache's port of apt's text layer to it: the port must
refuse exactly the `split` rows.

| Class | apt refused the shape | apt's first `E:`/`W:` line, for example |
|---|---|---|
| `split` | by its own checks of the file, before its verifier (gpgv or sqv) ran: the clearsign sniff, `SplitClearSignedFile` or `VerifyDetachedSignatureFile` (`apt-pkg/contrib/gpgv.cc`) | `Splitting up … into data and signature failed`; `Clearsigned file isn't valid, got 'NOSPLIT'`; `does not start with a signed message block`; `Detached signature file … contains unexpected line starting with a dash`, `… contains lines not belonging to a signature`, `… is in unsupported binary format`, `… contains unclosed signatures`; and, for a `Release.gpg` with no BEGIN line, `Signed file isn't valid, got 'NODATA'`, which apt then writes itself |
| `nodata` | its verifier ran and found no signature data: gpgv's NODATA | `Signed file isn't valid, got 'NODATA'` |
| `badsig` | its verifier ran and a signature failed its check | gpgv's `The following signatures were invalid: BADSIG …`; sqv's `Message has been manipulated` |
| `other` | any other refusal once apt had passed the file on to its verifier: what the verifier said, or a later refusal (`i_dash_escaped_end`: the verifier accepted the signature, and apt's `Release` parser refused the M its splitter made) | sqv's malformed-armor, malformed-packet and policy errors, or its crash; gpgv 2.2's `seems good, but expected details are missing`; `EXPKEYSIG`; `Unknown error executing apt-key` (`… gpgv` on `ubuntu:26.04`); `Unable to parse package file` |

The split is apt's control flow, not its wording, because the wording
cannot tell the two NODATA apart: for a refused shape `in-container.sh`
runs `apt-get update` a second time with the gpgv and sqv methods' debug
output (`Debug::Acquire::gpgv`, `Debug::Acquire::sqv`), which prints
`Preparing to exec:` (gpgv; apt-key on apt 2.2) or `Executing …sqv` only
once apt's own checks have passed the file on; the verdict and the line
come from the first run, made as before. `run.sh` then checks the class
against the words, so that a changed apt shows instead of shifting a class
silently: a `split` refusal must print one of the text-layer messages
above (`got 'NODATA'` only for a shape that is a `Release.gpg`), and a
refusal after the verifier ran none of them; `nodata`,
`badsig` and `other` come from the line. A disagreement, or a second run
whose exit status differs from the first's, fails the run. The messages
are the same in the six apt versions (their `gpgv.cc`, `methods/gpgv.cc`
and `methods/sqv.cc`; the final review's `PE/final-review/A/src-apt/`), and
so is the text layer: a row is `split` in every column or in none (64 rows
are, all six), which `TestAptShapeClasses_AgreeWithTheVerdicts` checks. Not
so in the older images (*The older images*): `debian:10`'s and
`ubuntu:20.04`'s text layer is the six's (a row is `split` there exactly
when it is in `classes.txt`: `TestAptShapeOldClasses_AgreeWithTheVerdicts`),
`ubuntu:18.04`'s, `16.04`'s and `14.04`'s is not, and apt 1.0 and 1.2
word some of an InRelease's split refusals `Clearsigned file isn't valid,
got 'NODATA'`, which `run.sh` accepts as `split` from an apt before 1.6
only.

## The images

The columns of `verdicts.txt` and `classes.txt`, in their order (`COLUMNS`
in `run.sh`), as measured on 2026-10-29:

| Image | apt | Verifier |
|---|---|---|
| `debian:11` | 2.2.4 | gpgv 2.2.27-2+deb11u3 |
| `ubuntu:22.04` | 2.4.14 | gpgv 2.2.27-3ubuntu2.5 |
| `ubuntu:24.04` | 2.8.3 | gpgv 2.4.4-2ubuntu17.4 |
| `debian:12` | 2.6.1 | gpgv 2.2.40-1.1+deb12u2 |
| `debian:13` | 3.0.3 | sqv 1.3.0-3+b2 (Sequoia; the image has no gpgv) |
| `ubuntu:26.04` | 3.2.0 | gpgv 2.4.8-4ubuntu3 |

They are e2e/flat's six. `E2E_APT_ORACLE_IMAGES` takes any of them, each
compared with its own column; `--record` measures all six, since
`verdicts.txt` has a column for each. Another set of images is another
set of columns: change `COLUMNS`, record, and check what reads the
columns (the table test).

## The older images

The cache supports every client that still has upstream support, Ubuntu
14.04 (through Ubuntu Pro, until May 2029) included (the hardening plan's
addendum of 2026-11-03, ruling HD-19). `verdicts-old.txt`'s columns, in
their order (`OLD_COLUMNS` in `run.sh`), as measured on 2026-11-05 (every
image pulled and ran):

| Image | apt | Verifier |
|---|---|---|
| `debian:10` | 1.8.2.3 | gpgv 2.2.12-1+deb10u2 |
| `ubuntu:20.04` | 2.0.10 | gpgv 2.2.19-3ubuntu2.4 |
| `ubuntu:18.04` | 1.6.17 | gpgv 2.2.4-1ubuntu1.6 |
| `ubuntu:16.04` | 1.2.35 | gpgv 1.4.20-1ubuntu3.3 |
| `ubuntu:14.04` | 1.0.1ubuntu2.24 | gpgv 1.4.16-1ubuntu2.6 |

`in-container.sh` adapts to an older apt three ways, each by the image's
apt version, so that the six current images run exactly as before:

- **apt before 1.4** (`ubuntu:16.04`, `14.04`) cannot read an armored key
  file in `signed-by` (apt 1.0 has no `signed-by` at all and ignores it):
  the image's own keyrings are removed, and the shape's keys, dearmored by
  the image's gpg (every block: gpg 1.4's `--dearmor` keeps both of a
  `ka_*` file's), are the one file of `/etc/apt/trusted.gpg.d`, the
  sources line naming no `signed-by`. The image's own directory, not
  `-o Dir::Etc::TrustedParts`: apt 1.2's `apt-key` reads its configuration
  by `apt-config`, never by `apt-get`'s options. The run stops (exit 2) if
  the image's `/etc/apt/trusted.gpg` is still there at a shape, so that the
  shape's keys are its keyring alone, as `signed-by` makes them.
- **apt before 1.5** (`ubuntu:16.04`) treats a repository it cannot
  authenticate as unsigned, a warning, and exits 0:
  `-o Acquire::AllowInsecureRepositories=false` makes that an error.
- **apt 1.0** (`ubuntu:14.04`) has no such option and exits 0 whatever
  gpgv said, so its status is taken as 100 when apt exited 0 but its lists
  hold no verified `InRelease` or `Release.gpg`, which apt keeps only once
  gpgv has passed it (`debReleaseIndex::IsTrusted` reads the same file).
  Measured on Task 9e's 865 rows: the verdict this gives is the gpgv
  method's own answer (`-o Debug::pkgAcquire::Worker=1`: `gpgv:201 URI
  Done` or `gpgv:400 URI Failure`), where the exit status alone missed 116
  refusals.

What they measure: `debian:10` and `ubuntu:20.04` give `debian:11`'s
verdict on every row. `ubuntu:18.04`'s gpgv 2.2.4 (and gpgv 1.4) also
checks a packet against a known key that cannot sign, a certify-only
primary or an encryption subkey, and refuses a bad one (`t7g5_s_primary`,
`us_bad_*`: BADSIG), and its apt reads some layouts the newer text layer
refuses. `ubuntu:16.04`'s apt 1.2 refuses an InRelease with any byte after
the clearsign marker on line 1, a CR or a blank (`i_crlf_all`,
`n_i_nginx_crlf`, `i_begin_trailing_ws`: nginx's real InRelease is CRLF),
and a good signature beside two by keys the client lacks
(`us_good_plus_two_unknown`: debian-buster-archive's real anchors are so).
gpgv 1.4 (`ubuntu:16.04`, `14.04`) cannot verify EdDSA or ECDSA (the `pk_`
rows of those keys), reads past base64 that needs no padding into the END
line when no CRC line follows it (`i_crc_missing`, `d_crc_missing`:
syncthing's real InRelease is so), and refuses a v6 packet or a 10 kB
unhashed area beside a good signature, a critical 29 or 33 (38: every gpgv
before 2.2.27), and, when the packets' hashes differ, any anchor whose
first packet is not good (it checks that one alone).

**The mirror method** (the second final review's V2-M2). The older images
were first classified on the corpus's rows only, so a mechanism the corpus
held only in its safe direction stayed open: gpgv 1.4's first-only rule
showed as an older image *accepting* a row the cache refuses
(`g5_good256_then_bad512`), and its other direction, an unknown first
packet, as no row at all (V2-C1). So every row where an older image
accepts what a current image refuses is read for its mechanism (why that
apt or gpgv reads it otherwise), and the mirrored shape is generated and
measured in all eleven images: the same mechanism hiding a check the cache
needs (a packet that older reading never checks, a field it reads another
way), so that it shows as an older image *refusing* what the cache
adopts. Such a row is a finding: fold it with the narrowest rule in T, A
or P (or S, for an integrity rule like S1's) if no real signer has its
layout (E3), else record it. Task 8's verifier fix swept the families of
its day (the report's table): gpgv 1.4's first-only rule (`mh_`, folded),
older gpgv's critical-subpacket lists (`cr_`, `us_bsn_`, folded), older
gpgv's issuer lookup (`mi_`, refused already by layer P), and apt 1.0's
and 1.2's missing erasure, older gpgv's key usage, older apt's text
layers and armor headers, whose mirrors the corpus already held and
measured (no gap but T6, folded in Task 9e).

## When to run it

- Before every release, on the tree you tag, both sets (`make
  e2e-apt-oracle` and `make e2e-apt-oracle-old`), after `docker pull` of
  the eleven images (*Running it*: without it the run measures the cached
  images again and passes by construction), adding a row to *Results*; a
  difference blocks the release until it is recorded and the table test
  agrees (*When apt changes*). `docs/open-decisions.md` records this under
  *Decided* ("The apt oracle runs before every release"), and lists it for
  1.1.0-rc1.
- After a go-crypto upgrade, and after a change to `internal/gpg`'s armor
  or signature checks: the table test shows what the cache does, the
  oracle that apt still does what the table says.
- When an image's apt or its verifier (gpgv, sqv) changes, to see whether
  the verdicts did.

## When apt changes

A difference names the shape, the image, the recorded and the measured
verdict (or error class), and what apt said. Run that image alone again to
rule out a flake (`E2E_APT_ORACLE_IMAGES=<image> make e2e-apt-oracle`);
then:

1. Record it: `bash e2e/apt-oracle/run.sh --record` (`--old --record`
   for an older image), and check that the diff of `verdicts.txt` and
   `classes.txt` (or `verdicts-old.txt` and `classes-old.txt`) holds only
   the changed cells.
2. Update the shape's description in *The corpus* above if its meaning
   changed (the hardening plan's Appendix A stays as measured on
   2026-10-28: `verdicts.txt` and this README are its successors, and
   step 1 has rewritten `verdicts.txt`), and add a row to *Results* below
   with the date.
3. Run the verifier's table test: the cache's verdict may now be a
   deviation to list (a refusal all six apt now accept), or a refusal to
   add (a shape some apt now refuses).

A run that fails on an error class apt's words do not bear out (a `split`
refusal without a text-layer message, or the reverse) means apt's checks
or their messages changed: read the image's `apt-pkg/contrib/gpgv.cc` and
its `methods/` against *Error classes*, and update the classes and this
section with what it shows. A difference that names a shape with no row in
`verdicts.txt` or `classes.txt`, or a row with no shape, means the corpus
and its measurement are out of step: regenerate and re-record them
together (*Adding a shape*).

## Results

| Date | Tree | Result | Time |
|---|---|---|---|
| 2026-10-29, 03:45 UTC (the hardening plan's Task 5) | the commit that adds the corpus, on 7c3aca0 | `--record`: 135 shapes in 6 images, every verdict the one Appendix A of the plan gives (2026-10-28); then `make e2e-apt-oracle`: PASS. apt 2.2.4, 2.4.14, 2.8.3, 2.6.1, 3.0.3 and 3.2.0 (*The images*) | 11 s each run, images cached |
| 2026-10-29, 05:38 UTC (ruling HD-13, after the plan's Task 6) | the commit that adds batch fx8, on 80af12b | `--from-controls` wrote fx8's 20 shapes and nothing else; `--record`: 155 shapes in 6 images, the 135 earlier rows unchanged and the 20 new ones as Task 6 measured them; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 12 s each run, images cached |
| 2026-10-29, 07:43 UTC (ruling HD-14, the plan's Task 7) | the commit that adds batch fx9, on 40d3614 | `--from-controls` wrote fx9's 13 shapes and nothing else (fx8's 20 again, byte for byte); `--record`: 168 shapes in 6 images, the 155 earlier rows unchanged and the 13 new ones as Task 7 measured them; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 13 s each run, images cached |
| 2026-10-29, 15:30 UTC (the plan's Task 9a) | the commit that adds batches fx10 to fx13 and `classes.txt`, on 78175bb | `--from-controls` wrote fx10 to fx12's 307 shapes (fx8's and fx9's 33 again, byte for byte), `--own-key` fx13's 15 and key O; `--record`: 490 shapes in 6 images, the 168 earlier rows unchanged, the 309 the final review measured as it measured them, and the error classes of every refusal; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 51 s and 50 s, images cached |
| 2026-10-29, 16:23 UTC (the plan's Task 9b) | the commit that adds batch fx14, on 9efa08d | `--from-controls` wrote fx14's 8 shapes (fx8 to fx12's 340 again, byte for byte); `--record`: 498 shapes in 6 images, the 490 earlier rows and their error classes unchanged; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 51 s and 54 s, images cached |
| 2026-11-01, 21:23 UTC (the plan's Task 9b, its fix round) | the commit that adds batches fx15 to fx17 | `--from-controls` wrote fx15 to fx17's 232 shapes and fx16's 6 key files (fx8 to fx12's and fx14's 348 again, byte for byte), the re-review's 226 files byte for byte as its generators wrote them; `--record`: 730 shapes in 6 images, the 498 earlier rows and their error classes unchanged, the 220 rows the re-review measured as it measured them, the 12 `sq_` rows as the table predicts; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 68 s and 68 s, images cached |
| 2026-11-01, 23:41 UTC (the plan's Task 9b, its second fix round) | the commit that adds batches fx18 and fx19 | `--from-controls` wrote fx18's 76 shapes (fx8 to fx12's and fx14 to fx17's 580 again, byte for byte), the second re-review's 76 files byte for byte as its generator wrote them; `--subkeys-key` fx19's 17 shapes and their 17 key files, signed by a new key K; `--record`: 823 shapes in 6 images, the 730 earlier rows and their error classes unchanged, the 93 new rows as the second re-review measured them (fx19's with K for its K3); then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 84 s and 77 s, images cached |
| 2026-11-05, 08:21 UTC (the plan's Task 9d) | the commit that adds batch fx20 | `--policy-keys` wrote fx20's 42 shapes and their 42 key files, signed by new throwaway keys (three earlier runs of the same batch, at 07:32, 07:35 and 07:51 UTC, each with other keys and fewer rows, gave the same verdict on every row they shared); `--record`: 865 shapes in 6 images, the 823 earlier rows and their error classes unchanged, the 42 new rows as *Batch fx20* above describes them; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 82 s, images cached |
| 2026-11-05, 09:31 UTC (the plan's Task 9e) | the commit that adds the older images, on 0d7b103 | `bash e2e/apt-oracle/run.sh --old --record`: 865 shapes in the five older images (*The older images*), every image pulled and ran; then `make e2e-apt-oracle-old`: PASS, every verdict and class as recorded, and `make e2e-apt-oracle`: PASS with the adapted `in-container.sh`. apt 1.8.2.3, 2.0.10, 1.6.17, 1.2.35 and 1.0.1ubuntu2.24 | 93 s and 94 s (the five), 82 s (the six), images cached |
| 2026-11-05, 10:53 UTC (the plan's Task 9e, its fix round) | the commit that adds batch fx21 | `--usage-keys` wrote fx21's 11 shapes and their 11 key files, signed by a new key U (`--from-controls` then rewrote every other batch byte for byte); `--record` and `--old --record`: 876 shapes in 6 and in 5 images, the 865 earlier rows and their error classes unchanged in both files, the 11 new ones as *Batch fx21* above describes them (as the Task 9e review measured its s1m and fx2 shapes); then both make targets: PASS. The same apt and verifiers | 83 s (the six), 94 s (the five), images cached |
| 2026-11-05, 17:43 UTC (the plan's Task 8, its verifier fix) | the commit that adds batch fx22 and fx21's six rows | `--usage-keys` wrote fx21's 17 shapes and key files, signed by a new key U, and `--from-controls` fx22's 14 (every other batch byte for byte); `--record` and `--old --record`: 896 shapes in 6 and in 5 images, the 876 earlier rows and their error classes unchanged in both files (fx21's 11 earlier rows too, with the new key), the 20 new ones as *Batch fx21* and *Batch fx22* above describe them (as the second final review's lens V2 measured its o2_, mh_, cr_ and vd_ probes); then both make targets: PASS. The same apt and verifiers | 92 s (the six), 101 s (the five), images cached |
