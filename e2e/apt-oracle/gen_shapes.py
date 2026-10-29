"""The apt-shape corpus: signed anchors in the shapes apt is measured on.

Writes into OUT (internal/gpg/testdata/aptshapes in the tree):

- one ``<name>.InRelease`` (inline) or ``<name>.Release.gpg`` (detached)
  per shape; a detached shape signs the shared ``Release``, or the
  ``<name>.Release`` written beside it when it has one of its own; an
  inline shape carries its signed text in its own file;
- ``Release``: a flat repository's ``Release``, with a fixed ``Date:`` in
  the past and the SHA256 line of an empty ``Packages`` (the oracle
  writes that file beside it);
- ``keys.gpg``: the public keys A, E, S and O in binary, the clients'
  ``signed-by`` in the oracle and the host keyring of the verifier's table
  test (Task 6 of the plan named below).

``verdicts.txt`` and ``classes.txt`` beside them are not written here: they
are apt's measurement of these bytes (``bash e2e/apt-oracle/run.sh
--record``).

The keys are made in a temporary GnuPG home, deleted when the run ends:
on an error, Ctrl-C, SIGTERM or SIGHUP too (only SIGKILL leaves it, and a
home that cannot be deleted is named on stderr). No secret key leaves it,
and ``keys.gpg`` is checked to hold public packets only.

- A, RSA-3072, signs: the repository's key. Every shape of batches fx to
  fx12 carries a signature by A but ``i_expired_only`` (E's),
  ``x3i_subkey_good`` and ``r_subkey_good_*`` (S's subkey's), and
  ``a_b64_empty_body`` and the ``vc2_`` packets (none).
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
  subkey: a signature by a subkey (``x3i_subkey_*``), and a forged packet
  naming the primary (``t7g5_s_primary``).
- O, RSA-3072, signs: the key of batch fx13, the shapes that need a signer
  to make them but no other key (``o_*``, ``o2_*``, ``n_i_sha224``,
  ``n_i_sha3_256``, ``n_i_nginx_crlf``). A key of its own, so that the
  batch can be made again alone (``--own-key``) once the other keys'
  secrets are gone, as they are after every run.

490 shapes, in 13 batches. The first 135 are the planning measurement's
(2026-10-28; DN-2 and Appendix A of
docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md), by the
same names and built as its throwaway generators built them (the plan's
PE/gen.py, gen2.py to gen6.py with pgp.py, and rereview/gen7.py, kept
outside the tree): the controller's batch fx, the security review's fx2 to
fx6 and its re-review's fx7, one function each below. Each planning batch
made keys of its own; here the batches share A, B, E and S and one
``Release``, so their controls are byte-identical: ``i_good``,
``i_crc_ok_rearmored`` and every ``x*i_control_good`` are one file (gpg's
armor and armor() below write the same bytes), ``d_good`` and every
``x*d_control_good`` another. The second block of the two-block shapes is
a second gpg signing (sig_a2, det_a2), as in the planning; one made in the
same second as the first is byte-identical to it (the planning's was), and
either way it is a second good signature. Batch fx8's 20 (``gi_`` inline,
``gd_`` detached; ruling HD-13) were found and measured in the plan's
Task 6, beyond Appendix A: lines and packets that go-crypto's readers see
otherwise than apt does, cut without a key from the fx7 controls. Batch
fx9's 13 (``t7c_`` controls, ``t7g`` gaps; ruling HD-14) were found and
measured in its Task 7: A's bad signature packet with its issuer
subpackets rewritten, beside A's good one, for which subpacket gpgv takes
the key from where go-crypto takes another; cut without a key from
committed fixtures (Controls).

Batches fx10 to fx13 are the plan's Task 9a (its addendum, DN-11), the
final review's shapes (PE/final-review/ of the plan's evidence, kept
outside the tree) and DN-11's new controls:

- fx10, 274 (``a_``, ``p_``, ``q_``, ``r_``, ``s_``): the adversarial
  lens's key-free shapes, its batches b1, b2, b5, b6 and b8, by its names,
  cut from committed fixtures exactly as its generator (tools/gen_a.py)
  cut them: marker lines, cleartext and armor headers, base64 and CRC
  lines, the signed text's dash-escapes, unhashed subpackets, framings and
  MPIs.
- fx11, 30: the verifier lens's (V/probe*): a packet hidden in a re-framed
  packet's tail (``sx_``), v6 and EdDSA packets (``v6_``, ``v4_``), a
  flipped hash tag (``ht_``), an unknown key's packet before a known key's
  bad one (``u_``), the oversized-length and embedded-signature packets of
  its V-C2 (``vc2_``), and those its minor findings describe (``vm1_``,
  ``vm3_``).
- fx12, 3 (``n_``): DN-11's new controls that need no key.
- fx13, 15: the adversarial lens's own-key batches b3 and b7, by its
  names, and DN-11's new controls that need a signer, made with key O.

AIDEV-NOTE: the fixtures, verdicts.txt and classes.txt are one
measurement. Each run makes new keys, new signatures and new random bytes
(the v3 garbage packets), so a regenerated corpus must be re-measured
before it is committed: regenerate, run ``bash e2e/apt-oracle/run.sh
--record``, review verdicts.txt's and classes.txt's diff (a changed row is
a finding: see e2e/apt-oracle/README.md), and commit fixtures and both
files together. Never edit a fixture by hand. Two modes write some of the
corpus and leave every other file's bytes as they are, so that --record
must leave every other row as it was: ``--from-controls`` makes no key and
writes only the batches cut from OUT's committed fixtures (FROM_CONTROLS:
fx8 to fx12, from CONTROL_FILES); ``--own-key`` makes key O alone and
writes only batch fx13 and O's public key in keys.gpg (the other keys'
packets keep their bytes).

Usage: python3 -I e2e/apt-oracle/gen_shapes.py OUT
       python3 -I e2e/apt-oracle/gen_shapes.py --from-controls OUT
       python3 -I e2e/apt-oracle/gen_shapes.py --own-key OUT
"""

from __future__ import annotations

import base64
import calendar
import hashlib
import os
import re
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from email.utils import formatdate
from pathlib import Path
from types import FrameType
from typing import Literal

# A signature subpacket: (type, critical, data).
Subpacket = tuple[int, bool, bytes]
# Shapes of one form: name -> the bytes of its InRelease or Release.gpg (or,
# in a batch's third dict, of a detached shape's own Release).
Shapes = dict[str, bytes]
# What a batch cut from Controls makes: its inline shapes, its detached ones
# and the own Release of those detached shapes that have one.
Batch = tuple[Shapes, Shapes, Shapes]

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
# RELEASE with a line starting with a dash, which gpg dash-escapes when it
# clearsigns it (the adversarial lens's ownkey.sh: dash.txt).
DASH_RELEASE = RELEASE.replace(b"Label: oracle\n", b"Label: oracle\n-x: y\n", 1)
# Key E: made at E_MADE and valid one year; signs at E_SIGNS.
E_MADE = "20200101T000000!"
E_SIGNS = "20200601T000000!"
# Key O's user ID: --own-key finds O's packets in keys.gpg by it.
OWN_KEY_UID = "Apt-shape O <o@example.invalid>"

BEGIN = b"-----BEGIN PGP SIGNATURE-----"
END = b"-----END PGP SIGNATURE-----"
MSG = b"-----BEGIN PGP SIGNED MESSAGE-----"

# SIGTERM (kill, timeout) and SIGHUP (a closed terminal) end a run as Ctrl-C
# does, by an exception: main installs exit_on_signal for them.
EXIT_SIGNALS = (signal.SIGTERM, signal.SIGHUP)

NAME_RE = re.compile(r"[A-Za-z0-9_]+")
# The files of a corpus directory, besides the shapes'.
CORPUS_FILES = ("keys.gpg", "Release", "verdicts.txt", "classes.txt")
# What a regeneration leaves in place: apt's measurement of the old bytes.
MEASUREMENT_FILES = ("verdicts.txt", "classes.txt")
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


def pad_packet(sig: bytes, inner: bytes) -> bytes:
    """sig, one signature packet in the old-format two-octet framing gpg
    writes, framed again with inner inside its declared length, after the
    signature's fields (inner empty: the same bytes)."""
    if not sig or sig[0] != 0x89:
        raise ValueError(f"want gpg's old-format two-octet framing, got {sig[:3]!r}")
    body = one_signature(sig) + inner
    return b"\x89" + struct.pack(">H", len(body)) + body


def frames(data: bytes) -> list[bytes]:
    """The packets of a stream as framed, header and body (split_packets
    without the parse): old format with a one-, two- or four-octet length,
    new format with a one-, two- or five-octet length."""
    out: list[bytes] = []
    i = 0
    while i < len(data):
        h = data[i]
        if not h & 0x80:
            raise ValueError(f"bad tag byte {h:#x} at {i}")
        if h & 0x40:
            l0 = data[i + 1]
            if l0 < 192:
                n, hl = l0, 2
            elif l0 < 224:
                n, hl = ((l0 - 192) << 8) + data[i + 2] + 192, 3
            elif l0 == 255:
                n, hl = int.from_bytes(data[i + 2 : i + 6], "big"), 6
            else:
                raise ValueError("partial lengths unsupported")
        elif h & 3 != 3:
            hl = {0: 2, 1: 3, 2: 5}[h & 3]
            n = int.from_bytes(data[i + 1 : i + hl], "big")
        else:
            raise ValueError(f"an indeterminate length at {i}")
        out.append(data[i : i + hl + n])
        i += hl + n
    return out


def filler(label: str, n: int) -> bytes:
    """n bytes that look random and are the same at every run, for a field no
    key made (a signature value or salt of a packet from no key): SHA-256 in
    counter mode over label."""
    out = b""
    i = 0
    while len(out) < n:
        out += hashlib.sha256(f"{label}:{i}".encode()).digest()
        i += 1
    return out[:n]


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


def before_crc(text: bytes, lines: list[bytes]) -> bytes:
    """text with lines (each then ended by a newline) inserted before its
    CRC line, the one line of text that starts with "="."""
    ls = text.split(b"\n")
    crc = [i for i, line in enumerate(ls) if line.startswith(b"=")]
    if len(crc) != 1:
        raise ValueError(f"want one CRC line, found {len(crc)}")
    return b"\n".join(ls[: crc[0]] + lines + ls[crc[0] :])


# --- The adversarial lens's helpers (PE/final-review/A/tools/gen_a.py) --------
#
# Raw-byte twins of the helpers above, as gen_a.py wrote them: they keep a
# subpacket area's bytes as they are (a five-octet length stays one), and
# armor_parts sets every line of an armor block. Batch fx10 is cut with them,
# byte for byte as that generator cut its batches.


def crc_line(packets: bytes) -> bytes:
    """The CRC line of an armor around packets."""
    return b"=" + base64.b64encode(crc24(packets).to_bytes(3, "big"))


def v4_parts(body: bytes) -> tuple[bytes, bytes, bytes, bytes]:
    """(head, hashed area, unhashed area, rest) of a v4 signature body, the
    areas' raw bytes."""
    if body[0] != 4:
        raise ValueError(f"not a v4 signature: version {body[0]}")
    hl = int.from_bytes(body[4:6], "big")
    ul = int.from_bytes(body[6 + hl : 8 + hl], "big")
    return body[:4], body[6 : 6 + hl], body[8 + hl : 8 + hl + ul], body[8 + hl + ul :]


def v4_build(head: bytes, hashed: bytes, unhashed: bytes, rest: bytes) -> bytes:
    """A v4 signature body from v4_parts' four parts."""
    return (
        head
        + struct.pack(">H", len(hashed))
        + hashed
        + struct.pack(">H", len(unhashed))
        + unhashed
        + rest
    )


def subpacket(
    typ: int, data: bytes, critical: bool = False, five_octet: bool = False
) -> bytes:
    """One subpacket, its length in the shortest form or in five octets."""
    body = bytes([typ | (0x80 if critical else 0)]) + data
    if five_octet:
        return b"\xff" + struct.pack(">I", len(body)) + body
    return new_len(len(body)) + body


def old_frame(body: bytes, octets: int = 2) -> bytes:
    """An old-format signature packet with a one-, two- or four-octet length."""
    if octets == 1:
        return bytes([0x88, len(body)]) + body
    if octets == 2:
        return b"\x89" + struct.pack(">H", len(body)) + body
    return b"\x8a" + struct.pack(">I", len(body)) + body


def new_frame(body: bytes, five_octet: bool = False) -> bytes:
    """A new-format signature packet, its length shortest or in five octets."""
    if five_octet:
        return b"\xc2\xff" + struct.pack(">I", len(body)) + body
    return packet(2, body)


def partial_frame(body: bytes) -> bytes:
    """New format, a 256-octet partial chunk then the rest (RFC 9580 4.2.1.4)."""
    first, rest = body[:256], body[256:]
    return b"\xc2" + bytes([0xE0 + 8]) + first + new_len(len(rest)) + rest


def with_unhashed(sig: bytes, extra: bytes, area: bytes | None = None) -> bytes:
    """sig (one signature packet) with extra appended to its unhashed area
    (or the area replaced by area, then extra), in gpg's old-format framing:
    a two-octet length, four octets past 65,535."""
    head, hashed, unhashed, rest = v4_parts(one_signature(sig))
    body = v4_build(head, hashed, (unhashed if area is None else area) + extra, rest)
    return old_frame(body, 2) if len(body) < 65536 else old_frame(body, 4)


def unhashed_of(sig: bytes) -> bytes:
    """The raw unhashed area of sig, one signature packet."""
    return v4_parts(one_signature(sig))[2]


def mpi_bits(sig: bytes) -> int:
    """The declared bit count of sig's first (RSA) MPI."""
    rest = v4_parts(one_signature(sig))[3]
    return int.from_bytes(rest[2:4], "big")


def mpi_rewrite(sig: bytes, target_bits: int, lead_zero: bool = False) -> bytes:
    """sig with its (first, RSA) MPI's bit count set to target_bits, its
    octets kept; lead_zero adds a zero octet before the value."""
    head, hashed, unhashed, rest = v4_parts(one_signature(sig))
    left16, value_mpi = rest[:2], rest[2:]
    bits = int.from_bytes(value_mpi[:2], "big")
    nbytes = (bits + 7) // 8
    value = value_mpi[2 : 2 + nbytes]
    tail = value_mpi[2 + nbytes :]
    if lead_zero:
        value = b"\x00" + value
    if (target_bits + 7) // 8 != len(value):
        raise ValueError(f"bits {target_bits} do not fit {len(value)} bytes")
    new_rest = left16 + struct.pack(">H", target_bits) + value + tail
    return old_frame(v4_build(head, hashed, unhashed, new_rest), 2)


def left16_rewrite(sig: bytes) -> bytes:
    """sig with the first octet of its hash's left 16 bits inverted."""
    head, hashed, unhashed, rest = v4_parts(one_signature(sig))
    new_rest = bytes([rest[0] ^ 0xFF, rest[1]]) + rest[2:]
    return old_frame(v4_build(head, hashed, unhashed, new_rest), 2)


def without_issuers(sig: bytes) -> tuple[bytes, bytes, bytes]:
    """(head, hashed area without its issuer subpackets 16 and 33, rest) of
    sig, one v4 signature packet."""
    head, hashed, _, rest = v4_parts(one_signature(sig))
    kept = b""
    j = 0
    while j < len(hashed):
        l0 = hashed[j]
        if l0 < 192:
            n, hl = l0, 1
        elif l0 < 255:
            n, hl = ((l0 - 192) << 8) + hashed[j + 1] + 192, 2
        else:
            n, hl = int.from_bytes(hashed[j + 1 : j + 5], "big"), 5
        if hashed[j + hl] & 0x7F not in (16, 33):
            kept += hashed[j : j + hl + n]
        j += hl + n
    return head, kept, rest


def armor_lines(packets: bytes, width: int = 64) -> list[bytes]:
    """packets' base64, cut into lines of width characters."""
    b = base64.b64encode(packets)
    return [b[i : i + width] for i in range(0, len(b), width)]


def armor_parts(
    packets: bytes,
    headers: list[bytes] | None = None,
    body: list[bytes] | None = None,
    crc: bytes | None | Literal["ok"] = "ok",
    term: bytes = b"",
    after_crc: list[bytes] | None = None,
) -> bytes:
    """An armor block with its parts set: header lines, the header
    terminator (term), base64 lines (body), the CRC line (crc: "ok", None,
    or its bytes) and lines after it; with no part set, armor(packets)."""
    lines = [BEGIN] + (headers or []) + [term]
    lines += body if body is not None else armor_lines(packets)
    if crc == "ok":
        lines.append(crc_line(packets))
    elif crc is not None:
        lines.append(crc)
    lines += after_crc or []
    lines.append(END)
    return b"\n".join(lines) + b"\n"


def second_line(fn: Callable[[bytes], bytes], lines: list[bytes]) -> list[bytes]:
    """lines with fn applied to the second one."""
    out = list(lines)
    out[1] = fn(out[1])
    return out


def merge(*parts: Batch) -> Batch:
    """The batches of parts as one; a name used twice fails."""
    inline: Shapes = {}
    detached: Shapes = {}
    releases: Shapes = {}
    for part in parts:
        for src, dst in zip(part, (inline, detached, releases), strict=True):
            for name, data in src.items():
                if name in dst:
                    raise ValueError(f"shape {name} is defined twice")
                dst[name] = data
    return inline, detached, releases


# --- RSA and the agent: the SHA3-256 signature --------------------------------

# libgcrypt's number for SHA3-256 (GCRY_MD_SHA3_256), which gpg-agent's
# SETHASH takes; OpenPGP's is 12 (RFC 9580, 9.5).
GCRY_MD_SHA3_256 = 313
OPENPGP_SHA3_256 = 12
# The DER DigestInfo prefixes of EMSA-PKCS1-v1_5 (RFC 8017, 9.2; NIST's
# hash OIDs 2.16.840.1.101.3.4.2.1 and .8).
DIGEST_INFO_SHA256 = bytes.fromhex("3031300d060960864801650304020105000420")
DIGEST_INFO_SHA3_256 = bytes.fromhex("3031300d060960864801650304020805000420")


def rsa_public_key(keyring: bytes) -> tuple[int, int]:
    """(n, e) of keyring's one v4 RSA primary key."""
    keys = [body for tag, body in split_packets(keyring) if tag == 6]
    if len(keys) != 1 or keys[0][0] != 4 or keys[0][5] != 1:
        raise ValueError("want one v4 RSA primary key")
    rest = keys[0][6:]
    values: list[int] = []
    for _ in range(2):
        n = (int.from_bytes(rest[:2], "big") + 7) // 8
        values.append(int.from_bytes(rest[2 : 2 + n], "big"))
        rest = rest[2 + n :]
    return values[0], values[1]


def emsa_pkcs1_v15(digest_info: bytes, digest: bytes, k: int) -> int:
    """The EMSA-PKCS1-v1_5 encoding of digest, as an integer (RFC 8017, 9.2)."""
    t = digest_info + digest
    return int.from_bytes(b"\x00\x01" + b"\xff" * (k - len(t) - 3) + b"\x00" + t, "big")


def signed_digest(
    body: bytes, data: bytes, digest_of: Callable[[bytes], bytes]
) -> bytes:
    """The digest a v4 signature packet's body signs over data: data, then
    the hashed part of the packet and its trailer (RFC 4880, 5.2.4),
    hashed by digest_of."""
    head, hashed, _, _ = v4_parts(body)
    part = head + struct.pack(">H", len(hashed)) + hashed
    return digest_of(data + part + b"\x04\xff" + struct.pack(">I", len(part)))


def sha256(data: bytes) -> bytes:
    """data's SHA-256 digest."""
    return hashlib.sha256(data).digest()


def sha3_256(data: bytes) -> bytes:
    """data's SHA3-256 digest."""
    return hashlib.sha3_256(data).digest()


def rsa_signature_value(body: bytes) -> int:
    """The RSA value of a v4 signature packet's body."""
    rest = v4_parts(body)[3]
    n = (int.from_bytes(rest[2:4], "big") + 7) // 8
    return int.from_bytes(rest[4 : 4 + n], "big")


def unescape_assuan(data: bytes) -> bytes:
    """An Assuan data line's payload, its %XX escapes decoded."""
    return re.sub(rb"%([0-9A-Fa-f]{2})", lambda m: bytes([int(m.group(1), 16)]), data)


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

    def detach_sign(self, key: str, text: bytes, textmode: bool = False) -> bytes:
        """key's binary detached signature of text: type 0x00, or with
        textmode 0x01 (a text signature, over text's canonical form)."""
        mode = ["--textmode"] if textmode else []
        return self.run(
            "--local-user",
            key,
            "--digest-algo",
            "SHA256",
            *mode,
            "--detach-sign",
            inp=text,
        )

    def export(self, *fprs: str) -> bytes:
        """The binary export of the public keys fprs."""
        return self.run("--export", *fprs)

    def keygrip(self, fpr: str) -> str:
        """The keygrip of primary key fpr: how gpg-agent names its secret."""
        lines = self.run("--with-colons", "--with-keygrip", "--list-keys", fpr)
        rows = [line.split(":") for line in lines.decode().splitlines()]
        for i, row in enumerate(rows[:-1]):
            if row[0] == "fpr" and row[9] == fpr and rows[i + 1][0] == "grp":
                return rows[i + 1][9]
        raise RuntimeError(f"gpg printed no keygrip for {fpr}")

    def agent_sign(self, grip: str, gcry_algo: int, digest: bytes) -> bytes:
        """The home's gpg-agent's signature of digest by the secret key grip:
        for an RSA key, PKCS #1 v1.5 over the DigestInfo of libgcrypt's hash
        algorithm gcry_algo. Returns the signature value's octets."""
        script = f"SIGKEY {grip}\nSETHASH {gcry_algo} {digest.hex().upper()}\n"
        proc = subprocess.run(
            ["gpg-connect-agent", "--homedir", str(self.home)],
            input=(script + "PKSIGN\n/bye\n").encode(),
            capture_output=True,
            check=False,
        )
        data = b""
        for line in proc.stdout.split(b"\n"):
            if line.startswith(b"ERR"):
                raise RuntimeError(f"gpg-agent: {line.decode(errors='replace')}")
            if line.startswith(b"D "):
                data += unescape_assuan(line[2:])
        # The S-expression (7:sig-val(3:rsa(1:s<length>:<octets>))).
        m = re.search(rb"\(1:s(\d+):", data)
        if proc.returncode != 0 or m is None:
            err = proc.stderr.decode(errors="replace").strip()
            raise RuntimeError(
                f"gpg-agent signed nothing: exit {proc.returncode}: {err}"
            )
        return data[m.end() : m.end() + int(m.group(1))]

    def close(self) -> None:
        """Stops the home's agent and deletes the home (its secret keys with
        it) and the socket dir gpg made for it under /run/user; says so on
        stderr when the home survives."""
        # AIDEV-NOTE: Ctrl-C, SIGTERM and SIGHUP are held until the home is
        # gone (one arriving now is delivered when the mask is restored):
        # each raises an exception, which would leave this clean-up half
        # done, the secret keys behind.
        held = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, *EXIT_SIGNALS})
        try:
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
            if self.home.exists():
                print(
                    f"gen_shapes.py: could not delete {self.home}, the temporary"
                    " GnuPG home with this run's secret keys: delete it by hand",
                    file=sys.stderr,
                )
        finally:
            signal.pthread_sigmask(signal.SIG_SETMASK, held)


@dataclass(frozen=True)
class Keys:
    """The fingerprints of the five keys (see the module comment)."""

    a: str
    b: str
    e: str
    s: str
    s_sub: str  # S's signing subkey
    o: str


def make_own_key(g: GnuPG) -> str:
    """Key O, batch fx13's."""
    return g.gen_key(OWN_KEY_UID, "sign", "never")


def make_keys(g: GnuPG) -> Keys:
    """Keys A, B, E, S and O."""
    a = g.gen_key("Apt-shape A <a@example.invalid>", "sign", "never")
    b = g.gen_key("Apt-shape B <b@example.invalid>", "sign", "never")
    e = g.gen_key("Apt-shape E <e@example.invalid>", "sign", "1y", faked=E_MADE)
    s = g.gen_key("Apt-shape S <s@example.invalid>", "cert", "never")
    s_sub = g.add_subkey(s, "sign")
    return Keys(a=a, b=b, e=e, s=s, s_sub=s_sub, o=make_own_key(g))


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


@dataclass(frozen=True)
class KeyBlock:
    """One key of a keyring: its packets, as exported, from its primary key
    packet to the next."""

    data: bytes
    fingerprints: frozenset[str]  # the primary's and its subkeys'
    uids: frozenset[bytes]


def key_blocks(keyring: bytes) -> list[KeyBlock]:
    """keyring's keys, in its order."""
    blocks: list[KeyBlock] = []
    data = b""
    fprs: set[str] = set()
    uids: set[bytes] = set()
    for framed in frames(keyring):
        tag, body = split_packets(framed)[0]
        if tag == 6 and data:
            blocks.append(KeyBlock(data, frozenset(fprs), frozenset(uids)))
            data, fprs, uids = b"", set(), set()
        if not data and tag != 6:
            raise ValueError(f"keyring: a packet of tag {tag} before any key")
        data += framed
        if tag in (6, 14):
            fprs.add(v4_fingerprint(body))
        elif tag == 13:
            uids.add(body)
    if data:
        blocks.append(KeyBlock(data, frozenset(fprs), frozenset(uids)))
    return blocks


def with_own_key(keyring: bytes, own_export: bytes, own_fpr: str) -> bytes:
    """keyring with key O's packets (the key whose user ID is OWN_KEY_UID)
    replaced by own_export, every other key's packets kept byte for byte;
    checked to hold public packets only, of exactly those keys."""
    kept = [b for b in key_blocks(keyring) if OWN_KEY_UID.encode() not in b.uids]
    out = b"".join(b.data for b in kept) + own_export
    want = {f for b in kept for f in b.fingerprints} | {own_fpr}
    check_keyring(out, want)
    return out


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
    fpr_s: bytes  # S's primary key's fingerprint (20 bytes)


# A good RSA-3072 signature's value is 384 octets; batch fx10's MPI shapes
# need its bit count to sit two or more above 8 * 383 and below 8 * 384.
RSA_3072_OCTETS = 384
MPI_TRIES = 30


def has_mpi_room(sig: bytes) -> bool:
    """sig's RSA value fills 384 octets, and its declared bit count can be
    made one lower than its octets allow or one higher than its value needs
    (batch fx10's p_good_mpi_bits_* and p_d_good_mpi_bits_*)."""
    bits = mpi_bits(sig)
    return 8 * (RSA_3072_OCTETS - 1) + 2 <= bits <= 8 * RSA_3072_OCTETS - 1


def sign_with_mpi_room(
    sign: Callable[[], bytes], sig_of: Callable[[bytes], bytes]
) -> bytes:
    """sign()'s output, signed again in a later second until the signature
    has_mpi_room.

    AIDEV-NOTE: a signature whose value sets its top octet's high bit (bit
    count 3072) leaves no room for p_good_mpi_bits_high or
    p_d_good_mpi_bits_high, and up to half of an RSA-3072 key's signatures
    do (how many depends on its modulus): a run that took the first ones
    would often cut batch fx10 without those shapes, and the corpus' names
    would change with the run. gpg signs deterministically within one second
    (RSA PKCS #1 v1.5, one creation time), so each try waits for the next
    second."""
    for _ in range(MPI_TRIES):
        out = sign()
        if has_mpi_room(sig_of(out)):
            return out
        time.sleep(1.1)
    raise RuntimeError(f"no signature with room in its MPI in {MPI_TRIES} tries")


def make_material(g: GnuPG, k: Keys) -> Material:
    """Signs what the shapes need, in the planning generators' order."""
    good = sign_with_mpi_room(lambda: g.clearsign(k.a, RELEASE), sig_packets)
    sig_a2 = sig_packets(g.clearsign(k.a, RELEASE))
    sig_a_other = sig_packets(g.clearsign(k.a, OTHER_TEXT))
    sig_b = sig_packets(g.clearsign(k.b, RELEASE))
    sig_e = sig_packets(g.clearsign(k.e, RELEASE, faked=E_SIGNS))
    sig_e_bad = sig_packets(g.clearsign(k.e, b"x\n", faked=E_SIGNS))
    sig_a_crit = sig_packets(
        g.clearsign(k.a, RELEASE, "--sig-notation", "!crit@example.invalid=1")
    )
    det_a = sign_with_mpi_room(lambda: g.detach_sign(k.a, RELEASE), lambda s: s)
    det_a2 = g.detach_sign(k.a, RELEASE)
    det_a_other = g.detach_sign(k.a, OTHER_TEXT)
    det_b = g.detach_sign(k.b, RELEASE)
    sha1 = g.clearsign(k.a, RELEASE, digest="SHA1")
    sig_a_sha1 = sig_packets(g.clearsign(k.a, RELEASE, digest="SHA1"))
    # "!": the subkey itself, as PE/gen3.py signed.
    sig_s = sig_packets(g.clearsign(k.s_sub + "!", RELEASE))
    sig_s_other = sig_packets(g.clearsign(k.s_sub + "!", OTHER_TEXT))
    dash_escaped_end = g.clearsign(k.a, RELEASE + b"-----END PGP SIGNATURE-----\n")
    return Material(
        good=good,
        head=good[: good.index(BEGIN)],
        head_sha1=sha1[: sha1.index(BEGIN)],
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
        fpr_s=bytes.fromhex(k.s),
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


# The committed fixtures the key-free batches are cut from (Controls), with
# what each gives them: --from-controls reads them from OUT, and a run that
# makes keys reads them from the shapes it has just built (build_shapes), so
# both cut from the same bytes.
CONTROL_FILES = (
    "x7i_control_good.InRelease",  # head + armor(sig_a): A's good inline signature
    "x7d_control_good.Release.gpg",  # armor(det_a): A's good detached signature
    "i_badsig_only.InRelease",  # A's signature of OTHER_TEXT, a bad one here
    "x3i_subkey_bad_plus_good.InRelease",  # S's subkey's bad packet, first
    "keys.gpg",  # S's primary key: the one primary with a subkey
    "Release",  # the shared Release (fx10's a_d_release_* change it)
    "i_unknown_unknownfirst.InRelease",  # B's packet, then A's good one
    "i_unknown_goodfirst.InRelease",  # A's good packet, then B's
    "i_good_plus_expired_good.InRelease",  # A's good packet, then E's good one
    "x3i_subkey_good.InRelease",  # S's subkey's good packet
    "d_badsig_goodfirst.Release.gpg",  # det_a, then A's bad detached packet
    "d_unknown_goodfirst.Release.gpg",  # det_a, then B's detached packet
    "xi_good_plus_v3garbage_A.InRelease",  # A's good packet, then a v3 one naming A
    "xd_good_plus_v3garbage_A.Release.gpg",  # the same, detached
    "xi_good_plus_unknownhash_A.InRelease",  # A's good packet, then an unknown hash's
)


@dataclass(frozen=True)
class Controls:
    """What the batches cut without a key start from: the bytes of
    CONTROL_FILES, by file name, and what is cut from them."""

    files: dict[str, bytes]

    def read(self, name: str) -> bytes:
        """The control file name (one of CONTROL_FILES)."""
        if name not in CONTROL_FILES:
            raise ValueError(f"{name} is not one of CONTROL_FILES")
        return self.files[name]

    @property
    def inline(self) -> bytes:
        """x7i_control_good: head + armor(sig_a)."""
        return self.read("x7i_control_good.InRelease")

    @property
    def detached(self) -> bytes:
        """x7d_control_good: armor(det_a)."""
        return self.read("x7d_control_good.Release.gpg")

    @property
    def head(self) -> bytes:
        """The inline controls' signed text and its header, up to the armor."""
        return self.inline[: self.inline.index(BEGIN)]

    @property
    def sig_a(self) -> bytes:
        """A's good inline signature packet, as gpg framed it."""
        return sig_packets(self.inline)

    @property
    def det_a(self) -> bytes:
        """A's good detached signature packet, as gpg framed it."""
        return sig_packets(self.detached)

    @property
    def sig_a_other(self) -> bytes:
        """A's signature of OTHER_TEXT: i_badsig_only's packet."""
        return sig_packets(self.read("i_badsig_only.InRelease"))

    @property
    def sig_s_other(self) -> bytes:
        """S's subkey's of OTHER_TEXT: x3i_subkey_bad_plus_good's first,
        framed new-format."""
        tag, body = split_packets(
            sig_packets(self.read("x3i_subkey_bad_plus_good.InRelease"))
        )[0]
        return packet(tag, body)

    @property
    def fpr_s(self) -> bytes:
        """S's primary key's fingerprint: keys.gpg's primary with a subkey."""
        return subkeyed_primary(self.read("keys.gpg"))


def subkeyed_primary(keyring: bytes) -> bytes:
    """The fingerprint of keyring's one primary key that has a subkey (S)."""
    with_subkey: list[str] = []
    primary = ""
    for tag, body in split_packets(keyring):
        if tag == 6:
            primary = v4_fingerprint(body)
        elif tag == 14 and primary and primary not in with_subkey:
            with_subkey.append(primary)
    if len(with_subkey) != 1:
        raise ValueError(f"want one primary key with a subkey, got {with_subkey}")
    return bytes.fromhex(with_subkey[0])


def controls_from(files: dict[str, bytes]) -> Controls:
    """The Controls in files (every name of CONTROL_FILES), checked: every
    inline control carries the signed text of x7i_control_good."""
    missing = [f for f in CONTROL_FILES if f not in files]
    if missing:
        raise SystemExit(
            f"no {', '.join(missing)} to cut from: generate the corpus first"
        )
    c = Controls(files={f: files[f] for f in CONTROL_FILES})
    for name in CONTROL_FILES:
        if name.endswith(".InRelease"):
            text = c.read(name)
            if text[: text.index(BEGIN)] != c.head:
                raise ValueError(f"{name}: its signed text is not the controls'")
    if c.read("Release") != RELEASE:
        raise ValueError("Release: not the generator's RELEASE")
    return c


def controls_from_corpus(out: Path) -> Controls:
    """The Controls cut from OUT's committed fixtures (CONTROL_FILES)."""
    names = {p.name for p in corpus_entries(out)}
    return controls_from(
        {f: (out / f).read_bytes() for f in CONTROL_FILES if f in names}
    )


def batch_fx8_from(c: Controls) -> Batch:
    """Ruling HD-13's 20 shapes (gi_ inline, gd_ detached), found and
    measured in the hardening plan's Task 6, beyond the planning corpus; cut
    without a key from the fx7 controls (x7i_control_good and
    x7d_control_good) as Task 6's probe cut them.

    g1 to g4: lines inserted before the CRC line, the first of which
    go-crypto's armor reader stops at once bytes.TrimSpace has trimmed it (a
    "-----END " prefix, or five characters starting "="), so that what
    follows it goes unread; c1 and c2 their controls, an ordinary junk line
    and an untrimmed "-----END X-----". p1 to p3: the signature packet framed
    again with bytes inside its declared length, after its fields (gpgv and
    sqv skip them; go-crypto reads them as the next packet); c3 the packet
    framed again with nothing added, the same bytes as the control.
    """
    inline_control, detached_control = c.inline, c.detached
    head = c.head
    sig_a, det_a = c.sig_a, c.det_a
    lines = {
        "c1_junk_line_control": [b"junk"],
        "c2_endx_junk_control": [b"-----END X-----", b"junk"],
        "g1_space_endx_junk": [b" -----END X-----", b"junk junk junk"],
        "g2_tab_end_junk": [b"\t-----END PGP SIGNATURE-----", b"junk"],
        "g3_vt_eq_line_junk": [b"\v=AAAA", b"junk"],
        "g4_space_endx_only": [b" -----END X-----"],
    }
    inner = {
        "c3_repadded_control": b"",
        "p1_inner_marker": packet(10, b"PGP"),
        "p2_inner_unknowntag": packet(60, b"\x00"),
        "p3_inner_zero_bytes": b"\x00\x00",
    }
    inline: Shapes = {}
    detached: Shapes = {}
    for name, added in lines.items():
        inline["gi_" + name] = before_crc(inline_control, added)
        detached["gd_" + name] = before_crc(detached_control, added)
    for name, data in inner.items():
        inline["gi_" + name] = head + armor(pad_packet(sig_a, data))
        detached["gd_" + name] = armor(pad_packet(det_a, data))
    return inline, detached, {}


def with_issuers(
    sig: bytes, hashed: list[Subpacket], unhashed: list[Subpacket]
) -> bytes:
    """sig, one v4 signature packet, with both its subpacket areas replaced
    (its hash no longer covers what it signed: a bad signature's subpackets,
    issuers included, can be rewritten so without a key); new-format framing."""
    s = parse_v4_sig(one_signature(sig))
    return packet(
        2, build_v4_sig(V4Sig(s.head, build_subpackets(hashed), [], s.rest), unhashed)
    )


def batch_fx9_from(c: Controls) -> Batch:
    """Ruling HD-14's 13 shapes (t7c_ controls, t7g gaps), found and measured
    in the hardening plan's Task 7, beyond the planning corpus: which issuer
    subpacket gpgv takes a bad signature packet's key from, where go-crypto
    takes another. Each is A's bad signature packet (A's signature of
    OTHER_TEXT, i_badsig_only's), its issuer subpackets rewritten, before
    A's good one (x7i_control_good's); t7g5 is S's subkey's bad packet
    naming S's primary key, which only certifies. Cut without a key, as
    Task 7's scratch generators (gapgen.py, gapgen2.py) cut them.

    B is a v4 fingerprint, X a v6-form one (version 6, 32 bytes), that no
    key has; "unh" names the unhashed area. gpgv reads the first hashed 33
    in the form its version knows (t7g4: a v6 form in a v4 signature is no
    fingerprint; t7g7: a later 33 is not read), else the first 16, hashed
    then unhashed (t7g1, t7g3, t7g3b); gpgv 2.2 also falls back to that 16
    when the fingerprint names a key it lacks (t7g6, t7g6b; 2.4 does not).
    """
    head = c.head
    sig_a = c.sig_a
    bad_a = c.sig_a_other
    hashed_a = subpackets(parse_v4_sig(one_signature(bad_a)).hashed_raw)
    fpr_a = next(d for t, _, d in hashed_a if t == 33)[1:]
    kid_a = fpr_a[12:]
    no_issuer = [sp for sp in hashed_a if sp[0] not in (16, 33)]
    hashed_s = subpackets(parse_v4_sig(one_signature(c.sig_s_other)).hashed_raw)
    s_no_issuer = [sp for sp in hashed_s if sp[0] not in (16, 33)]
    fpr_b = b"\x04" + bytes.fromhex("00112233445566778899aabbccddeeff01234567")
    fpr_x = b"\x06" + bytes.fromhex("8899aabbccddeeff0011223344556677" * 2)
    kid_b = bytes.fromhex("0123456789abcdef")
    a16: Subpacket = (16, False, kid_a)
    b16: Subpacket = (16, False, kid_b)
    b33: Subpacket = (33, False, fpr_b)
    x33: Subpacket = (33, False, fpr_x)
    bad = {
        # Controls: packets gpgv and the cache both take as A's.
        "t7c_bad_a_as_is": bad_a,
        "t7c_nohashed_unh16A": with_issuers(bad_a, no_issuer, [a16]),
        "t7c_nohashed_unh33B_16A": with_issuers(bad_a, no_issuer, [b33, a16]),
        # Nothing hashed; unhashed 16 = A, then B's fingerprint (go-crypto's
        # issuer is the last: B's).
        "t7g1_nohashed_unh16A_33B": with_issuers(bad_a, no_issuer, [a16, b33]),
        # Two hashed 16s: gpgv reads the first.
        "t7g3_hashed16A_16B": with_issuers(bad_a, no_issuer + [a16, b16], []),
        "t7g3b_hashed16B_16A": with_issuers(bad_a, no_issuer + [b16, a16], []),
        # A v6-form fingerprint in a v4 signature, as gpg wrote the rest.
        "t7g4_v6fpr_unh16A": with_issuers(bad_a, no_issuer + [x33], [a16]),
        # S's certify-only primary key named.
        "t7g5_s_primary": with_issuers(
            c.sig_s_other,
            s_no_issuer + [(33, False, b"\x04" + c.fpr_s)],
            [(16, False, c.fpr_s[12:])],
        ),
        # A fingerprint no key has, then (or with) A's 16.
        "t7g6_hashed33B_unh16A": with_issuers(bad_a, no_issuer + [b33], [a16]),
        "t7g6b_hashed33B_16A": with_issuers(bad_a, no_issuer + [b33, a16], []),
        # A first hashed 33 in v6 form, then a v4 one.
        "t7g7_33junk_33B_unh16A": with_issuers(bad_a, no_issuer + [x33, b33], [a16]),
        "t7g7b_33junk_33A": with_issuers(
            bad_a, no_issuer + [x33, (33, False, b"\x04" + fpr_a)], []
        ),
    }
    inline = {name: head + armor(p + sig_a) for name, p in bad.items()}
    inline["t7c_good_alone"] = head + armor(sig_a)
    return inline, {}, {}


def fx10_armor(c: Controls) -> Batch:
    """The adversarial lens's batch b1 (gen_a.py's batch_armor): marker lines,
    the cleartext's and the armor's headers and their terminators, the
    signed text's last line and line ends, base64 and CRC lines, line ends,
    a changed Release under the detached control, two blocks."""
    ci, cd, head, p, pd = c.inline, c.detached, c.head, c.sig_a, c.det_a
    # B's packet as framed in i_unknown_unknownfirst (B's, then A's good one).
    whole = sig_packets(c.read("i_unknown_unknownfirst.InRelease"))
    if not whole.endswith(p):
        raise ValueError("i_unknown_unknownfirst does not end with A's good packet")
    sig_b_pkt = whole[: len(whole) - len(p)]
    if head + armor_parts(p) != ci or armor_parts(pd) != cd:
        raise ValueError("a control is not its packets in armor_parts' armor")
    inl: Shapes = {}
    det: Shapes = {}
    rel: Shapes = {}
    cut_end = len(END) + 1  # the END line and its LF, at the end of a control

    # Marker lines: what follows the marker on its line.
    for tag, suffix in {
        "cr_sp": b"\r ",
        "cr_tab": b"\r\t",
        "sp_cr_sp": b" \r ",
        "sp_cr": b" \r",
        "vt": b"\v",
        "ff": b"\f",
        "nbsp": b"\xc2\xa0",
        "nul": b"\x00",
    }.items():
        inl[f"a_msgbegin_{tag}"] = replace(ci, MSG + b"\n", MSG + suffix + b"\n", 1)
        inl[f"a_end_{tag}"] = ci[:-cut_end] + END + suffix + b"\n"
        det[f"a_d_end_{tag}"] = cd[:-cut_end] + END + suffix + b"\n"
        det[f"a_d_begin_{tag}"] = replace(cd, BEGIN + b"\n", BEGIN + suffix + b"\n", 1)
    inl["a_sigbegin_cr_sp"] = replace(ci, BEGIN + b"\n", BEGIN + b"\r \n", 1)
    inl["a_end_cr_sp_eof"] = ci[:-cut_end] + END + b"\r "
    det["a_d_end_cr_sp_eof"] = cd[:-cut_end] + END + b"\r "
    inl["a_end_cr_tab_eof"] = ci[:-cut_end] + END + b"\r\t"

    # The cleartext header block's terminator (the line after "Hash: SHA256").
    for tag, t in {
        "cr_sp": b"\r ",
        "sp_cr_sp": b" \r ",
        "tab_cr_tab": b"\t\r\t",
        "vt": b"\v",
        "ff": b"\f",
        "nbsp": b"\xc2\xa0",
        "nel": b"\xc2\x85",
        "u3000": b"\xe3\x80\x80",
        "u2028": b"\xe2\x80\xa8",
        "sp": b" ",
        "crcr": b"\r\r",
        "nul": b"\x00",
        "raw_a0": b"\xa0",
    }.items():
        inl[f"a_hdrterm_{tag}"] = replace(
            ci, b"Hash: SHA256\n\n", b"Hash: SHA256\n" + t + b"\n", 1
        )

    # The cleartext's Hash header itself.
    inl["a_hash_lowercase_key"] = replace(ci, b"Hash: SHA256\n", b"hash: SHA256\n", 1)
    inl["a_hash_two_headers"] = replace(
        ci, b"Hash: SHA256\n", b"Hash: SHA256\nHash: SHA512\n", 1
    )
    inl["a_hash_nospace"] = replace(ci, b"Hash: SHA256\n", b"Hash:SHA256\n", 1)
    inl["a_hash_lead_space"] = replace(ci, b"Hash: SHA256\n", b" Hash: SHA256\n", 1)

    # The signed text's last line, empty as gpg writes it (the line before
    # the signature BEGIN line): dash-escaped, or blanks.
    if ci.count(b"Packages\n\n" + BEGIN) != 1:
        raise ValueError("the control's signed text does not end with an empty line")
    for tag, t in {
        "dash_sp": b"- ",
        "dash_sp_tab": b"- \t",
        "dash_sp_sp": b"-  ",
        "dash_sp_cr": b"- \r",
        "sp": b" ",
        "tab": b"\t",
        "cr": b"\r",
        "crcr": b"\r\r",
        "cr_sp": b"\r ",
        "sp_cr": b" \r",
    }.items():
        inl[f"a_lastline_{tag}"] = replace(
            ci, b"Packages\n\n" + BEGIN, b"Packages\n" + t + b"\n" + BEGIN, 1
        )
    # A text line's ending.
    for tag, t in {
        "crcr": b"\r\r",
        "cr_sp": b"\r ",
        "tab_cr": b"\t\r",
        "nul": b"\x00",
        "vt": b"\v",
    }.items():
        inl[f"a_textline_{tag}"] = replace(
            ci, b"\nOrigin: oracle\n", b"\nOrigin: oracle" + t + b"\n", 1
        )

    # The signature armor's header terminator.
    for tag, t in {"cr_sp": b"\r ", "sp_cr": b" \r", "ff": b"\f"}.items():
        inl[f"a_sighdrterm_{tag}"] = head + armor_parts(p, term=t)
        det[f"a_d_sighdrterm_{tag}"] = armor_parts(pd, term=t)

    # The signature armor's header lines.
    hdrs: dict[str, list[bytes]] = {
        "trailing_space": [b"Comment: "],
        "trailing_tab": [b"Comment:\t"],
        "colon_then_colonspace": [b"Junk:junk: x"],
        "tab_then_colonspace": [b"Comment:\tx: y"],
        "nul_before_colon": [b"Com\x00ment: x"],
        "nul_key": [b"\x00: x"],
        "nul_value": [b"Comment: a\x00b"],
        "hash_in_sig": [b"Hash: SHA256"],
        "vt_lead": [b"\vComment: x"],
        "nbsp_lead": [b"\xc2\xa0Comment: x"],
        "cr_mid": [b"Comment: a\rb"],
        "long_colon_early": [b"Comment: " + b"x" * 150],
        "long_colon_late": [b"X" * 120 + b": y"],
        "colon_cr_space": [b"Comment:\r x"],
        "folded_tab": [b"Comment: x", b"\tmore: y"],
        "dash_in_value": [b"Comment: -x-"],
    }
    detached_hdrs = (
        "trailing_space",
        "trailing_tab",
        "colon_then_colonspace",
        "nul_before_colon",
        "cr_mid",
        "vt_lead",
    )
    for tag, h in hdrs.items():
        inl[f"a_hdr_{tag}"] = head + armor_parts(p, headers=h)
        if tag in detached_hdrs:
            det[f"a_d_hdr_{tag}"] = armor_parts(pd, headers=h)

    # Base64 body lines.
    lines, dlines = armor_lines(p), armor_lines(pd)
    edits: dict[str, Callable[[bytes], bytes]] = {
        "lead_vt": lambda line: b"\v" + line,
        "trail_vt": lambda line: line + b"\v",
        "lead_ff": lambda line: b"\f" + line,
        "lead_nbsp": lambda line: b"\xc2\xa0" + line,
        "trail_nbsp": lambda line: line + b"\xc2\xa0",
        "lead_nel": lambda line: b"\xc2\x85" + line,
        "lead_u3000": lambda line: b"\xe3\x80\x80" + line,
        "cr_mid": lambda line: line[:10] + b"\r" + line[10:],
        "tab_mid": lambda line: line[:10] + b"\t" + line[10:],
        "sp_mid": lambda line: line[:10] + b" " + line[10:],
        "nul_end": lambda line: line + b"\x00",
        "lead_tab": lambda line: b"\t" + line,
    }
    detached_edits = ("lead_vt", "lead_nbsp", "trail_vt", "tab_mid", "sp_mid", "cr_mid")
    for tag, fn in edits.items():
        inl[f"a_b64_{tag}"] = head + armor_parts(p, body=second_line(fn, lines))
        if tag in detached_edits:
            det[f"a_d_b64_{tag}"] = armor_parts(pd, body=second_line(fn, dlines))
    for tag, extra in {
        "vt_line": b"\v",
        "nbsp_line": b"\xc2\xa0",
        "ff_line": b"\f",
        "cr_sp_line": b"\r ",
    }.items():
        inl[f"a_b64_{tag}"] = head + armor_parts(
            p, body=lines[:3] + [extra] + lines[3:]
        )
    det["a_d_b64_vt_line"] = armor_parts(pd, body=dlines[:3] + [b"\v"] + dlines[3:])
    for w in (76, 80, 96, 97, 100, 200):
        inl[f"a_b64_width_{w}"] = head + armor_parts(p, body=armor_lines(p, w))
    det["a_d_b64_width_97"] = armor_parts(pd, body=armor_lines(pd, 97))
    # Irregular line lengths, none a multiple of four (one over 96: the lens's
    # builder slip, q_b64_irregular_le76 is the case it meant).
    b = base64.b64encode(p)
    irregular = [b[0:63], b[63:64], b[64:129], b[129:200], b[200:]]
    inl["a_b64_irregular"] = head + armor_parts(p, body=irregular)
    # Padding inside the body: the packet's base64 cut in two, each padded.
    for k in (100, 200, 437):
        parts = armor_lines(p[:k]) + armor_lines(p[k:])
        inl[f"a_pad_split_{k}"] = head + armor_parts(p, body=parts)
        det[f"a_d_pad_split_{k}"] = armor_parts(
            pd, body=armor_lines(pd[:k]) + armor_lines(pd[k:])
        )
    # B's packet (an unknown key's) with a 4-octet unknown non-critical
    # unhashed subpacket, so that its length is not a multiple of 3 and its
    # base64 ends in padding, then A's good one.
    qb = with_unhashed(sig_b_pkt, subpacket(100, b"xy"))
    both = qb + p
    if len(qb) % 3 == 0 or len(both) % 3 == 0:
        raise ValueError(
            "B's packet leaves no padding: run again (each run signs anew)"
        )
    inl["a_pad_unknown_then_good"] = head + armor_parts(
        both, body=armor_lines(qb) + armor_lines(p)
    )
    inl["a_pad_unknown_then_good_control"] = head + armor_parts(both)
    # A's packet, then B's padded one, each base64-encoded on its own (A's
    # ends unpadded: no padding mid-stream).
    inl["a_b64_good_then_unknown_rewrapped"] = head + armor_parts(
        p + qb, body=armor_lines(p) + armor_lines(qb)
    )
    # The final padding ("=" or "==") alone on its own line.
    bl = armor_lines(both)
    npad = bl[-1].count(b"=")
    inl["a_pad_own_line"] = head + armor_parts(
        both, body=bl[:-1] + [bl[-1][:-npad], bl[-1][-npad:]]
    )
    inl["a_b64_urlsafe"] = head + armor_parts(
        p, body=[line.replace(b"+", b"-").replace(b"/", b"_") for line in lines]
    )
    inl["a_b64_empty_body"] = head + armor_parts(b"", body=[])
    # CRC line.
    crc, dcrc = crc_line(p), crc_line(pd)
    for tag, line in {
        "lead_nbsp": b"\xc2\xa0" + crc,
        "lead_vt": b"\v" + crc,
        "trail_vt": crc + b"\v",
        "trail_nbsp": crc + b"\xc2\xa0",
        "lead_ff": b"\f" + crc,
        "extra_char": crc + b"A",
        "trail_cr_sp": crc + b"\r ",
        "lead_tab": b"\t" + crc,
    }.items():
        inl[f"a_crc_{tag}"] = head + armor_parts(p, crc=line)
        if tag in ("lead_nbsp", "lead_vt", "extra_char"):
            det[f"a_d_crc_{tag}"] = armor_parts(pd, crc=line.replace(crc, dcrc))
    # The CRC on the last base64 line (p's base64 has no padding).
    inl["a_crc_on_last_line"] = head + armor_parts(
        p, body=lines[:-1] + [lines[-1] + crc], crc=None
    )
    # After the CRC line.
    for tag, extra in {
        "cr_sp": b"\r ",
        "vt": b"\v",
        "nbsp": b"\xc2\xa0",
        "sp_tab_cr": b" \t\r",
    }.items():
        inl[f"a_aftercrc_{tag}"] = head + armor_parts(p, after_crc=[extra])
        det[f"a_d_aftercrc_{tag}"] = armor_parts(pd, after_crc=[extra])
    # Line endings.
    inl["a_lf_cr_in_body"] = head + armor_parts(
        p, body=[lines[0] + b"\r"] + [b"\r" + line for line in lines[1:]]
    )
    inl["a_crlf_mixed_sig"] = head + armor_parts(p).replace(b"\n", b"\r\n", 3)
    det["a_d_crlf_mixed"] = armor_parts(pd).replace(b"\n", b"\r\n", 3)
    inl["a_text_crlf_sig_lf"] = head.replace(b"\n", b"\r\n") + armor_parts(p)
    # Marker variants.
    inl["a_end_lowercase"] = ci[:-cut_end] + b"-----end pgp signature-----\n"
    det["a_d_begin_extra_dash"] = replace(cd, BEGIN + b"\n", b"-" + BEGIN + b"\n", 1)
    det["a_d_begin_message_type"] = replace(
        cd, BEGIN + b"\n", b"-----BEGIN PGP MESSAGE-----\n", 1
    )
    det["a_d_end_message_type"] = cd[:-cut_end] + b"-----END PGP MESSAGE-----\n"

    # Detached pairs: the Release changed under the control's signature.
    release = c.read("Release")
    for name, changed in {
        "a_d_release_crlf": release.replace(b"\n", b"\r\n"),
        "a_d_release_extra_nl": release + b"\n",
        "a_d_release_no_final_nl": release.rstrip(b"\n"),
    }.items():
        det[name] = cd
        rel[name] = changed
    det["a_d_twoblocks_same"] = cd + cd
    inl["a_twoblocks_same"] = ci + armor_parts(p)
    # Two good packets of the same bytes in one armor.
    inl["a_good_dup"] = head + armor_parts(p + p)
    det["a_d_good_dup"] = armor_parts(pd + pd)
    return inl, det, rel


def fx10_packet(c: Controls) -> Batch:
    """The adversarial lens's batch b2 (gen_a.py's batch_packet): A's good
    signature's unhashed area, framing and MPI, and A's bad one with no
    issuer at all."""
    head, p, pd = c.head, c.sig_a, c.det_a
    bad = c.sig_a_other
    inl: Shapes = {}
    det: Shapes = {}
    un = unhashed_of(p)
    if un[:2] != b"\x09\x10":
        raise ValueError(f"unexpected unhashed area {un.hex()}: want gpg's issuer 16")
    kid_a = un[2:10]
    hashed = v4_parts(one_signature(p))[1]
    i = hashed.index(b"\x16\x21\x04")  # gpg's issuer fingerprint subpacket
    fpr_a = hashed[i + 3 : i + 23]
    name = b"crit@example.invalid"
    notation = struct.pack(">BBBBHH", 0x80, 0, 0, 0, len(name), 1) + name + b"1"
    extras = {
        "unh_crit_unknown100": subpacket(100, b"x", critical=True),
        "unh_crit_notation": subpacket(20, notation, critical=True),
        "unh_crit_signeruid": subpacket(28, b"x <x@example.invalid>", critical=True),
        "unh_crit_ksflags": subpacket(23, b"\x80", critical=True),
        "unh_crit_sigtarget": subpacket(31, b"\x01\x08" + b"\x00" * 32, critical=True),
        "unh_crit_issuer16": subpacket(16, kid_a, critical=True),
        "unh_crit_keyflags": subpacket(27, b"\x02", critical=True),
        "unh_noncrit_unknown100": subpacket(100, b"x"),
        "unh_noncrit_notation": subpacket(20, notation),
        "unh_creation_now": subpacket(2, struct.pack(">I", 1793000000)),
        "unh_sig_expire_1s": subpacket(3, struct.pack(">I", 1)),
        "unh_fpr_v4_dup": subpacket(33, b"\x04" + fpr_a),
        "unh_fpr_v3form": subpacket(33, b"\x03" + fpr_a),
        "unh_fpr_v0form": subpacket(33, b"\x00" + fpr_a),
        "unh_16_dup": subpacket(16, kid_a),
        "unh_16_fiveoctet_len": subpacket(16, kid_a, five_octet=True),
        "unh_big_9990": subpacket(100, b"x" * (9990 - 10 - 3)),
        "unh_big_10001": subpacket(100, b"x" * (10001 - 10 - 3)),
        "unh_big_30000": subpacket(100, b"x" * (30000 - 10 - 3)),
    }
    detached_extras = (
        "unh_crit_unknown100",
        "unh_crit_notation",
        "unh_big_10001",
        "unh_noncrit_unknown100",
    )
    for tag, extra in extras.items():
        inl[f"p_good_{tag}"] = head + armor_parts(with_unhashed(p, extra))
        if tag in detached_extras:
            det[f"p_d_good_{tag}"] = armor_parts(with_unhashed(pd, extra))
    # The unhashed 16 dropped (only the hashed 33 names A).
    inl["p_good_unh_empty"] = head + armor_parts(with_unhashed(p, b"", area=b""))
    # An unknown critical subpacket in the unhashed area of a second,
    # duplicate good packet (the first is untouched).
    inl["p_good_plus_dup_unh_crit"] = head + armor_parts(
        p + with_unhashed(p, subpacket(100, b"x", critical=True))
    )
    # Framing.
    body, dbody = one_signature(p), one_signature(pd)
    inl["p_good_newformat"] = head + armor_parts(new_frame(body))
    inl["p_good_new5"] = head + armor_parts(new_frame(body, five_octet=True))
    inl["p_good_old4"] = head + armor_parts(old_frame(body, 4))
    inl["p_good_partial"] = head + armor_parts(partial_frame(body))
    inl["p_good_indeterminate"] = head + armor_parts(b"\x8b" + body)
    det["p_d_good_indeterminate"] = armor_parts(b"\x8b" + dbody)
    det["p_d_good_newformat"] = armor_parts(new_frame(dbody))
    # The MPI's bit count, one lower than its octets allow or one higher
    # than its value needs; a leading zero octet (the bit count 8 more).
    for sig in (p, pd):
        if not has_mpi_room(sig):
            raise ValueError("a control's MPI leaves no room for the bit-count edits")
    bits, dbits = mpi_bits(p), mpi_bits(pd)
    inl["p_good_mpi_bits_low"] = head + armor_parts(
        mpi_rewrite(p, 8 * ((bits + 7) // 8 - 1) + 1)
    )
    inl["p_good_mpi_bits_high"] = head + armor_parts(
        mpi_rewrite(p, 8 * ((bits + 7) // 8))
    )
    inl["p_good_mpi_bits_minus1"] = head + armor_parts(mpi_rewrite(p, bits - 1))
    inl["p_good_mpi_leadzero"] = head + armor_parts(
        mpi_rewrite(p, bits + 8, lead_zero=True)
    )
    det["p_d_good_mpi_bits_low"] = armor_parts(
        mpi_rewrite(pd, 8 * ((dbits + 7) // 8 - 1) + 1)
    )
    det["p_d_good_mpi_bits_high"] = armor_parts(mpi_rewrite(pd, 8 * ((dbits + 7) // 8)))
    # The hash's left 16 bits changed.
    inl["p_good_left16_changed"] = head + armor_parts(left16_rewrite(p))
    # A's bad packet with no issuer subpacket at all, before A's good one.
    bh, bhashed, brest = without_issuers(bad)
    noiss = old_frame(v4_build(bh, bhashed, b"", brest), 2)
    inl["p_badA_noissuer_plus_good"] = head + armor_parts(noiss + p)
    # A's bad packet as gpg wrote it, before A's good one: control.
    inl["p_badA_plus_good_control"] = head + armor_parts(bad + p)
    return inl, det, {}


def fx10_extra(c: Controls) -> Batch:
    """The adversarial lens's batch b5 (gen_a.py's batch_extra): the good
    packet's unhashed issuers (gpgv 2.2's GOODSIG key ID), big unhashed
    areas beside a good packet, an unknown critical subpacket on an unknown
    key's packet, sqv's issuer-less packet, and armor follow-ups."""
    ci, cd, head, p, pd = c.inline, c.detached, c.head, c.sig_a, c.det_a
    whole = sig_packets(c.read("i_unknown_unknownfirst.InRelease"))
    qb = whole[: len(whole) - len(p)]  # B's packet, as framed
    bad = c.sig_a_other
    kid_a = unhashed_of(p)[2:10]
    hashed = v4_parts(one_signature(p))[1]
    i = hashed.index(b"\x16\x21\x04")
    fpr_a = hashed[i + 3 : i + 23]
    kid_b = bytes.fromhex("0123456789abcdef")
    fpr_b = bytes.fromhex("00112233445566778899aabbccddeeff01234567")

    def s16(k: bytes) -> bytes:
        return subpacket(16, k)

    def s33(f: bytes) -> bytes:
        return subpacket(33, b"\x04" + f)

    inl: Shapes = {}
    det: Shapes = {}
    for tag, area in {
        "unh_16B_16A": s16(kid_b) + s16(kid_a),
        "unh_16A_16B": s16(kid_a) + s16(kid_b),
        "unh_33A_only": s33(fpr_a),
        "unh_33B_16A": s33(fpr_b) + s16(kid_a),
        "unh_16A_33A": s16(kid_a) + s33(fpr_a),
    }.items():
        inl[f"q_good_{tag}"] = head + armor_parts(with_unhashed(p, b"", area=area))
    det["q_d_good_unh_empty"] = armor_parts(with_unhashed(pd, b"", area=b""))
    det["q_d_good_unh_16B_16A"] = armor_parts(
        with_unhashed(pd, b"", area=s16(kid_b) + s16(unhashed_of(pd)[2:10]))
    )
    big = subpacket(100, b"x" * (10001 - 10 - 3))
    crit = subpacket(100, b"x", critical=True)
    inl["q_good_plus_bigunh_dup"] = head + armor_parts(p + with_unhashed(p, big))
    inl["q_bigunh_dup_plus_good"] = head + armor_parts(with_unhashed(p, big) + p)
    inl["q_unknownB_bigunh_plus_good"] = head + armor_parts(with_unhashed(qb, big) + p)
    inl["q_unknownB_critunh_plus_good"] = head + armor_parts(
        with_unhashed(qb, crit) + p
    )
    inl["q_good_plus_unknownB_critunh"] = head + armor_parts(
        p + with_unhashed(qb, crit)
    )
    # A bad packet naming no issuer at all, after the good one; and the same
    # packet naming B by an unhashed 16 only, before it (it has an issuer).
    bh, bhashed, brest = without_issuers(bad)
    noiss = old_frame(v4_build(bh, bhashed, b"", brest), 2)
    inl["q_good_plus_noissuer_bad"] = head + armor_parts(p + noiss)
    inl["q_unh16B_bad_plus_good"] = head + armor_parts(
        old_frame(v4_build(bh, bhashed, s16(kid_b), brest), 2) + p
    )
    # Armor follow-ups.
    b = base64.b64encode(p)
    cuts = [0, 63, 65, 66, 137, 201, 266, 330, 395, 461, 525, len(b)]
    irregular = [b[cuts[k] : cuts[k + 1]] for k in range(len(cuts) - 1)]
    if not all(0 < len(x) <= 76 for x in irregular):
        raise ValueError("q_b64_irregular_le76's cuts do not fit the control's base64")
    inl["q_b64_irregular_le76"] = head + armor_parts(p, body=irregular)
    lines = armor_lines(p)
    inl["q_b64_ff_mid"] = head + armor_parts(
        p, body=[lines[0], lines[1][:10] + b"\f" + lines[1][10:]] + lines[2:]
    )
    inl["q_b64_nbsp_mid"] = head + armor_parts(
        p, body=[lines[0], lines[1][:10] + b"\xc2\xa0" + lines[1][10:]] + lines[2:]
    )
    inl["q_sighdrterm_vt"] = head + armor_parts(p, term=b"\v")
    inl["q_sighdrterm_tab"] = head + armor_parts(p, term=b"\t")
    dlines = armor_lines(pd)
    det["q_d_b64_lead_ff"] = armor_parts(
        pd, body=[dlines[0], b"\f" + dlines[1]] + dlines[2:]
    )
    det["q_d_b64_lead_nel"] = armor_parts(
        pd, body=[dlines[0], b"\xc2\x85" + dlines[1]] + dlines[2:]
    )
    # Header-line follow-ups (the first-colon rule).
    for tag, h in {
        "colon_nul_space": [b"Comment:\x00 x"],
        "two_colons_space_first": [b"Comment: a:b"],
        "comment_empty_cr": [b"Comment:\r"],
    }.items():
        inl[f"q_hdr_{tag}"] = head + armor_parts(p, headers=h)
    det["q_d_hdr_key_only_colon"] = armor_parts(pd, headers=[b"Comment:"])
    det["q_d_hdr_tab_then_colonspace"] = armor_parts(pd, headers=[b"Comment:\tx: y"])
    # Cleartext header terminators apt reads as empty.
    inl["q_hdrterm_sp_cr"] = replace(ci, b"Hash: SHA256\n\n", b"Hash: SHA256\n \r\n", 1)
    inl["q_hdrterm_tab_crcr"] = replace(
        ci, b"Hash: SHA256\n\n", b"Hash: SHA256\n\t\r\r\n", 1
    )
    # No Hash header, and a terminator apt does not read as empty.
    inl["q_nohash_hdrterm_vt"] = replace(ci, b"Hash: SHA256\n\n", b"\v\n", 1)
    # Marker lines: blanks then CRs (apt's order: accepted).
    inl["q_msgbegin_sp_tab_crcr"] = replace(ci, MSG + b"\n", MSG + b" \t\r\r\n", 1)
    inl["q_end_tab_sp_crcr"] = ci[: -len(END) - 1] + END + b"\t \r\r\n"
    det["q_d_begin_tab_crcr"] = replace(cd, BEGIN + b"\n", BEGIN + b"\t\r\r\n", 1)
    return inl, det, {}


def fx10_round3(c: Controls) -> Batch:
    """The adversarial lens's batch b6 (gen_a.py's batch_round3): a malformed
    MPI on a second packet; an unknown critical unhashed subpacket on the
    expired key E's good packet and on S's subkey's; S's subkey's good packet
    without its unhashed 16."""
    head, p = c.head, c.sig_a
    ex = split_packets(sig_packets(c.read("i_good_plus_expired_good.InRelease")))
    if old_frame(ex[0][1], 2) != p:
        raise ValueError("i_good_plus_expired_good does not start with A's packet")
    sig_e = old_frame(ex[1][1], 2)
    sub = sig_packets(c.read("x3i_subkey_good.InRelease"))
    crit = subpacket(100, b"x", critical=True)
    bits = mpi_bits(p)
    low = mpi_rewrite(p, 8 * ((bits + 7) // 8 - 1) + 1)
    inl = {
        "r_good_plus_dup_mpi_low": head + armor_parts(p + low),
        "r_dup_mpi_low_plus_good": head + armor_parts(low + p),
        "r_expiredE_critunh_plus_good": head
        + armor_parts(with_unhashed(sig_e, crit) + p),
        "r_good_plus_expiredE_control": head + armor_parts(p + sig_e),
        "r_subkey_good_critunh": head + armor_parts(with_unhashed(sub, crit)),
        "r_subkey_good_unh_empty": head
        + armor_parts(with_unhashed(sub, b"", area=b"")),
        "r_subkey_good_control": head + armor_parts(sub),
    }
    return inl, {}, {}


def fx10_round4(c: Controls) -> Batch:
    """The adversarial lens's batch b8 (gen_a.py's batch_round4): armor header
    lines as sqv reads them (encodings, CRs, NULs), gpgv's quoted-printable
    CRC workaround, the cleartext Hash header's trailing bytes."""
    ci, head, p, pd = c.inline, c.head, c.sig_a, c.det_a
    inl: Shapes = {}
    det: Shapes = {}
    for tag, h in {
        "invalid_utf8_value": b"Comment: \xff",
        "invalid_utf8_key": b"Comm\xffent: x",
        "cr_in_key": b"Com\rment: x",
        "lead_tab": b"\tComment: x",
        "nonascii_key": b"Comm\xc3\xa9nt: x",
        "value_trail_vt": b"Comment: x\v",
        "colon_space_only": b": ",
        "two_spaces": b"Comment:  x",
    }.items():
        inl[f"s_hdr_{tag}"] = head + armor_parts(p, headers=[h])
        det[f"s_d_hdr_{tag}"] = armor_parts(pd, headers=[h])
    inl["s_crc_qp_3d"] = head + armor_parts(p, crc=b"=3D" + crc_line(p)[1:])
    for tag, t in {"trail_cr_sp": b"\r ", "trail_tab": b"\t", "trail_sp": b" "}.items():
        inl[f"s_hash_hdr_{tag}"] = replace(
            ci, b"Hash: SHA256\n", b"Hash: SHA256" + t + b"\n", 1
        )
    return inl, det, {}


def batch_fx10_from(c: Controls) -> Batch:
    """The final review's adversarial lens's 274 key-free shapes (its batches
    b1, b2, b5, b6 and b8; PE/final-review/A-report.txt measures them), by its
    names, cut without a key from committed fixtures as its generator
    (PE/final-review/A/tools/gen_a.py) cut them, byte for byte."""
    return merge(
        fx10_armor(c), fx10_packet(c), fx10_extra(c), fx10_round3(c), fx10_round4(c)
    )


def batch_fx11_from(c: Controls) -> Batch:
    """The final review's verifier lens's 30 shapes (PE/final-review/V-report.txt),
    cut without a key from committed fixtures as its probes (V/probe/) cut
    them, but where noted.

    sx_ (V-C1, probe/straddle.py): A's good packet framed again with, inside
    its declared length after its fields, the header and fields of a v4
    signature X whose RSA MPI's bit count covers exactly the next framed
    packet, which go-crypto then reads as X's MPI and gpgv as a packet of its
    own: A's bad one, a v3 packet naming A or one with an unknown hash;
    unknownB and plain_good_badA are controls. v6_ and v4_ (V-C3,
    probe/v6.py): a v6 packet (Ed25519, a v6-form issuer no key has) before
    or after A's good v4 one; v4 Ed25519 and Ed448 packets from no key; their
    salts and signature values, random in the probe, are filler() here, so
    that --from-controls writes the same bytes at every run. ht_
    (probe/hashtag.py): A's good packet with its hash's left 16 bits flipped.
    u_ (V-I2): B's good packet before A's bad and good ones, in both orders.
    vc2_ (V-C2, probe/zz_alloc_test.go.txt): a v6 packet claiming a hashed
    area of 0xFFFFFFF0 octets, and a v4 packet whose unhashed embedded
    signature (32) holds a v6 header claiming 0x40000000, each alone; the
    probe built them in Go and measured only the cache, which allocates
    gibibytes before any check on them. vm1_ (V-M1,
    probe/zz_nilguard_test.go.txt): a v6 packet whose only issuer is an
    unhashed 16 naming A, before A's good one. vm3_ (V-M3, which the lens
    describes and did not build): an armor header holding the END marker.
    """
    head, sig_a, det_a = c.head, c.sig_a, c.det_a
    sig_a_other = c.sig_a_other  # A's bad packet, as gpg framed it

    def second(name: str) -> bytes:
        pkts = split_packets(sig_packets(c.read(name)))
        if len(pkts) != 2:
            raise ValueError(f"{name}: want two packets")
        return packet(pkts[1][0], pkts[1][1])

    sig_b = second("i_unknown_goodfirst.InRelease")  # framed new-format
    det_a_other = second("d_badsig_goodfirst.Release.gpg")
    det_b = second("d_unknown_goodfirst.Release.gpg")
    v3_i = second("xi_good_plus_v3garbage_A.InRelease")
    v3_d = second("xd_good_plus_v3garbage_A.Release.gpg")
    uh_i = second("xi_good_plus_unknownhash_A.InRelease")

    def straddle(good: bytes, hidden: bytes) -> bytes:
        x_fields = (
            bytes([4, 0x00, 1, 8])  # v4, binary, RSA, SHA-256
            + struct.pack(">H", 6)
            + bytes([5, 2])
            + struct.pack(">I", 1700000000)  # creation time
            + struct.pack(">H", 0)  # no unhashed subpacket
            + b"\x12\x34"  # hash tag
            + struct.pack(">H", 8 * len(hidden))  # the MPI's bit count
        )
        x_hdr = b"\xc2" + new_len(len(x_fields) + len(hidden))
        body = one_signature(good) + x_hdr + x_fields
        return b"\x89" + struct.pack(">H", len(body)) + body + hidden

    def v6(sigtype: int, label: str) -> bytes:
        fpr = b"\x06" + bytes.fromhex("a1b2c3d4e5f60718" * 4)
        hashed = build_subpackets(
            [(2, False, struct.pack(">I", 1700000000)), (33, False, fpr)]
        )
        body = bytes([6, sigtype, 27, 8])  # v6, type, Ed25519, SHA-256
        body += struct.pack(">I", len(hashed)) + hashed
        body += struct.pack(">I", 0)  # no unhashed subpacket
        body += b"\x12\x34" + bytes([16]) + filler(label + ":salt", 16)
        return packet(2, body + filler(label + ":signature", 64))

    def v4_algo(pk: int, sig_len: int, label: str) -> bytes:
        fpr = b"\x04" + bytes.fromhex("00112233445566778899aabbccddeeff01234567")
        hashed = build_subpackets(
            [(2, False, struct.pack(">I", 1700000000)), (33, False, fpr)]
        )
        body = bytes([4, 0x01, pk, 8]) + struct.pack(">H", len(hashed)) + hashed
        body += struct.pack(">H", 0) + b"\x12\x34" + filler(label, sig_len)
        return packet(2, body)

    def flip_tag(sig: bytes) -> bytes:
        s = parse_v4_sig(one_signature(sig))
        rest = bytes([s.rest[0] ^ 0xFF, s.rest[1] ^ 0xFF]) + s.rest[2:]
        return packet(
            2, build_v4_sig(V4Sig(s.head, s.hashed_raw, [], rest), s.unhashed)
        )

    def reframed(sig: bytes) -> bytes:
        s = parse_v4_sig(one_signature(sig))
        return packet(2, build_v4_sig(s, s.unhashed))

    # V-C2: a v6 packet whose four-octet hashed-area length claims 4 GiB.
    v6_huge = bytes([6, 0x00, 27, 8]) + struct.pack(">I", 0xFFFFFFF0) + b"\x41" * 32
    v6_huge_pkt = b"\xc2" + bytes([len(v6_huge)]) + v6_huge
    # V-C2: a v4 packet whose unhashed embedded signature (32) is a v6 header
    # claiming 1 GiB of hashed subpackets.
    emb = bytes([6, 0x18, 27, 8]) + struct.pack(">I", 0x40000000) + b"\x41" * 16
    sub32 = bytes([1 + len(emb), 32]) + emb
    v4e = bytes([4, 0x00, 1, 8, 0, 6, 5, 2, 0x65, 0, 0, 0])
    v4e += struct.pack(">H", len(sub32)) + sub32 + bytes([0x12, 0x34, 0x00, 0x08, 0xFF])
    v4_embedded_pkt = b"\xc2" + bytes([len(v4e)]) + v4e
    # V-M1: a v6 packet whose only issuer subpacket is an unhashed 16 naming A.
    kid_a = unhashed_of(sig_a)[2:10]
    hashed_v = bytes([5, 2, 0x65, 0, 0, 0])
    unhashed_v = bytes([9, 16]) + kid_a
    vm1 = bytes([6, 0x01, 27, 8]) + struct.pack(">I", len(hashed_v)) + hashed_v
    vm1 += struct.pack(">I", len(unhashed_v)) + unhashed_v
    vm1 += bytes([0x12, 0x34, 16]) + b"\x5a" * (16 + 64)
    vm1_pkt = b"\xc2" + bytes([len(vm1)]) + vm1
    end_in_header = "Comment: -----END PGP SIGNATURE-----\n"

    inline = {
        # V-C1: A's bad packet hidden from go-crypto inside X's MPI; the
        # controls (the hidden packet B's good one, no straddle).
        "sx_i_straddle_badA": head + armor(straddle(sig_a, sig_a_other)),
        "sx_i_straddle_unknownB": head + armor(straddle(sig_a, sig_b)),
        "sx_i_plain_good_badA": head + armor(sig_a + sig_a_other),
        "sx_i_straddle_v3garbageA": head + armor(straddle(sig_a, v3_i)),
        "sx_i_straddle_unknownhashA": head + armor(straddle(sig_a, uh_i)),
        # V-C3, and v4 EdDSA packets from no key.
        "v4_i_ed25519unknown_plus_good": head
        + armor(v4_algo(27, 64, "v4_i_ed25519unknown_plus_good") + sig_a),
        "v4_i_ed448unknown_plus_good": head
        + armor(v4_algo(28, 114, "v4_i_ed448unknown_plus_good") + sig_a),
        "v6_i_good_plus_v6unknown": head
        + armor(sig_a + v6(0x01, "v6_i_good_plus_v6unknown")),
        "v6_i_v6unknown_plus_good": head
        + armor(v6(0x01, "v6_i_v6unknown_plus_good") + sig_a),
        # The hash tag.
        "ht_i_control_reframed": head + armor(reframed(sig_a)),
        "ht_i_tag_flipped_only": head + armor(flip_tag(sig_a)),
        "ht_i_tag_flipped_plus_good": head + armor(flip_tag(sig_a) + sig_a),
        # V-I2: S1 past an unknown key's packet.
        "u_i_unknown_bad_good": head + armor(sig_b + sig_a_other + sig_a),
        "u_i_unknown_good_bad": head + armor(sig_b + sig_a + sig_a_other),
        # V-C2, V-M1, V-M3.
        "vc2_i_v6_hashed_len_4g": head + armor(v6_huge_pkt),
        "vc2_i_v4_embedded_v6_1g": head + armor(v4_embedded_pkt),
        "vm1_i_v6_unh16A_plus_good": head + armor(vm1_pkt + sig_a),
        "vm3_i_end_marker_in_header": head + armor(sig_a, headers=end_in_header),
    }
    detached = {
        "sx_d_straddle_badA": armor(straddle(det_a, det_a_other)),
        "sx_d_straddle_unknownB": armor(straddle(det_a, det_b)),
        "sx_d_plain_good_badA": armor(det_a + det_a_other),
        "sx_d_straddle_v3garbageA": armor(straddle(det_a, v3_d)),
        "v6_d_good_plus_v6unknown": armor(det_a + v6(0x00, "v6_d_good_plus_v6unknown")),
        "v6_d_v6unknown_plus_good": armor(v6(0x00, "v6_d_v6unknown_plus_good") + det_a),
        "ht_d_control_reframed": armor(reframed(det_a)),
        "ht_d_tag_flipped_only": armor(flip_tag(det_a)),
        "ht_d_tag_flipped_plus_good": armor(flip_tag(det_a) + det_a),
        "vc2_d_v6_hashed_len_4g": armor(v6_huge_pkt),
        "vc2_d_v4_embedded_v6_1g": armor(v4_embedded_pkt),
        "vm3_d_end_marker_in_header": armor(det_a, headers=end_in_header),
    }
    return inline, detached, {}


def batch_fx12_from(c: Controls) -> Batch:
    """DN-11's new controls that need no key (the hardening plan's Task 9a):
    a binary-mode (0x00) packet from a key the clients lack first in an
    InRelease, before A's good text-mode one (B's packet, its signature type
    set to 0x00: no client can check it either way); a detached armor in
    76-character lines (the inline one is x4i_b64_line_76); and A's good
    inline packet as a detached Release.gpg of Release, the text-mode twin
    of the inline control (gpg's clearsigning of Release hashes exactly what
    a text-mode signature of it does: Release with CRLF line ends)."""
    head, sig_a, det_a = c.head, c.sig_a, c.det_a
    whole = sig_packets(c.read("i_unknown_unknownfirst.InRelease"))
    sig_b = whole[: len(whole) - len(sig_a)]
    if len(frames(sig_b)) != 1 or sig_b[0] != 0x89:
        raise ValueError("i_unknown_unknownfirst does not start with B's packet")
    b_binary = sig_b[:4] + b"\x00" + sig_b[5:]  # 0x89, two-octet length, v4, type
    inline = {"n_i_unknown00_first": head + armor(b_binary + sig_a)}
    detached = {
        "n_d_b64_width_76": armor(det_a, width=76),
        "n_d_textmode_twin": armor(sig_a),
    }
    return inline, detached, {}


@dataclass(frozen=True)
class OwnMaterial:
    """What batch fx13 is cut from: key O's signatures (see the module
    comment), gpg's whole output for the clearsignings."""

    plain: bytes  # RELEASE clearsigned by O
    dash: bytes  # DASH_RELEASE clearsigned by O (gpg dash-escapes "-x: y")
    empty: bytes  # RELEASE + an empty line, clearsigned by O
    det_binary: bytes  # O's binary-mode (0x00) detached signature of RELEASE
    det_text: bytes  # O's text-mode (0x01) detached signature of RELEASE
    sha224: bytes  # RELEASE clearsigned by O with SHA-224
    sha3_256: bytes  # RELEASE clearsigned by O with SHA3-256 (sha3_clearsign)
    nginx: bytes  # RELEASE without its final newline clearsigned by O


def sha3_clearsign(g: GnuPG, o: str, text: bytes, cleartext: bytes) -> bytes:
    """text clearsigned by key O with SHA3-256, laid out as gpg's SHA-256
    clearsigning of it (cleartext): its header naming SHA3-256, its
    signature packet's fields and subpackets gpg's, only the hash
    algorithm and the signature new.

    AIDEV-NOTE: gpg 2.4 cannot sign with SHA3-256 (its hash list ends at
    SHA224), and libgcrypt can: the packet is built here and its hash
    signed by the temporary home's gpg-agent (PKSIGN, which wraps it in
    SHA3-256's DigestInfo), so that the secret key never leaves the home.
    Two checks, against O's public key, make the bytes trustworthy without
    a verifier that knows SHA3: gpg's own SHA-256 signature must verify over
    the canonical text this signs (text with CRLF line ends, as gpg's
    clearsigning hashes a text whose lines carry no trailing blanks and none
    starts with a dash), and the new SHA3-256 signature must verify too."""
    if any(
        line != line.rstrip(b" \t") or line.startswith(b"-")
        for line in text.split(b"\n")
    ):
        raise ValueError("sha3_clearsign: text with trailing blanks or a dash line")
    canonical = text.replace(b"\n", b"\r\n")
    n, e = rsa_public_key(g.export(o))
    k = (n.bit_length() + 7) // 8
    gpg_body = one_signature(sig_packets(cleartext))
    if pow(rsa_signature_value(gpg_body), e, n) != emsa_pkcs1_v15(
        DIGEST_INFO_SHA256, signed_digest(gpg_body, canonical, sha256), k
    ):
        raise RuntimeError("sha3_clearsign: gpg's signature does not cover CRLF text")
    head, hashed, unhashed, _ = v4_parts(gpg_body)
    if head[3] != 8:
        raise ValueError("sha3_clearsign: cleartext is not a SHA-256 signing")
    head = head[:3] + bytes([OPENPGP_SHA3_256])
    partial = v4_build(head, hashed, unhashed, b"")
    digest = signed_digest(partial, canonical, sha3_256)
    value = g.agent_sign(g.keygrip(o), GCRY_MD_SHA3_256, digest)
    if pow(int.from_bytes(value, "big"), e, n) != emsa_pkcs1_v15(
        DIGEST_INFO_SHA3_256, digest, k
    ):
        raise RuntimeError(
            "sha3_clearsign: gpg-agent's SHA3-256 signature does not verify"
        )
    body = partial + digest[:2] + mpi(value)
    pkt = b"\x89" + struct.pack(">H", len(body)) + body  # gpg's framing
    header = replace(
        cleartext[: cleartext.index(BEGIN)], b"Hash: SHA256\n", b"Hash: SHA3-256\n", 1
    )
    return header + armor(pkt)


def make_own_material(g: GnuPG, o: str) -> OwnMaterial:
    """Signs what batch fx13 needs, with key O."""
    plain = g.clearsign(o, RELEASE)
    return OwnMaterial(
        plain=plain,
        dash=g.clearsign(o, DASH_RELEASE),
        empty=g.clearsign(o, RELEASE + b"\n"),
        det_binary=g.detach_sign(o, RELEASE),
        det_text=g.detach_sign(o, RELEASE, textmode=True),
        sha224=g.clearsign(o, RELEASE, digest="SHA224"),
        sha3_256=sha3_clearsign(g, o, RELEASE, plain),
        nginx=g.clearsign(o, RELEASE.rstrip(b"\n")),
    )


def batch_fx13(om: OwnMaterial) -> Batch:
    """The shapes that need a signer but no other key, made with key O.

    The adversarial lens's own-key batches, by its names, built as its
    scripts built them (PE/final-review/A/tools/ownkey.sh, ownkey2.sh, with
    its gen_a.py's batch_own), with O for its keys K and K2: b3, the
    dash-escape family (``o_``: a signed "-x: y" line, which gpg escapes as
    "- -x: y", un-escaped or escaped with two spaces; a text ending in two
    empty lines, either or both written "- "); b7, the signature types a
    signer chooses (``o2_``: a binary-mode signature in an InRelease, a
    text-mode Release.gpg over Release, as signed and with CRLFs, and their
    controls).

    DN-11's new controls that need a signer: SHA-224 and SHA3-256
    signatures, and nginx's layout, an InRelease with CRLF line ends
    throughout whose signed text ends without a newline (so no empty line
    before the signature), whose detached twin is o2_textmode_detached (a
    text-mode Release.gpg; nginx's own is binary, which every apt refuses:
    d_binary)."""
    begin_text = b"Packages\n\n\n" + BEGIN  # RELEASE + an empty line, as gpg writes it
    inline = {
        "o_plain_control": om.plain,
        "o_dash_control": om.dash,
        "o_dash_unescaped": replace(om.dash, b"\n- -x: y\n", b"\n-x: y\n", 1),
        "o_dash_double_space": replace(om.dash, b"\n- -x: y\n", b"\n-  -x: y\n", 1),
        "o_empty2_control": om.empty,
        "o_empty2_both_dash": replace(
            om.empty, begin_text, b"Packages\n- \n- \n" + BEGIN, 1
        ),
        "o_empty2_first_dash": replace(
            om.empty, begin_text, b"Packages\n- \n\n" + BEGIN, 1
        ),
        "o2_clearsign_control": om.plain,
        "o2_binary_sig_in_clearsign": om.plain[: om.plain.index(BEGIN)]
        + armor(om.det_binary),
        "n_i_sha224": om.sha224,
        "n_i_sha3_256": om.sha3_256,
        "n_i_nginx_crlf": replace(om.nginx, b"\n", b"\r\n"),
    }
    detached = {
        "o2_textmode_detached": armor(om.det_text),
        "o2_textmode_detached_release_crlf": armor(om.det_text),
        "o2_binary_detached_control": armor(om.det_binary),
    }
    releases = {"o2_textmode_detached_release_crlf": RELEASE.replace(b"\n", b"\r\n")}
    return inline, detached, releases


# The batches cut from Material, in a run that makes keys.
BATCHES: list[Callable[[Material], tuple[Shapes, Shapes]]] = [
    batch_fx,
    batch_fx2,
    batch_fx3,
    batch_fx4,
    batch_fx5,
    batch_fx6,
    batch_fx7,
]

# The batches cut without a key from committed fixtures, which
# ``--from-controls`` writes alone: Controls -> shapes. A run that makes keys
# cuts them from the control files it has just built.
FROM_CONTROLS: list[Callable[[Controls], Batch]] = [
    batch_fx8_from,
    batch_fx9_from,
    batch_fx10_from,
    batch_fx11_from,
    batch_fx12_from,
]


@dataclass
class Corpus:
    """Shapes by form, each name once, and the own Release of the detached
    shapes that have one."""

    inline: Shapes = field(default_factory=dict)
    detached: Shapes = field(default_factory=dict)
    releases: Shapes = field(default_factory=dict)

    def add(
        self, inline: Shapes, detached: Shapes, releases: Shapes | None = None
    ) -> None:
        """Adds a batch's shapes; a name used twice, or one that is no
        [A-Za-z0-9_]+, fails."""
        for shapes, into in ((inline, self.inline), (detached, self.detached)):
            for name, data in shapes.items():
                if not NAME_RE.fullmatch(name):
                    raise ValueError(f"shape name {name!r}: want [A-Za-z0-9_]+")
                if name in self.inline or name in self.detached:
                    raise ValueError(f"shape {name} is defined twice")
                into[name] = data
        for name, data in (releases or {}).items():
            if name not in detached:
                raise ValueError(
                    f"{name}.Release: no detached shape {name} in its batch"
                )
            self.releases[name] = data

    def files(self) -> dict[str, bytes]:
        """The shapes' files, by file name."""
        out = {f"{n}.InRelease": d for n, d in self.inline.items()}
        out |= {f"{n}.Release.gpg": d for n, d in self.detached.items()}
        out |= {f"{n}.Release": d for n, d in self.releases.items()}
        return out


def build_shapes(m: Material, om: OwnMaterial, keyring: bytes) -> Corpus:
    """Every batch's shapes: those cut from Material, those cut from the
    control files among them (as --from-controls cuts them from committed
    ones), and batch fx13; names are unique."""
    corpus = Corpus()
    for batch in BATCHES:
        corpus.add(*batch(m))
    controls = controls_from(corpus.files() | {"keys.gpg": keyring, "Release": RELEASE})
    for batch_from in FROM_CONTROLS:
        corpus.add(*batch_from(controls))
    corpus.add(*batch_fx13(om))
    return corpus


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


def write_corpus(out: Path, corpus: Corpus, keyring: bytes) -> None:
    """Replaces OUT's fixtures (verdicts.txt and classes.txt are left as
    they are)."""
    for p in corpus_entries(out):
        if p.name not in MEASUREMENT_FILES:
            p.unlink()
    out.mkdir(parents=True, exist_ok=True)
    (out / "Release").write_bytes(RELEASE)
    (out / "keys.gpg").write_bytes(keyring)
    for file, data in corpus.files().items():
        (out / file).write_bytes(data)


def write_files(out: Path, files: dict[str, bytes]) -> None:
    """Writes files into OUT, none of them a control file."""
    for file in files:
        if file in CONTROL_FILES:
            raise ValueError(f"{file} would overwrite a control")
    for file, data in files.items():
        (out / file).write_bytes(data)


def write_from_controls(out: Path) -> Corpus:
    """Writes into OUT the FROM_CONTROLS batches, cut from OUT's committed
    fixtures (CONTROL_FILES), and nothing else: no key is made, so every
    measured fixture keeps its bytes. Returns what it wrote."""
    controls = controls_from_corpus(out)
    corpus = Corpus()
    for batch in FROM_CONTROLS:
        corpus.add(*batch(controls))
    write_files(out, corpus.files())
    return corpus


def write_own_key(out: Path) -> Corpus:
    """Makes key O in a temporary GnuPG home, writes batch fx13 into OUT and
    replaces O's public key in OUT's keys.gpg (adding it if it has none),
    every other key's packets byte for byte; writes nothing else. Returns
    what it wrote."""
    if not (out / "keys.gpg").is_file():
        raise SystemExit(f"{out}: no keys.gpg: generate the corpus first")
    corpus_entries(out)  # refuse a wrong OUT before making a key
    old = (out / "keys.gpg").read_bytes()
    g = GnuPG()
    try:
        o = make_own_key(g)
        om = make_own_material(g, o)
        own_export = g.export(o)
    finally:
        g.close()
    keyring = with_own_key(old, own_export, o)
    corpus = Corpus()
    corpus.add(*batch_fx13(om))
    write_files(out, corpus.files())
    (out / "keys.gpg").write_bytes(keyring)
    return corpus


def exit_on_signal(signum: int, _frame: FrameType | None) -> None:
    """SIGTERM or SIGHUP: unwind with exit status 128 + signum. Once: later
    ones are ignored, so a second cannot cut the clean-up short."""
    for s in EXIT_SIGNALS:
        signal.signal(s, signal.SIG_IGN)
    raise SystemExit(128 + signum)


def main(argv: list[str]) -> int:
    # AIDEV-NOTE: Python's default SIGTERM and SIGHUP kill the process
    # without unwinding: the finally below would not run close(), leaving
    # the GnuPG home, its secret keys and its agent (the Task 5 review saw
    # it). As an exception, the signal unwinds through it; subprocess.run
    # kills the gpg it waits for. Only SIGKILL still leaves the home.
    for s in EXIT_SIGNALS:
        signal.signal(s, exit_on_signal)
    args = argv[1:]
    mode = args[0] if args and args[0] in ("--from-controls", "--own-key") else ""
    if mode:
        args = args[1:]
    if len(args) != 1:
        print(
            "usage: python3 -I e2e/apt-oracle/gen_shapes.py"
            " [--from-controls | --own-key] OUT",
            file=sys.stderr,
        )
        return 2
    out = Path(args[0])
    if mode:
        corpus = (
            write_from_controls(out)
            if mode == "--from-controls"
            else write_own_key(out)
        )
        what = (
            f"cut from {out}'s committed fixtures"
            if mode == "--from-controls"
            else f"signed by a new key O, whose public key replaces O's in {out}/keys.gpg"
        )
        n_inline, n_detached = len(corpus.inline), len(corpus.detached)
        print(
            f"wrote {n_inline + n_detached} shapes ({n_inline} inline, "
            f"{n_detached} detached) {what}; every other file is as it was: "
            "measure the new rows with bash e2e/apt-oracle/run.sh --record"
        )
        return 0
    corpus_entries(out)  # refuse a wrong OUT before making keys
    g = GnuPG()
    try:
        keys = make_keys(g)
        material = make_material(g, keys)
        own = make_own_material(g, keys.o)
        keyring = g.export(keys.a, keys.e, keys.s, keys.o)
    finally:
        g.close()
    check_keyring(keyring, {keys.a, keys.e, keys.s, keys.s_sub, keys.o})
    corpus = build_shapes(material, own, keyring)
    write_corpus(out, corpus, keyring)
    total = len(corpus.inline) + len(corpus.detached)
    print(
        f"wrote {total} shapes ({len(corpus.inline)} inline, {len(corpus.detached)}"
        f" detached), Release and keys.gpg (A, E, S, O) to {out}"
    )
    if any((out / f).exists() for f in MEASUREMENT_FILES):
        print(
            "verdicts.txt and classes.txt measured the old fixtures: re-measure"
            " with bash e2e/apt-oracle/run.sh --record"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
