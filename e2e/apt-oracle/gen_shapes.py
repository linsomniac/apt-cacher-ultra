"""The apt-shape corpus: signed anchors in the shapes apt is measured on.

Writes into OUT (internal/gpg/testdata/aptshapes in the tree):

- one ``<name>.InRelease`` (inline) or ``<name>.Release.gpg`` (detached)
  per shape; a detached shape signs the shared ``Release``, an inline
  shape carries its signed text in its own file;
- ``Release``: a flat repository's ``Release``, with a fixed ``Date:`` in
  the past and the SHA256 line of an empty ``Packages`` (the oracle
  writes that file beside it);
- ``keys.gpg``: the public keys A, E and S in binary, the clients'
  ``signed-by`` in the oracle and the host keyring of the verifier's table
  test (Task 6 of the plan named below).

``verdicts.txt`` beside them is not written here: it is apt's measurement
of these bytes (``bash e2e/apt-oracle/run.sh --record``).

The keys are made in a temporary GnuPG home, deleted at exit; no secret
key leaves it, and ``keys.gpg`` is checked to hold public packets only.

- A, RSA-3072, signs: the repository's key. Every shape carries a
  signature by A but ``i_expired_only`` (E's) and ``x3i_subkey_good``
  (S's subkey's).
- B, RSA-3072, signs, never exported: a key the clients do not know, for
  an extra signature apt ignores (``i_unknown_*``, ``d_unknown_goodfirst``,
  ``x*_twoblocks_good_unknown``) and a garbage packet naming a key apt
  lacks (``xi_good_plus_v3garbage_B``).
- E, RSA-3072, signs, made at 2020-01-01 with gpg's --faked-system-time
  and valid one year (gpg's "1y", 365 days: it expired at 2020-12-31).
  Its signatures are made at 2020-06-01, inside that year: a key in
  ``signed-by`` that has expired (``i_expired_*``,
  ``i_good_plus_expired_good``; apt takes EXPKEYSIG beside a good
  signature).
- S, an RSA-3072 primary that only certifies, with an RSA-3072 signing
  subkey: a signature by a subkey (``x3i_subkey_*``).

The 135 shapes are the planning measurement's (2026-10-28; DN-2 and
Appendix A of docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md),
by the same names and built as its throwaway generators built them (the
plan's PE/gen.py, gen2.py to gen6.py with pgp.py, and rereview/gen7.py,
kept outside the tree): the controller's batch fx, the security review's
fx2 to fx6 and its re-review's fx7, one function each below. Each
planning batch made keys of its own; here the batches share A, B, E and
S and one ``Release``, so their controls are byte-identical: ``i_good``,
``i_crc_ok_rearmored`` and every ``x*i_control_good`` are one file (gpg's
armor and armor() below write the same bytes), ``d_good`` and every
``x*d_control_good`` another. The second block of the two-block shapes
is a second gpg signing (sig_a2, det_a2), as in the planning; one made in
the same second as the first is byte-identical to it (the planning's
was), and either way it is a second good signature.

AIDEV-NOTE: the fixtures and verdicts.txt are one measurement. Each run
makes new keys, new signatures and new random bytes (the v3 garbage
packets), so a regenerated corpus must be re-measured before it is
committed: regenerate, run ``bash e2e/apt-oracle/run.sh --record``,
review verdicts.txt's diff (a changed row is a finding: see
e2e/apt-oracle/README.md), and commit fixtures and verdicts.txt together.
Never edit a fixture by hand.

Usage: python3 -I e2e/apt-oracle/gen_shapes.py OUT
"""

from __future__ import annotations

import base64
import calendar
import hashlib
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from email.utils import formatdate
from pathlib import Path
from typing import Literal

# A signature subpacket: (type, critical, data).
Subpacket = tuple[int, bool, bytes]
# Shapes of one form: name -> the bytes of its InRelease or Release.gpg.
Shapes = dict[str, bytes]

# A Date in the past: apt refuses a Release dated in the future, and there
# is no Valid-Until, so the corpus does not age.
RELEASE_DATE = formatdate(calendar.timegm((2026, 10, 1, 0, 0, 0)), usegmt=True)
PACKAGES = b""
# The planning generators' Release, field for field ("Origin: oracle" first:
# the xi_text_* shapes rewrite that line).
RELEASE = (
    "Origin: oracle\nLabel: oracle\nSuite: test\nCodename: test\n"
    f"Date: {RELEASE_DATE}\nArchitectures: amd64\n"
    "SHA256:\n"
    f" {hashlib.sha256(PACKAGES).hexdigest()} {len(PACKAGES)} Packages\n"
).encode()
# What a bad signature signs instead of Release.
OTHER_TEXT = b"some other text\n"
# Key E: made at E_MADE and valid one year; signs at E_SIGNS.
E_MADE = "20200101T000000!"
E_SIGNS = "20200601T000000!"

NAME_RE = re.compile(r"[A-Za-z0-9_]+")
# The files of a corpus directory, besides the shapes'.
CORPUS_FILES = ("keys.gpg", "Release", "verdicts.txt")
SHAPE_SUFFIXES = (".InRelease", ".Release.gpg", ".Release")


# --- OpenPGP packets and armor (PE/pgp.py, typed) ---------------------------


def crc24(data: bytes) -> int:
    """The armor checksum (RFC 4880, 6.1)."""
    crc = 0xB704CE
    for b in data:
        crc ^= b << 16
        for _ in range(8):
            crc <<= 1
            if crc & 0x1000000:
                crc ^= 0x1864CFB
    return crc & 0xFFFFFF


def new_len(n: int) -> bytes:
    """A new-format packet length (a subpacket's length is the same octets)."""
    if n < 192:
        return bytes([n])
    if n < 8384:
        n -= 192
        return bytes([(n >> 8) + 192, n & 0xFF])
    return b"\xff" + struct.pack(">I", n)


def packet(tag: int, body: bytes) -> bytes:
    """A new-format packet."""
    return bytes([0xC0 | tag]) + new_len(len(body)) + body


def split_packets(data: bytes) -> list[tuple[int, bytes]]:
    """[(tag, body)] of a stream of old- or new-format packets."""
    out: list[tuple[int, bytes]] = []
    i = 0
    while i < len(data):
        h = data[i]
        if not h & 0x80:
            raise ValueError(f"bad tag byte {h:#x} at {i}")
        if h & 0x40:
            tag = h & 0x3F
            l0 = data[i + 1]
            if l0 < 192:
                n, hl = l0, 2
            elif l0 < 224:
                n, hl = ((l0 - 192) << 8) + data[i + 2] + 192, 3
            elif l0 == 255:
                n, hl = int.from_bytes(data[i + 2 : i + 6], "big"), 6
            else:
                raise ValueError("partial lengths unsupported")
        else:
            tag = (h >> 2) & 0x0F
            lt = h & 3
            if lt == 0:
                n, hl = data[i + 1], 2
            elif lt == 1:
                n, hl = int.from_bytes(data[i + 1 : i + 3], "big"), 3
            elif lt == 2:
                n, hl = int.from_bytes(data[i + 1 : i + 5], "big"), 5
            else:
                n, hl = len(data) - i - 1, 1
        out.append((tag, data[i + hl : i + hl + n]))
        i += hl + n
    return out


def subpackets(area: bytes) -> list[Subpacket]:
    """The subpackets of a signature's subpacket area."""
    out: list[Subpacket] = []
    i = 0
    while i < len(area):
        l0 = area[i]
        if l0 < 192:
            n, hl = l0, 1
        elif l0 < 255:
            n, hl = ((l0 - 192) << 8) + area[i + 1] + 192, 2
        else:
            n, hl = int.from_bytes(area[i + 1 : i + 5], "big"), 5
        body = area[i + hl : i + hl + n]
        out.append((body[0] & 0x7F, bool(body[0] & 0x80), body[1:]))
        i += hl + n
    return out


def build_subpackets(sps: list[Subpacket]) -> bytes:
    """A subpacket area (RFC 4880, 5.2.3.1)."""
    out = b""
    for typ, crit, data in sps:
        body = bytes([typ | (0x80 if crit else 0)]) + data
        out += new_len(len(body)) + body
    return out


@dataclass(frozen=True)
class V4Sig:
    """A v4 signature packet's body, cut where the unhashed area can change."""

    head: bytes  # version, signature type, public-key and hash algorithms
    hashed_raw: bytes  # the hashed subpacket area, verbatim
    unhashed: list[Subpacket]
    rest: bytes  # the hash's left 16 bits and the signature's MPIs


def parse_v4_sig(body: bytes) -> V4Sig:
    """Cuts a v4 signature packet's body (RFC 4880, 5.2.3)."""
    if body[0] != 4:
        raise ValueError(f"not a v4 signature: version {body[0]}")
    hl = int.from_bytes(body[4:6], "big")
    ul = int.from_bytes(body[6 + hl : 8 + hl], "big")
    return V4Sig(
        head=body[:4],
        hashed_raw=body[6 : 6 + hl],
        unhashed=subpackets(body[8 + hl : 8 + hl + ul]),
        rest=body[8 + hl + ul :],
    )


def build_v4_sig(s: V4Sig, unhashed: list[Subpacket]) -> bytes:
    """s's body with another unhashed area (the signed bytes are unchanged)."""
    u = build_subpackets(unhashed)
    hashed_len = struct.pack(">H", len(s.hashed_raw))
    return s.head + hashed_len + s.hashed_raw + struct.pack(">H", len(u)) + u + s.rest


def armor(
    packets: bytes,
    crc: Literal["ok", "forged"] | None = "ok",
    headers: str = "",
    width: int = 64,
) -> bytes:
    """A PGP SIGNATURE armor block around packets.

    crc: "ok", "forged" (the checksum's first character changed) or None
    (no CRC line). headers: armor header lines, each ending in a newline.
    """
    body = base64.b64encode(packets).decode()
    lines = [body[i : i + width] for i in range(0, len(body), width)]
    c = base64.b64encode(crc24(packets).to_bytes(3, "big")).decode()
    if crc == "forged":
        c = ("B" if c[0] != "B" else "C") + c[1:]
    out = "-----BEGIN PGP SIGNATURE-----\n" + headers + "\n" + "\n".join(lines) + "\n"
    if crc is not None:
        out += "=" + c + "\n"
    return (out + "-----END PGP SIGNATURE-----\n").encode()


def mpi(b: bytes) -> bytes:
    """An OpenPGP multiprecision integer."""
    v = int.from_bytes(b, "big")
    return struct.pack(">H", v.bit_length()) + v.to_bytes(
        (v.bit_length() + 7) // 8, "big"
    )


def v3_sig_garbage(keyid: bytes, sigtype: int = 0x01, hashalgo: int = 8) -> bytes:
    """A v3 RSA signature packet naming keyid, its MPI random (never valid)."""
    body = (
        bytes([3, 5, sigtype])
        + struct.pack(">I", 1700000000)
        + keyid
        + bytes([1, hashalgo])
        + b"\x12\x34"
    )
    body += mpi(b"\x7f" + os.urandom(383))
    return packet(2, body)


def sig_packets(clearsigned: bytes) -> bytes:
    """The binary signature packets of a clearsigned message."""
    armored = clearsigned[clearsigned.index(b"-----BEGIN PGP SIGNATURE-----") :]
    lines = armored.decode().splitlines()
    i = lines.index("")  # the end of the armor headers
    b64 = "".join(
        line
        for line in lines[i + 1 :]
        if not line.startswith("=") and not line.startswith("-----")
    )
    return base64.b64decode(b64)


def one_signature(sig: bytes) -> bytes:
    """The body of sig, which must be exactly one signature packet."""
    pkts = split_packets(sig)
    if len(pkts) != 1 or pkts[0][0] != 2:
        raise ValueError(f"want one signature packet, got tags {[t for t, _ in pkts]}")
    return pkts[0][1]


def rewrite_issuer(sig: bytes, keyid: bytes | None) -> bytes:
    """Replaces (keyid) or drops (None) the unhashed issuer subpacket 16."""
    s = parse_v4_sig(one_signature(sig))
    un = [sp for sp in s.unhashed if sp[0] != 16]
    if keyid is not None:
        un.append((16, False, keyid))
    return packet(2, build_v4_sig(s, un))


def add_unhashed(sig: bytes, *sps: Subpacket) -> bytes:
    """Appends subpackets to the unhashed area."""
    s = parse_v4_sig(one_signature(sig))
    return packet(2, build_v4_sig(s, s.unhashed + list(sps)))


def set_algo(sig: bytes, pk: int | None = None, h: int | None = None) -> bytes:
    """Overwrites the public-key (pk) or hash (h) algorithm octet."""
    b = bytearray(one_signature(sig))
    if pk is not None:
        b[2] = pk
    if h is not None:
        b[3] = h
    return packet(2, bytes(b))


# --- Byte surgery on armored text --------------------------------------------


def replace(data: bytes, old: bytes, new: bytes, count: int = -1) -> bytes:
    """bytes.replace, failing when old is absent: no shape silently stays
    the control it was cut from."""
    if old not in data:
        raise ValueError(f"{old!r} not found")
    return data.replace(old, new, count)


def junk_before_end(arm: bytes, lines: bytes) -> bytes:
    """Lines inserted before the END line (PE/gen2.py)."""
    return replace(
        arm,
        b"\n-----END PGP SIGNATURE-----",
        b"\n" + lines + b"-----END PGP SIGNATURE-----",
    )


def hdrjunk(arm: bytes) -> bytes:
    """A BEGIN line and a junk line before the armor (PE/gen2.py)."""
    return b"-----BEGIN PGP SIGNATURE-----\njunk\n" + arm


def first_b64_line(arm: bytes, fn: Callable[[bytes], bytes]) -> bytes:
    """The armor with fn applied to its first base64 line (PE/gen4.py)."""
    ls = arm.split(b"\n")
    i = ls.index(b"") + 1
    ls[i] = fn(ls[i])
    return b"\n".join(ls)


def armor_with_header(packets: bytes, hdr: bytes) -> bytes:
    """The armor with raw header bytes after its BEGIN line (PE/gen5.py)."""
    return replace(
        armor(packets),
        b"-----BEGIN PGP SIGNATURE-----\n",
        b"-----BEGIN PGP SIGNATURE-----\n" + hdr,
        1,
    )


# --- GnuPG --------------------------------------------------------------------


def key_created(status: bytes, kind: str) -> str:
    """The fingerprint of gpg's KEY_CREATED status line of kind (P or S)."""
    for line in status.decode().splitlines():
        f = line.split()
        if len(f) >= 4 and f[:3] == ["[GNUPG:]", "KEY_CREATED", kind]:
            return f[3]
    raise RuntimeError(f"gpg printed no KEY_CREATED {kind}")


class GnuPG:
    """gpg in a temporary home of its own, which close() deletes."""

    def __init__(self) -> None:
        self.home = Path(tempfile.mkdtemp(prefix="apt-shapes-gnupg-"))
        self.home.chmod(0o700)

    def run(self, *args: str, inp: bytes | None = None) -> bytes:
        """gpg's stdout; raises with its stderr when it fails."""
        cmd = ["gpg", "--homedir", str(self.home), "--batch", "--yes"]
        cmd += ["--pinentry-mode", "loopback", "--passphrase", "", *args]
        proc = subprocess.run(cmd, input=inp, capture_output=True, check=False)
        if proc.returncode != 0:
            err = proc.stderr.decode(errors="replace").strip()
            raise RuntimeError(f"gpg {' '.join(args)}: exit {proc.returncode}: {err}")
        return proc.stdout

    def gen_key(self, uid: str, usage: str, expire: str, faked: str = "") -> str:
        """An RSA-3072 primary key without passphrase; its fingerprint."""
        fake = ["--faked-system-time", faked] if faked else []
        out = self.run(
            *fake, "--status-fd", "1", "--quick-gen-key", uid, "rsa3072", usage, expire
        )
        return key_created(out, "P")

    def add_subkey(self, primary: str, usage: str) -> str:
        """An RSA-3072 subkey of primary that never expires; its fingerprint."""
        out = self.run(
            "--status-fd", "1", "--quick-add-key", primary, "rsa3072", usage, "never"
        )
        return key_created(out, "S")

    def clearsign(
        self,
        key: str,
        text: bytes,
        *extra: str,
        digest: str = "SHA256",
        faked: str = "",
    ) -> bytes:
        """text clearsigned by key (gpg's whole output)."""
        fake = ["--faked-system-time", faked] if faked else []
        return self.run(
            *fake,
            "--local-user",
            key,
            "--digest-algo",
            digest,
            *extra,
            "--clearsign",
            inp=text,
        )

    def detach_sign(self, key: str, text: bytes) -> bytes:
        """key's binary detached signature of text."""
        return self.run(
            "--local-user", key, "--digest-algo", "SHA256", "--detach-sign", inp=text
        )

    def export(self, *fprs: str) -> bytes:
        """The binary export of the public keys fprs."""
        return self.run("--export", *fprs)

    def close(self) -> None:
        """Stops the home's agent and deletes the home (its secret keys with
        it) and the socket dir gpg made for it under /run/user."""
        for args in (["--kill", "all"], ["--remove-socketdir"]):
            try:
                subprocess.run(
                    ["gpgconf", "--homedir", str(self.home), *args],
                    capture_output=True,
                    check=False,
                )
            except OSError:
                pass  # no gpgconf: the home is still deleted below
        shutil.rmtree(self.home, ignore_errors=True)


@dataclass(frozen=True)
class Keys:
    """The fingerprints of the four keys (see the module comment)."""

    a: str
    b: str
    e: str
    s: str
    s_sub: str  # S's signing subkey


def make_keys(g: GnuPG) -> Keys:
    """Keys A, B, E and S."""
    a = g.gen_key("Apt-shape A <a@example.invalid>", "sign", "never")
    b = g.gen_key("Apt-shape B <b@example.invalid>", "sign", "never")
    e = g.gen_key("Apt-shape E <e@example.invalid>", "sign", "1y", faked=E_MADE)
    s = g.gen_key("Apt-shape S <s@example.invalid>", "cert", "never")
    return Keys(a=a, b=b, e=e, s=s, s_sub=g.add_subkey(s, "sign"))


def v4_fingerprint(key_body: bytes) -> str:
    """A v4 key packet's fingerprint (RFC 4880, 12.2)."""
    h = hashlib.sha1(b"\x99" + struct.pack(">H", len(key_body)) + key_body)
    return h.hexdigest().upper()


def check_keyring(keyring: bytes, want: set[str]) -> None:
    """keyring holds public packets only, of exactly the keys want."""
    public = {2, 6, 13, 14}  # signature, public key, user ID, public subkey
    got: set[str] = set()
    for tag, body in split_packets(keyring):
        if tag not in public:
            raise RuntimeError(f"keys.gpg: packet tag {tag} is not a public packet")
        if tag in (6, 14):
            if body[0] != 4:
                raise RuntimeError(f"keys.gpg: a v{body[0]} key (the check knows v4)")
            got.add(v4_fingerprint(body))
    if got != want:
        raise RuntimeError(f"keys.gpg holds {sorted(got)}, want {sorted(want)}")


# --- The shapes ---------------------------------------------------------------


@dataclass(frozen=True)
class Material:
    """What the shapes are cut from: signatures of RELEASE, good and bad."""

    good: bytes  # RELEASE clearsigned by A, as gpg wrote it
    head: bytes  # good up to its signature armor: the signed text and its header
    head_sha1: bytes  # the same from a SHA-1 clearsigning ("Hash: SHA1")
    sig_a: bytes  # the signature packet of good
    sig_a2: bytes  # a second signing of RELEASE by A
    sig_a_other: bytes  # A's signature of OTHER_TEXT: a bad signature here
    sig_a_crit: bytes  # A's signature of RELEASE with a critical notation
    sig_a_sha1: bytes  # A's SHA-1 signature of RELEASE
    sig_b: bytes  # B's signature of RELEASE
    sig_e: bytes  # E's signature of RELEASE, made inside E's validity
    sig_e_bad: bytes  # E's signature of another text
    sig_s: bytes  # S's subkey's signature of RELEASE
    sig_s_other: bytes  # S's subkey's signature of OTHER_TEXT
    dash_escaped_end: bytes  # RELEASE + an END line, clearsigned by A
    det_a: bytes  # A's binary detached signature of RELEASE
    det_a2: bytes  # a second one
    det_a_other: bytes  # A's detached signature of OTHER_TEXT
    det_b: bytes  # B's detached signature of RELEASE
    keyid_a: bytes
    keyid_b: bytes


def make_material(g: GnuPG, k: Keys) -> Material:
    """Signs what the shapes need, in the planning generators' order."""
    good = g.clearsign(k.a, RELEASE)
    sig_a2 = sig_packets(g.clearsign(k.a, RELEASE))
    sig_a_other = sig_packets(g.clearsign(k.a, OTHER_TEXT))
    sig_b = sig_packets(g.clearsign(k.b, RELEASE))
    sig_e = sig_packets(g.clearsign(k.e, RELEASE, faked=E_SIGNS))
    sig_e_bad = sig_packets(g.clearsign(k.e, b"x\n", faked=E_SIGNS))
    sig_a_crit = sig_packets(
        g.clearsign(k.a, RELEASE, "--sig-notation", "!crit@example.invalid=1")
    )
    det_a = g.detach_sign(k.a, RELEASE)
    det_a2 = g.detach_sign(k.a, RELEASE)
    det_a_other = g.detach_sign(k.a, OTHER_TEXT)
    det_b = g.detach_sign(k.b, RELEASE)
    sha1 = g.clearsign(k.a, RELEASE, digest="SHA1")
    sig_a_sha1 = sig_packets(g.clearsign(k.a, RELEASE, digest="SHA1"))
    # "!": the subkey itself, as PE/gen3.py signed.
    sig_s = sig_packets(g.clearsign(k.s_sub + "!", RELEASE))
    sig_s_other = sig_packets(g.clearsign(k.s_sub + "!", OTHER_TEXT))
    dash_escaped_end = g.clearsign(k.a, RELEASE + b"-----END PGP SIGNATURE-----\n")
    begin = b"-----BEGIN PGP SIGNATURE-----"
    return Material(
        good=good,
        head=good[: good.index(begin)],
        head_sha1=sha1[: sha1.index(begin)],
        sig_a=sig_packets(good),
        sig_a2=sig_a2,
        sig_a_other=sig_a_other,
        sig_a_crit=sig_a_crit,
        sig_a_sha1=sig_a_sha1,
        sig_b=sig_b,
        sig_e=sig_e,
        sig_e_bad=sig_e_bad,
        sig_s=sig_s,
        sig_s_other=sig_s_other,
        dash_escaped_end=dash_escaped_end,
        det_a=det_a,
        det_a2=det_a2,
        det_a_other=det_a_other,
        det_b=det_b,
        keyid_a=bytes.fromhex(k.a[-16:]),
        keyid_b=bytes.fromhex(k.b[-16:]),
    )


def batch_fx(m: Material) -> tuple[Shapes, Shapes]:
    """The controller's 47 shapes (PE/gen.py; apt's verdicts PE/table.txt)."""
    good, head, sig_a, det_a = m.good, m.head, m.sig_a, m.det_a
    end = b"\n-----END PGP SIGNATURE-----"
    inline = {
        "i_good": good,
        "i_trail_lf": good + b"\n",
        "i_trail_ws": good + b"  \n",
        "i_trail_text": good + b"junk\n",
        "i_no_final_nl": good.rstrip(b"\n"),
        "i_end_trailing_ws": good.rstrip(b"\n") + b"  \n",
        "i_end_crlf": good.rstrip(b"\n") + b"\r\n",
        "i_crlf_all": replace(good, b"\n", b"\r\n"),
        "i_lead_nl": b"\n" + good,
        "i_crc_forged": head + armor(sig_a, crc="forged"),
        "i_crc_missing": head + armor(sig_a, crc=None),
        "i_crc_ok_rearmored": head + armor(sig_a),
        "i_line_after_crc": replace(head + armor(sig_a), end, b"\nZm9vYmFy" + end),
        "i_comment_header": head + armor(sig_a, headers="Comment: hello\n"),
        "i_badsig_goodfirst": head + armor(sig_a + m.sig_a_other),
        "i_badsig_badfirst": head + armor(m.sig_a_other + sig_a),
        "i_unknown_goodfirst": head + armor(sig_a + m.sig_b),
        "i_unknown_unknownfirst": head + armor(m.sig_b + sig_a),
        "i_badsig_only": head + armor(m.sig_a_other),
        "i_begin_trailing_ws": replace(
            good,
            b"-----BEGIN PGP SIGNED MESSAGE-----\n",
            b"-----BEGIN PGP SIGNED MESSAGE-----  \n",
            1,
        ),
        "i_blank_in_base64": replace(head + armor(sig_a), b"\n=", b"\n\n=", 1),
        "i_sigbegin_trailing_ws": replace(
            good,
            b"-----BEGIN PGP SIGNATURE-----\n",
            b"-----BEGIN PGP SIGNATURE-----  \n",
            1,
        ),
        "i_dash_escaped_end": m.dash_escaped_end,
        "i_crc_lowercase_eq_ws": replace(head + armor(sig_a), b"\n=", b"\n =", 1),
        "i_expired_good_plus_good": head + armor(m.sig_e + sig_a),
        "i_good_plus_expired_good": head + armor(sig_a + m.sig_e),
        "i_expired_bad_plus_good": head + armor(m.sig_e_bad + sig_a),
        "i_expired_only": head + armor(m.sig_e),
    }
    detached = {
        "d_good": armor(det_a),
        "d_trail_lf": armor(det_a) + b"\n",
        "d_no_final_nl": armor(det_a).rstrip(b"\n"),
        "d_lead_nl": b"\n" + armor(det_a),
        "d_crc_forged": armor(det_a, crc="forged"),
        "d_crc_missing": armor(det_a, crc=None),
        "d_badsig_goodfirst": armor(det_a + m.det_a_other),
        "d_badsig_badfirst": armor(m.det_a_other + det_a),
        "d_unknown_goodfirst": armor(det_a + m.det_b),
        "d_binary": det_a,
        "d_twoblocks": armor(det_a) + armor(m.det_a_other),
        "d_end_trailing_ws": armor(det_a).rstrip(b"\n") + b"  \n",
        "d_end_crlf": armor(det_a).rstrip(b"\n") + b"\r\n",
        "d_crlf_all": replace(armor(det_a), b"\n", b"\r\n"),
        "d_begin_trailing_ws": replace(
            armor(det_a),
            b"-----BEGIN PGP SIGNATURE-----\n",
            b"-----BEGIN PGP SIGNATURE-----  \n",
            1,
        ),
        "d_comment_header": armor(det_a, headers="Comment: hello\n"),
        "d_line_after_crc": replace(armor(det_a), end, b"\nZm9vYmFy" + end),
        "d_blank_in_base64": replace(armor(det_a), b"\n=", b"\n\n=", 1),
        "d_lead_ws": b"  " + armor(det_a),
    }
    return inline, detached


def batch_fx2(m: Material) -> tuple[Shapes, Shapes]:
    """The security review's first batch, 42 shapes (PE/gen2.py)."""
    head, sig_a, det_a = m.head, m.sig_a, m.det_a
    end = b"\n-----END PGP SIGNATURE-----"
    rnd = bytes.fromhex("0123456789abcdef")  # a key ID no key has
    literal = packet(11, b"b\x00" + b"\x00\x00\x00\x00" + b"hello")
    marker = packet(10, b"PGP")
    control = head + armor(sig_a)
    inline = {
        "xi_control_good": control,
        "xi_twoblocks_goodgood": control + armor(m.sig_a2),
        "xi_twoblocks_good_unknown": control + armor(m.sig_b),
        "xi_twoblocks_good_badsig": control + armor(m.sig_a_other),
        "xi_hdrjunk_begin": head + hdrjunk(armor(sig_a)),
        "xi_endsuffix_nocrc": head
        + junk_before_end(
            armor(sig_a, crc=None), b"-----END PGP SIGNATURE-----x\njunk\n"
        ),
        "xi_endx_nocrc": head
        + junk_before_end(armor(sig_a, crc=None), b"-----END X-----\njunk\n"),
        "xi_hash_sha512": replace(control, b"Hash: SHA256\n", b"Hash: SHA512\n", 1),
        "xi_hash_sha1": replace(control, b"Hash: SHA256\n", b"Hash: SHA1\n", 1),
        "xi_hash_none": replace(control, b"Hash: SHA256\n", b"", 1),
        "xi_cleartext_comment_hdr": replace(
            control, b"Hash: SHA256\n", b"Hash: SHA256\nComment: x\n", 1
        ),
        "xi_badsig_issuer_rewritten": head
        + armor(sig_a + rewrite_issuer(m.sig_a_other, rnd)),
        "xi_badsig_issuer_removed": head
        + armor(sig_a + rewrite_issuer(m.sig_a_other, None)),
        "xi_goodsig_issuer_rewritten": head + armor(rewrite_issuer(sig_a, rnd)),
        "xi_good_plus_v3garbage_A": head + armor(sig_a + v3_sig_garbage(m.keyid_a)),
        "xi_good_plus_v3garbage_B": head + armor(sig_a + v3_sig_garbage(m.keyid_b)),
        "xi_good_plus_unknownpk_A": head
        + armor(sig_a + set_algo(m.sig_a_other, pk=99)),
        "xi_good_plus_unknownhash_A": head
        + armor(sig_a + set_algo(m.sig_a_other, h=100)),
        "xi_good_plus_literal": head + armor(sig_a + literal),
        "xi_good_plus_marker": head + armor(sig_a + marker),
        "xi_crit_plus_good": head + armor(m.sig_a_crit + sig_a),
        "xi_crit_only": head + armor(m.sig_a_crit),
        "xi_blank_after_crc": replace(control, end, b"\n" + end),
        "xi_crc_forged_leading_ws": replace(
            head + armor(sig_a, crc="forged"), b"\n=", b"\n =", 1
        ),
        "xi_crc_trailing_ws": replace(control, end, b"  " + end),
        "xi_end_double_cr": control.rstrip(b"\n") + b"\r\r\n",
        "xi_text_dash_escaped_extra": replace(
            control, b"\nOrigin: oracle\n", b"\n- Origin: oracle\n", 1
        ),
        "xi_text_trailing_ws": replace(
            control, b"\nOrigin: oracle\n", b"\nOrigin: oracle   \n", 1
        ),
        "xi_unknown_armor_header": head + armor(sig_a, headers="Foo: bar\n"),
        "xi_armor_hdr_nocolon": head + armor(sig_a, headers="junk\n"),
        "xi_two_crc_lines": replace(control, end, b"\n=AAAA" + end),
    }
    detached = {
        "xd_control_good": armor(det_a),
        "xd_twoblocks_goodgood": armor(det_a) + armor(m.det_a2),
        "xd_twoblocks_good_unknown": armor(det_a) + armor(m.det_b),
        "xd_hdrjunk_begin": hdrjunk(armor(det_a)),
        "xd_endsuffix_nocrc": junk_before_end(
            armor(det_a, crc=None), b"-----END PGP SIGNATURE-----x\njunk\n"
        ),
        "xd_endx_nocrc": junk_before_end(
            armor(det_a, crc=None), b"-----END X-----\njunk\n"
        ),
        "xd_badsig_issuer_rewritten": armor(det_a + rewrite_issuer(m.det_a_other, rnd)),
        "xd_good_plus_v3garbage_A": armor(
            det_a + v3_sig_garbage(m.keyid_a, sigtype=0x00)
        ),
        "xd_good_plus_literal": armor(det_a + literal),
        "xd_blank_after_crc": replace(armor(det_a), end, b"\n" + end),
        "xd_end_double_cr": armor(det_a).rstrip(b"\n") + b"\r\r\n",
    }
    return inline, detached


def batch_fx3(m: Material) -> tuple[Shapes, Shapes]:
    """Issuer fingerprints, subkeys, SHA-1, END/CR variants, BOM (PE/gen3.py)."""
    head, sig_a, det_a = m.head, m.sig_a, m.det_a
    # An issuer fingerprint (v4) no key has.
    rnd_fpr = bytes([4]) + bytes.fromhex("00112233445566778899aabbccddeeff01234567")
    control = head + armor(sig_a)
    no_blank = (
        b"-----BEGIN PGP SIGNATURE-----\n\n",
        b"-----BEGIN PGP SIGNATURE-----\n",
    )
    inline = {
        "x3i_control_good": control,
        "x3i_badsig_unhashed_fpr_added": head
        + armor(sig_a + add_unhashed(m.sig_a_other, (33, False, rnd_fpr))),
        "x3i_goodsig_unhashed_fpr_added": head
        + armor(add_unhashed(sig_a, (33, False, rnd_fpr))),
        "x3i_subkey_good": head + armor(m.sig_s),
        "x3i_subkey_bad_plus_good": head + armor(m.sig_s_other + sig_a),
        "x3i_sha1_plus_good": head + armor(m.sig_a_sha1 + sig_a),
        "x3i_sha1_only": m.head_sha1 + armor(m.sig_a_sha1),
        "x3i_end_cr_eof": control.rstrip(b"\n") + b"\r",
        "x3i_end_tab": control.rstrip(b"\n") + b"\t\n",
        "x3i_after_end_cr_line": control + b"\r",
        "x3i_hdr_blank_ws": replace(
            head + armor(sig_a, headers="Comment: x\n"),
            b"Comment: x\n\n",
            b"Comment: x\n   \n",
            1,
        ),
        "x3i_armor_no_blank": replace(control, *no_blank, 1),
    }
    detached = {
        "x3d_control_good": armor(det_a),
        "x3d_badsig_unhashed_fpr_added": armor(
            det_a + add_unhashed(m.det_a_other, (33, False, rnd_fpr))
        ),
        "x3d_bom": b"\xef\xbb\xbf" + armor(det_a),
        "x3d_end_cr_eof": armor(det_a).rstrip(b"\n") + b"\r",
        "x3d_armor_no_blank": replace(armor(det_a), *no_blank, 1),
    }
    return inline, detached


def batch_fx4(m: Material) -> tuple[Shapes, Shapes]:
    """Armor header grammar and base64 leniency (PE/gen4.py)."""
    head, sig_a, det_a = m.head, m.sig_a, m.det_a

    def star(line: bytes) -> bytes:
        return line[:10] + b"*" + line[10:]

    inline = {
        "x4i_control_good": head + armor(sig_a),
        "x4i_hdr_colon_nospace": head + armor(sig_a, headers="Junk:junk\n"),
        "x4i_hdr_leading_space": head + armor(sig_a, headers=" Comment: x\n"),
        "x4i_b64_leading_space": head
        + first_b64_line(armor(sig_a), lambda line: b" " + line),
        "x4i_b64_invalid_char": head + first_b64_line(armor(sig_a), star),
        "x4i_b64_line_76": head + armor(sig_a, width=76),
    }
    detached = {
        "x4d_control_good": armor(det_a),
        "x4d_hdr_colon_nospace": armor(det_a, headers="Junk:junk\n"),
        "x4d_b64_invalid_char": first_b64_line(armor(det_a), star),
    }
    return inline, detached


def batch_fx5(m: Material) -> tuple[Shapes, Shapes]:
    """Armor header grammar details, inline only (PE/gen5.py)."""
    head, sig_a = m.head, m.sig_a
    inline = {
        "x5i_control_good": head + armor(sig_a),
        "x5i_hdr_empty_value": head + armor_with_header(sig_a, b"Comment:\n"),
        "x5i_hdr_ctrl": head + armor_with_header(sig_a, b"Comment: a\x01b\n"),
        "x5i_hdr_space_in_key": head + armor_with_header(sig_a, b"Com ment: x\n"),
        "x5i_hdr_no_key": head + armor_with_header(sig_a, b": x\n"),
        # U+00E9 in UTF-8.
        "x5i_hdr_utf8": head + armor_with_header(sig_a, b"Comment: \xc3\xa9\n"),
        "x5i_hdr_tab_after_colon": head + armor_with_header(sig_a, b"Comment:\tx\n"),
        "x5i_hdr_version_multi": head
        + armor_with_header(sig_a, b"Version: GnuPG v1\nComment: a\nComment: b\n"),
    }
    return inline, {}


def batch_fx6(m: Material) -> tuple[Shapes, Shapes]:
    """Packets that are not signatures after a good one, inline (PE/gen6.py)."""
    head, sig_a = m.head, m.sig_a
    inline = {
        "x6i_control_good": head + armor(sig_a),
        "x6i_good_plus_emptyliteral": head
        + armor(sig_a + packet(11, b"b\x00\x00\x00\x00\x00")),
        "x6i_good_plus_userid": head
        + armor(sig_a + packet(13, b"x <x@example.invalid>")),
        "x6i_good_plus_unknowntag": head + armor(sig_a + packet(60, b"hello")),
        "x6i_good_plus_trailing_zero": head + armor(sig_a + b"\x00"),
    }
    return inline, {}


def batch_fx7(m: Material) -> tuple[Shapes, Shapes]:
    """Text after the END marker on its line; a BEGIN line with two CRs
    (PE/rereview/gen7.py; apt's verdicts PE/rereview/table-fx7.txt)."""
    head, sig_a, det_a = m.head, m.sig_a, m.det_a
    end = b"-----END PGP SIGNATURE-----"
    begin = b"-----BEGIN PGP SIGNATURE-----"
    control = head + armor(sig_a)
    inline = {
        "x7i_control_good": control,
        "x7i_end_trailing_text": replace(control, end + b"\n", end + b"junk\n"),
        "x7i_end_trailing_space_text": replace(control, end + b"\n", end + b" junk\n"),
        "x7i_sigbegin_double_cr": replace(control, begin + b"\n", begin + b"\r\r\n", 1),
    }
    detached = {
        "x7d_control_good": armor(det_a),
        "x7d_end_trailing_text": replace(armor(det_a), end + b"\n", end + b"junk\n"),
        "x7d_begin_double_cr": replace(
            armor(det_a), begin + b"\n", begin + b"\r\r\n", 1
        ),
    }
    return inline, detached


BATCHES: list[Callable[[Material], tuple[Shapes, Shapes]]] = [
    batch_fx,
    batch_fx2,
    batch_fx3,
    batch_fx4,
    batch_fx5,
    batch_fx6,
    batch_fx7,
]


def build_shapes(m: Material) -> tuple[Shapes, Shapes]:
    """Every batch's shapes, inline and detached; names are unique."""
    inline: Shapes = {}
    detached: Shapes = {}
    for batch in BATCHES:
        batch_inline, batch_detached = batch(m)
        for shapes, into in ((batch_inline, inline), (batch_detached, detached)):
            for name, data in shapes.items():
                if not NAME_RE.fullmatch(name):
                    raise ValueError(f"shape name {name!r}: want [A-Za-z0-9_]+")
                if name in inline or name in detached:
                    raise ValueError(f"shape {name} is defined twice")
                into[name] = data
    return inline, detached


# --- The corpus directory -------------------------------------------------------


def corpus_entries(out: Path) -> list[Path]:
    """OUT's corpus files; refuses anything else in it."""
    if not out.exists():
        return []
    if not out.is_dir():
        raise SystemExit(f"{out}: not a directory")
    entries: list[Path] = []
    for p in sorted(out.iterdir()):
        ours = p.name in CORPUS_FILES or p.name.endswith(SHAPE_SUFFIXES)
        if not ours or not p.is_file() or p.is_symlink():
            raise SystemExit(
                f"{out}: {p.name} is not a corpus file; OUT holds the corpus only"
            )
        entries.append(p)
    return entries


def write_corpus(out: Path, inline: Shapes, detached: Shapes, keyring: bytes) -> None:
    """Replaces OUT's fixtures (verdicts.txt is left as it is)."""
    for p in corpus_entries(out):
        if p.name != "verdicts.txt":
            p.unlink()
    out.mkdir(parents=True, exist_ok=True)
    (out / "Release").write_bytes(RELEASE)
    (out / "keys.gpg").write_bytes(keyring)
    for name, data in inline.items():
        (out / f"{name}.InRelease").write_bytes(data)
    for name, data in detached.items():
        (out / f"{name}.Release.gpg").write_bytes(data)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: python3 -I e2e/apt-oracle/gen_shapes.py OUT", file=sys.stderr)
        return 2
    out = Path(argv[1])
    corpus_entries(out)  # refuse a wrong OUT before making keys
    g = GnuPG()
    try:
        keys = make_keys(g)
        material = make_material(g, keys)
        keyring = g.export(keys.a, keys.e, keys.s)
    finally:
        g.close()
    check_keyring(keyring, {keys.a, keys.e, keys.s, keys.s_sub})
    inline, detached = build_shapes(material)
    write_corpus(out, inline, detached, keyring)
    total = len(inline) + len(detached)
    print(
        f"wrote {total} shapes ({len(inline)} inline, {len(detached)} detached), "
        f"Release and keys.gpg (A, E, S) to {out}"
    )
    if (out / "verdicts.txt").exists():
        print(
            "verdicts.txt measured the old fixtures: re-measure with "
            "bash e2e/apt-oracle/run.sh --record"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
