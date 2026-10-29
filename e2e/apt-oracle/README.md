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
  on every shape and compares its verdicts with those the corpus records
  in `verdicts.txt`.

The oracle is manual, like `make e2e-live`, and never runs in CI.

## Running it

```sh
make e2e-apt-oracle                                    # all six images, compared
E2E_APT_ORACLE_IMAGES=debian:13 make e2e-apt-oracle    # some of them
bash e2e/apt-oracle/run.sh --record                    # all six: rewrite verdicts.txt
```

It needs Docker and GNU `timeout`, and the six images (pulled on first
use; the containers themselves run with `--network none`). The images run
in parallel, one `apt-get update` per shape in each; with the images
cached a run takes about 10 seconds on a 12-core machine. To measure
today's apt, `docker pull` the six images first: `docker run` never
refreshes a cached tag, and the version lines a run prints show which apt
it measured.

The exit status is 0 when apt gives the recorded verdicts (or `--record`
wrote them), 1 when apt changed or a container failed, and 2 for a usage
or setup error: an image that is not a column of `verdicts.txt`, `--record`
with only some of the images, a corpus file missing.

## The corpus

`internal/gpg/testdata/aptshapes/` holds:

| File | What it is |
|---|---|
| `<name>.InRelease` | an inline shape (104 of them): the whole `InRelease`, its signed text included |
| `<name>.Release.gpg` | a detached shape (51): a `Release.gpg` that signs the shared `Release` |
| `Release` | the shared `Release`, a flat repository's: the SHA256 line of an empty `Packages`, a fixed `Date:` in the past (2026-10-01), no `Valid-Until` |
| `keys.gpg` | the public keys A, E and S, in binary: the clients' `signed-by` here, and the host keyring of the verifier's table test (the hardening plan's Task 6) |
| `verdicts.txt` | apt's verdicts: a `#` header naming the measurement's date and each image's apt and verifier versions, then one row per shape, `<name>` and one verdict per image in the order of *The images* below, `ok` (`apt-get update` exited 0) or `REJ` |

A detached shape that signs another text than the shared `Release` would
carry it as `<name>.Release`, which `in-container.sh` then serves in its
place; none does today. The repository's `.gitattributes` marks every
file here `-text`, so no checkout or commit converts their line ends,
whatever `core.autocrlf` says: several shapes differ from their controls
only in CRs.

There are 155 shapes. The first 135 are those the hardening plan measured
on 2026-10-28
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
fx8; `i_trail_lf` is a blank line after the END line,
`xd_good_plus_literal` a literal-data packet beside a good detached
signature, a `*_control_good` its batch's unmodified control.

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

The keys are made in a temporary GnuPG home that the generator deletes
when the run ends, by an error, Ctrl-C, SIGTERM or SIGHUP too (only
SIGKILL leaves it, and a home it cannot delete is named on stderr); no
secret key is written anywhere else, and the generator checks that
`keys.gpg` holds public packets only, of exactly A, E and S.

| Key | What it is | Why |
|---|---|---|
| A | RSA-3072, signs | the repository's key: every shape but `i_expired_only` and `x3i_subkey_good` carries a signature by A |
| B | RSA-3072, signs; not in `keys.gpg` | a key the clients do not know: an extra signature apt ignores (`i_unknown_*`, `d_unknown_goodfirst`, `x*_twoblocks_good_unknown`), and a garbage packet naming it (`xi_good_plus_v3garbage_B`) |
| E | RSA-3072, signs; made at 2020-01-01, valid one year (it expired at 2020-12-31); its signatures made at 2020-06-01 | a key in `signed-by` that has expired: its good or bad signature beside a good one, and its good one alone (`i_expired_*`, `i_good_plus_expired_good`) |
| S | an RSA-3072 primary that only certifies, with an RSA-3072 signing subkey | a signature by a subkey (`x3i_subkey_*`) |

### Adding a shape

1. Write it in `gen_shapes.py`: an entry in a batch function (or a new
   function, listed in `BATCHES`) that cuts it from `Material` — the good
   inline signature (`head`, `sig_a`), the detached one (`det_a`) and the
   others — with `replace`, `armor` and the packet helpers. Its name says
   what it is; the generator refuses a name used twice. A detached shape
   that signs another text than the shared `Release` needs `write_corpus`
   extended to write its `<name>.Release` (no shape does today).
2. Regenerate the whole corpus:
   `python3 -I e2e/apt-oracle/gen_shapes.py internal/gpg/testdata/aptshapes`.
   Each run makes new keys and new signatures, so every fixture changes;
   `verdicts.txt` is left in place, and is now stale. A shape cut without a
   key from the fx7 controls can go in a `FROM_CONTROLS` batch instead, as
   fx8's do; `python3 -I e2e/apt-oracle/gen_shapes.py --from-controls
   internal/gpg/testdata/aptshapes` then writes those batches alone, from
   the committed controls, and every other fixture keeps its bytes (so in
   step 4 every other row must come out unchanged).
3. Re-measure: `bash e2e/apt-oracle/run.sh --record`.
4. Review `git diff --no-ext-diff internal/gpg/testdata/aptshapes/verdicts.txt`:
   the new row, and no other row changed. An old row that changed is apt
   changing or a broken builder: find out which before going on.
5. Run the verifier's table test (`go test ./internal/gpg`, once the
   hardening plan's Task 6 has added it): it holds the cache to the new
   row, or the shape goes in its deviations, with a reason.
6. Commit the generator, the fixtures and `verdicts.txt` together.

Never edit a fixture by hand: the fixtures and `verdicts.txt` are one
measurement.

## What the oracle checks

`run.sh` starts one container per image, all at once (`docker run --rm
--init --network none`, the corpus mounted read-only at `/fx`, 600 seconds
each at most). In each, `in-container.sh` builds every shape's flat
`file:` repository in a temporary directory — the shape's file as
`InRelease`, or as `Release.gpg` beside `Release`, and an empty `Packages`
— and runs `apt-get update` on that repository alone, with
`signed-by=/fx/keys.gpg`. `run.sh` checks that every image measured every
shape once, prints the verdicts as one table (shape by image) and compares
them with `verdicts.txt`: each difference is printed as **apt changed**,
with the first `E:` or `W:` line apt printed for that shape, and the run
fails.

It checks apt, not the cache: that the six images' apt still gives the
recorded verdicts on these bytes. Holding the cache to `verdicts.txt` is
the job of the verifier's Go table test over the same files, which the
hardening plan adds in its Tasks 6 and 7
(`TestVerifier_FollowsTheAptShapeTable`): the cache refuses a shape when
any of the six apt refuses it, and accepts a shape all six accept, but for
the exceptions the test lists, each with its reason.

That guarantee — the cache adopts no anchor a supported apt refuses, but
for the one known-unsafe shape the test lists (`x3i_sha1_only`: weak
digests are outside the plan) — is only as wide as the corpus: it holds
for these 155 shapes. An anchor shaped in a way the corpus lacks is
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

## The images

The columns of `verdicts.txt`, in its order (`COLUMNS` in `run.sh`), as
measured on 2026-10-29:

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
verdict, and what apt said. Run that image alone again to rule out a
flake (`E2E_APT_ORACLE_IMAGES=<image> make e2e-apt-oracle`); then:

1. Record it: `bash e2e/apt-oracle/run.sh --record`, and check that
   `verdicts.txt`'s diff holds only the changed cells.
2. Update the shape's row in the record that succeeds the hardening
   plan's Appendix A (the plan itself stays as measured on 2026-10-28),
   and add a row to *Results* below with the date.
3. Run the verifier's table test: the cache's verdict may now be a
   deviation to list (a refusal all six apt now accept), or a refusal to
   add (a shape some apt now refuses).

A difference that names a shape with no row in `verdicts.txt`, or a row
with no shape, means the corpus and `verdicts.txt` are out of step:
regenerate and re-record them together (*Adding a shape*).

## Results

| Date | Tree | Result | Time |
|---|---|---|---|
| 2026-10-29, 03:45 UTC (the hardening plan's Task 5) | the commit that adds the corpus, on 7c3aca0 | `--record`: 135 shapes in 6 images, every verdict the one Appendix A of the plan gives (2026-10-28); then `make e2e-apt-oracle`: PASS. apt 2.2.4, 2.4.14, 2.8.3, 2.6.1, 3.0.3 and 3.2.0 (*The images*) | 11 s each run, images cached |
| 2026-10-29, 05:38 UTC (ruling HD-13, after the plan's Task 6) | the commit that adds batch fx8, on 80af12b | `--from-controls` wrote fx8's 20 shapes and nothing else; `--record`: 155 shapes in 6 images, the 135 earlier rows unchanged and the 20 new ones as Task 6 measured them; then `make e2e-apt-oracle`: PASS. The same apt and verifiers | 12 s each run, images cached |
