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
  `classes.txt`.

The oracle is manual, like `make e2e-live`, and never runs in CI.

## Running it

```sh
make e2e-apt-oracle                                    # all six images, compared
E2E_APT_ORACLE_IMAGES=debian:13 make e2e-apt-oracle    # some of them
bash e2e/apt-oracle/run.sh --record                    # all six: rewrite verdicts.txt and classes.txt
```

It needs Docker and GNU `timeout`, and the six images (pulled on first
use; the containers themselves run with `--network none`). The images run
in parallel, one `apt-get update` per shape in each and a second one for
each shape apt refuses (*Error classes*); with the images cached a run
takes about 50 seconds on a 12-core machine. To measure today's apt,
`docker pull` the six images first: `docker run` never refreshes a cached
tag, and the version lines a run prints show which apt it measured.

The exit status is 0 when apt gives the recorded verdicts and error
classes (or `--record` wrote them), 1 when apt changed, a container failed
or an error class and apt's words disagree, and 2 for a usage or setup
error: an image that is not a column of `verdicts.txt`, `--record` with
only some of the images, a corpus file missing.

## The corpus

`internal/gpg/testdata/aptshapes/` holds 744 files, about 890 KiB:

| File | What it is |
|---|---|
| `<name>.InRelease` | an inline shape (488 of them): the whole `InRelease`, its signed text included |
| `<name>.Release.gpg` | a detached shape (242): a `Release.gpg` that signs the shared `Release`, or its own |
| `<name>.Release` | a detached shape's own `Release` (4: `a_d_release_crlf`, `a_d_release_extra_nl`, `a_d_release_no_final_nl`, `o2_textmode_detached_release_crlf`), which `in-container.sh` serves in the shared one's place |
| `<name>.keys.asc` | a shape's own `signed-by` (6: batch fx16's `ka_*`): an armored key file, the keys A and E in one block each, which `in-container.sh` names in `keys.gpg`'s place and the table test loads as that shape's host keyring, as `LoadKeyring` reads a `.asc` file (every block) |
| `Release` | the shared `Release`, a flat repository's: the SHA256 line of an empty `Packages`, a fixed `Date:` in the past (2026-10-01), no `Valid-Until` |
| `keys.gpg` | the public keys A, E, S and O, in binary: the clients' `signed-by` here, and the host keyring of the verifier's table test (the hardening plan's Task 6), which loads the file whole, so every key in it is a trusted signer there |
| `verdicts.txt` | apt's verdicts: a `#` header naming the measurement's date and each image's apt and verifier versions, then one row per shape, `<name>` and one verdict per image in the order of *The images* below, `ok` (`apt-get update` exited 0) or `REJ` |
| `classes.txt` | apt's error class of each refusal, the same rows and columns: `-` where the verdict is `ok`, else `split`, `nodata`, `badsig` or `other` (*Error classes*) |

The repository's `.gitattributes` marks every file here `-text`, so no
checkout or commit converts their line ends, whatever `core.autocrlf`
says: several shapes differ from their controls only in CRs.

There are 823 shapes, in nineteen batches. The first 135 are those the
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
batch fx18, `e7_` batch fx19;
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
(`Unknown error executing gpgv`; GnuPG 2.4 lists no SHA3) and the
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

The keys are made in a temporary GnuPG home that the generator deletes
when the run ends, by an error, Ctrl-C, SIGTERM or SIGHUP too (only
SIGKILL leaves it, and a home it cannot delete is named on stderr); no
secret key is written anywhere else, and the generator checks that
`keys.gpg` holds public packets only, of exactly A, E, S and O, and
K's public key, fx19's `keys.asc`, public packets only, of K and its
two subkeys.

| Key | What it is | Why |
|---|---|---|
| A | RSA-3072, signs | the repository's key: every shape of batches fx to fx12 carries a signature by A but `i_expired_only` (E's), `x3i_subkey_good` and `r_subkey_good_*` (S's subkey's), and `a_b64_empty_body` and the `vc2_` packets (none) |
| B | RSA-3072, signs; not in `keys.gpg` | a key the clients do not know: an extra signature apt ignores (`i_unknown_*`, `d_unknown_goodfirst`, `x*_twoblocks_good_unknown`), and a garbage packet naming it (`xi_good_plus_v3garbage_B`) |
| E | RSA-3072, signs; made at 2020-01-01, valid one year (it expired at 2020-12-31); its signatures made at 2020-06-01 | a key in `signed-by` that has expired: its good or bad signature beside a good one, and its good one alone (`i_expired_*`, `i_good_plus_expired_good`) |
| S | an RSA-3072 primary that only certifies, with an RSA-3072 signing subkey | a signature by a subkey (`x3i_subkey_*`), and a forged packet naming the primary (`t7g5_s_primary`) |
| O | RSA-3072, signs | batch fx13's key: the shapes that need a signer but no other key, so that they can be made again alone (`--own-key`), the other keys' secrets being gone after every run |
| K | RSA-3072, signs, with two RSA-3072 signing subkeys; not in `keys.gpg` | batch fx19's key: signatures by its primary and by each subkey (`e7_`), each shape with K's public key as its own `keys.asc`, made again alone with `--subkeys-key` |

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
     batch, as fx8 to fx12 and fx14 to fx18 are; a shape that needs a
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
   - **The keys A, B, E and S** (`Material`): a batch in `BATCHES`, made
     only by a full regeneration.
   A detached shape that signs another text than the shared `Release`
   gives that text in its batch's third dict; the generator writes it as
   `<name>.Release`.
2. Regenerate. With `--from-controls`, `--own-key` or `--subkeys-key`,
   only that part changes, so in step 4 every other row must come out
   unchanged. A full regeneration,
   `python3 -I e2e/apt-oracle/gen_shapes.py internal/gpg/testdata/aptshapes`,
   makes new keys A, B, E, S, O and K and new signatures, so every fixture
   changes (the key-free batches are cut from the new controls, byte
   surgery the same); `verdicts.txt` and `classes.txt` are left in place,
   and are now stale.
3. Re-measure: `bash e2e/apt-oracle/run.sh --record`.
4. Review `git diff --no-ext-diff internal/gpg/testdata/aptshapes/verdicts.txt
   internal/gpg/testdata/aptshapes/classes.txt`: the new rows, and no
   other row changed. An old row that changed is apt changing or a broken
   builder: find out which before going on.
5. Run the verifier's table test (`go test ./internal/gpg`): it holds the
   cache to the new row, or the shape goes in its deviations, with a
   reason.
6. Commit the generator, the fixtures, `verdicts.txt` and `classes.txt`
   together.

Never edit a fixture by hand: the fixtures, `verdicts.txt` and
`classes.txt` are one measurement.

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

It checks apt, not the cache: that the six images' apt still gives the
recorded verdicts on these bytes. Holding the cache to `verdicts.txt` is
the job of the verifier's Go table test over the same files, which the
hardening plan adds in its Tasks 6 and 7
(`TestVerifier_FollowsTheAptShapeTable`): the cache refuses a shape when
any of the six apt refuses it, and accepts a shape all six accept, but for
the exceptions the test lists, each with its reason, and for the 138
shapes of batches fx10 to fx13 it skips by name (`pendingHybrid`) until
the plan's Task 9b: 105 the cache adopts though some apt refuses them, 29
it refuses though every apt accepts them, and the four V-C2 shapes, which
it refuses as apt does but only after go-crypto has allocated gibibytes
for each. `TestAptShapeClasses_AgreeWithTheVerdicts` checks
`classes.txt` against `verdicts.txt`.

That guarantee — the cache adopts no anchor a supported apt refuses, but
for the one known-unsafe shape the test lists (`x3i_sha1_only`: weak
digests are outside the plan) — is only as wide as the corpus: it holds
for the 352 shapes the table test runs, not for the 105 adopted ones it
skips, nor for a shape the corpus lacks: an anchor shaped otherwise is
unmeasured, and the cache may adopt it while some apt refuses it. When a
new shape turns up (a vendor's anchor, a go-crypto change, a review), add
it.

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
| `other` | its verifier ran and said anything else | sqv's malformed-armor, malformed-packet and policy errors, or its crash; gpgv 2.2's `seems good, but expected details are missing`; `EXPKEYSIG`; `Unknown error executing gpgv` |

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
are, all six), which `TestAptShapeClasses_AgreeWithTheVerdicts` checks.

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

## When to run it

- Before a release, on the tree you tag (`docs/open-decisions.md`, the
  list before the 1.1.0 release candidate), adding a row to *Results*.
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

1. Record it: `bash e2e/apt-oracle/run.sh --record`, and check that the
   diff of `verdicts.txt` and `classes.txt` holds only the changed cells.
2. Update the shape's row in the record that succeeds the hardening
   plan's Appendix A (the plan itself stays as measured on 2026-10-28),
   and add a row to *Results* below with the date.
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
