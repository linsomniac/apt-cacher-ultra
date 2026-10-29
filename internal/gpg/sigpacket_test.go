package gpg

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// TestParseSigPackets_Framing pins P1 on what the corpus does not hold: one
// signature packet framed in each of the six length encodings of RFC 9580
// §4.2, two packets and eight are accepted; a partial or an indeterminate
// length, a truncated packet, a packet of another type, a byte that starts
// no packet, or a ninth packet is refused — though go-crypto's reader would
// parse the partial and indeterminate forms as a signature.
func TestParseSigPackets_Framing(t *testing.T) {
	small := newEdDSAEntity(t)
	large := newTestEntity(t, "Signer", "signer@example.com")
	bodyOf := func(e *openpgp.Entity) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := openpgp.DetachSign(&buf, e, strings.NewReader(fakeReleasePlaintext), nil); err != nil {
			t.Fatal(err)
		}
		sig := buf.Bytes()
		// go-crypto writes new-format headers: 0xC2, then a one- or
		// two-octet length.
		switch {
		case sig[0] == 0xc2 && sig[1] < 192:
			return sig[2:]
		case sig[0] == 0xc2 && sig[1] < 224:
			return sig[3:]
		}
		t.Fatalf("test premise: an unexpected header % x", sig[:3])
		return nil
	}
	smallBody, largeBody := bodyOf(small), bodyOf(large)
	if len(smallBody) >= 192 || len(largeBody) < 256 {
		t.Fatalf("test premise: body lengths %d and %d, want one under 192 and one over 255", len(smallBody), len(largeBody))
	}
	be32 := func(n int) []byte { return []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)} }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	l := len(largeBody) - 192
	one := cat([]byte{0xc2, byte(len(smallBody))}, smallBody)
	for name, framed := range map[string][]byte{
		"new format, one-octet length":  one,
		"new format, two-octet length":  cat([]byte{0xc2, byte(l>>8) + 192, byte(l)}, largeBody),
		"new format, five-octet length": cat([]byte{0xc2, 0xff}, be32(len(largeBody)), largeBody),
		"old format, one-octet length":  cat([]byte{0x88, byte(len(smallBody))}, smallBody),
		"old format, two-octet length":  cat([]byte{0x89, byte(len(largeBody) >> 8), byte(len(largeBody))}, largeBody),
		"old format, four-octet length": cat([]byte{0x8a}, be32(len(largeBody)), largeBody),
		"two packets":                   cat(one, []byte{0x88, byte(len(smallBody))}, smallBody),
		"eight packets":                 bytes.Repeat(one, sigMaxPackets),
	} {
		if _, err := parseSigPackets(framed); err != nil {
			t.Errorf("%s: %v; want accepted", name, err)
		}
	}
	// A partial length: a one-octet first part (0xE0), then the rest.
	partial := cat([]byte{0xc2, 0xe0}, smallBody[:1], []byte{byte(len(smallBody) - 1)}, smallBody[1:])
	indeterminate := cat([]byte{0x8b}, smallBody)
	for _, goCryptoParses := range [][]byte{partial, indeterminate} {
		if p, err := packet.Read(bytes.NewReader(goCryptoParses)); err != nil {
			t.Fatalf("test premise: go-crypto does not parse % x…: %v", goCryptoParses[:2], err)
		} else if _, ok := p.(*packet.Signature); !ok {
			t.Fatalf("test premise: go-crypto parses % x… as a %T", goCryptoParses[:2], p)
		}
	}
	for name, framed := range map[string][]byte{
		"a partial length":        partial,
		"an indeterminate length": indeterminate,
		"a truncated body":        cat([]byte{0xc2, byte(len(smallBody))}, smallBody[:len(smallBody)-1]),
		"a truncated header":      {0xc2, 0xff, 0x00},
		"a marker packet":         cat(one, []byte{0xca, 0x03, 'P', 'G', 'P'}),
		"an old-format marker":    cat(one, []byte{0xa8, 0x03, 'P', 'G', 'P'}),
		"a byte that is no tag":   cat(one, []byte{0x00}),
		"nine packets":            bytes.Repeat(one, sigMaxPackets+1),
		"no byte at all":          nil, // A4 never hands one over: the bypass's "at least one packet"
	} {
		if _, err := parseSigPackets(framed); ruleOf(err) != "P1" {
			t.Errorf("%s: %v; want refused by P1", name, err)
		}
	}
}

// newEdDSAEntity makes an EdDSA signing key, whose signature packets are
// short enough (under 192 octets) for the one-octet length encodings.
func newEdDSAEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("EdDSA", "", "eddsa@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatalf("NewEntity (EdDSA): %v", err)
	}
	return e
}

// Hand-framed signature packets for P2–P5 (TestHashedIssuer's builders,
// moved here with it): subpackets in any length form, and a v4 packet of any
// fields, parsed back by go-crypto in P5.
const testIDA, testIDB = uint64(0x1111222233334444), uint64(0x5555666677778888)

// tsub frames a subpacket (RFC 9580 §5.2.3.7), its length in the shortest
// form: one octet below 192, two below 16320, else five.
func tsub(typ byte, data ...byte) []byte {
	var l []byte
	switch n := 1 + len(data); {
	case n < 192:
		l = []byte{byte(n)}
	case n < 16320:
		l = []byte{byte((n-192)>>8) + 192, byte(n - 192)}
	default:
		l = binary.BigEndian.AppendUint32([]byte{0xff}, uint32(n))
	}
	return append(append(l, typ), data...)
}

// tsub5 is tsub with a five-octet length, whatever the size.
func tsub5(typ byte, data ...byte) []byte {
	return append(append(binary.BigEndian.AppendUint32([]byte{0xff}, uint32(1+len(data))), typ), data...)
}

func tkeyID(id uint64) []byte { return binary.BigEndian.AppendUint64(nil, id) }

// tv4fpr is a v4 fingerprint subpacket's data: version 4, then 20 bytes
// whose last 8 are the key ID.
func tv4fpr(id uint64) []byte {
	return append(append([]byte{4}, bytes.Repeat([]byte{0xab}, 12)...), tkeyID(id)...)
}

// tv6fpr is a v6 fingerprint subpacket's data: its key ID its first 8 bytes.
func tv6fpr(id uint64) []byte {
	return append(append([]byte{6}, tkeyID(id)...), bytes.Repeat([]byte{0xcd}, 24)...)
}

var (
	tcreated = tsub(2, 0x65, 0x00, 0x00, 0x00) // signature creation time, 2023-11-14
	tmpi8    = []byte{0x00, 0x08, 0xff}        // an 8-bit MPI
)

// tpacket frames body new-format: a one-, two- or five-octet length.
func tpacket(body []byte) []byte {
	switch n := len(body); {
	case n < 192:
		return append([]byte{0xc2, byte(n)}, body...)
	case n < 8384:
		return append([]byte{0xc2, byte((n-192)>>8) + 192, byte(n - 192)}, body...)
	default:
		return append(binary.BigEndian.AppendUint32([]byte{0xc2, 0xff}, uint32(n)), body...)
	}
}

// tsig is a v4 signature packet: the given type and algorithms, areas and
// fields after the hash's left 16 bits.
func tsig(version, sigType, pk, hash byte, hashed, unhashed, fields []byte) []byte {
	body := []byte{version, sigType, pk, hash}
	body = binary.BigEndian.AppendUint16(body, uint16(len(hashed)))
	body = append(body, hashed...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(unhashed)))
	body = append(body, unhashed...)
	body = append(append(body, 0x12, 0x34), fields...)
	return tpacket(body)
}

// trsa is tsig for a v4 RSA SHA-256 binary signature with an 8-bit MPI.
func trsa(hashed, unhashed []byte) []byte {
	return tsig(4, 0x00, 1, 8, hashed, unhashed, tmpi8)
}

// TestParseSigPackets_Body pins P2 and P3: the version, type and
// algorithms layer P reads, the field layout each algorithm implies, the
// areas' limits and framing, minimal MPIs and exact consumption.
func TestParseSigPackets_Body(t *testing.T) {
	iss := joined(tcreated, tsub(16, tkeyID(testIDA)...))
	ed := func(pk byte, n int) []byte { return tsig(4, 0x00, pk, 8, iss, nil, bytes.Repeat([]byte{0x5a}, n)) }
	note := func(n int) []byte { // a notation subpacket whose value is n bytes
		const name = "pad@example.invalid"
		d := binary.BigEndian.AppendUint16([]byte{0x80, 0, 0, 0}, uint16(len(name)))
		d = binary.BigEndian.AppendUint16(d, uint16(n))
		return tsub(20, append(append(d, name...), bytes.Repeat([]byte{'x'}, n)...)...)
	}
	area := func(size int) []byte { // a hashed area of exactly size octets
		n := size - len(iss) - 3 - 8 - 19 // tsub's two-octet length and type, the notation's fields and name
		a := joined(iss, note(n))
		if len(a) != size {
			t.Fatalf("test premise: an area of %d octets, want %d", len(a), size)
		}
		return a
	}
	for _, tc := range []struct {
		name string
		pkt  []byte
		rule string
	}{
		{"a v4 RSA signature", trsa(iss, nil), ""},
		{"text mode", tsig(4, 0x01, 1, 8, iss, nil, tmpi8), ""},
		{"RSA sign-only", tsig(4, 0x00, 3, 8, iss, nil, tmpi8), ""},
		{"SHA-1, SHA-384, SHA-512", joined(tsig(4, 0x00, 1, 2, iss, nil, tmpi8), tsig(4, 0x00, 1, 9, iss, nil, tmpi8), tsig(4, 0x00, 1, 10, iss, nil, tmpi8)), ""},
		{"DSA, two MPIs", tsig(4, 0x00, 17, 8, iss, nil, joined(tmpi8, tmpi8)), ""},
		{"ECDSA, two MPIs", tsig(4, 0x00, 19, 8, iss, nil, joined(tmpi8, tmpi8)), ""},
		{"EdDSA, two MPIs", tsig(4, 0x00, 22, 8, iss, nil, joined(tmpi8, tmpi8)), ""},
		{"Ed25519, 64 octets", ed(27, 64), ""},
		{"Ed448, 114 octets", ed(28, 114), ""},
		{"a 10,000-octet hashed area", trsa(area(10000), nil), ""},
		{"version 3", tsig(3, 0x00, 1, 8, iss, nil, tmpi8), "P2"},
		{"version 5", tsig(5, 0x00, 1, 8, iss, nil, tmpi8), "P2"},
		{"version 6", tsig(6, 0x00, 1, 8, iss, nil, tmpi8), "P2"},
		{"a body too short", tpacket([]byte{4, 0, 1, 8, 0}), "P2"},
		{"a key-binding signature type", tsig(4, 0x18, 1, 8, iss, nil, tmpi8), "P2"},
		{"public-key algorithm 18 (ECDH)", tsig(4, 0x00, 18, 8, iss, nil, tmpi8), "P2"},
		{"public-key algorithm 99", tsig(4, 0x00, 99, 8, iss, nil, tmpi8), "P2"},
		{"SHA-224", tsig(4, 0x00, 1, 11, iss, nil, tmpi8), "P2"},
		{"SHA3-256", tsig(4, 0x00, 1, 12, iss, nil, tmpi8), "P2"},
		{"RIPEMD-160", tsig(4, 0x00, 1, 3, iss, nil, tmpi8), "P2"},
		{"a 10,001-octet hashed area", trsa(area(10001), nil), "P3"},
		{"a 10,001-octet unhashed area", trsa(iss, note(10001-3-8-19)), "P3"},
		{"a subpacket of length 0", trsa(joined(iss, []byte{0}), nil), "P3"},
		{"a subpacket past its area", trsa(joined(iss, []byte{9, 20}), nil), "P3"},
		{"a two-octet length past its area", trsa(joined(iss, []byte{200}), nil), "P3"},
		{"a five-octet length past its area", trsa(joined(iss, []byte{255, 0, 0}), nil), "P3"},
		{"an area past the body", tpacket([]byte{4, 0, 1, 8, 0x10, 0x00, 1, 2}), "P3"},
		{"no left 16 bits", tpacket(append(binary.BigEndian.AppendUint16(append([]byte{4, 0, 1, 8, 0, byte(len(iss))}, iss...), 0), 0x12)), "P3"},
		{"a byte after the MPI", tsig(4, 0x00, 1, 8, iss, nil, joined(tmpi8, []byte{0})), "P3"},
		{"a second MPI where RSA has one", tsig(4, 0x00, 1, 8, iss, nil, joined(tmpi8, tmpi8)), "P3"},
		{"DSA with one MPI", tsig(4, 0x00, 17, 8, iss, nil, tmpi8), "P3"},
		{"an MPI with a leading zero octet", tsig(4, 0x00, 1, 8, iss, nil, []byte{0x00, 0x10, 0x00, 0xff}), "P3"},
		{"an MPI of zero bits", tsig(4, 0x00, 1, 8, iss, nil, []byte{0x00, 0x00}), "P3"},
		{"an MPI declaring one bit too many", tsig(4, 0x00, 1, 8, iss, nil, []byte{0x00, 0x09, 0xff}), "P3"},
		{"an MPI declaring one bit too few", tsig(4, 0x00, 1, 8, iss, nil, []byte{0x00, 0x07, 0xff}), "P3"},
		{"an MPI past the body", tsig(4, 0x00, 1, 8, iss, nil, []byte{0x00, 0x10, 0xff}), "P3"},
		{"Ed25519, 63 octets", ed(27, 63), "P3"},
		{"Ed25519, 65 octets", ed(27, 65), "P3"},
		{"Ed448, 113 octets", ed(28, 113), "P3"},
	} {
		if _, err := parseSigPackets(tc.pkt); ruleOf(err) != tc.rule {
			t.Errorf("%s: %v (rule %q); want rule %q", tc.name, err, ruleOf(err), tc.rule)
		}
	}
}

// TestParseSigPackets_OneIssuer pins P4: the subpacket types refused in
// either area, the critical ones gpgv 2.2 handles, one creation time, and
// one issuer that every reader takes — exactly one issuer key ID (16), and
// an issuer fingerprint (33) only in the v4 form naming the same key. The
// issuer cases are TestHashedIssuer's (ruling HD-14, deleted with s1Issuer):
// where gpgv and go-crypto took the issuer from different subpackets, the
// packet is now refused; where they agree, accepted with that issuer.
func TestParseSigPackets_OneIssuer(t *testing.T) {
	a16, b16 := tsub(16, tkeyID(testIDA)...), tsub(16, tkeyID(testIDB)...)
	a33, b33 := tsub(33, tv4fpr(testIDA)...), tsub(33, tv4fpr(testIDB)...)
	note := tsub(20, append(binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16([]byte{0x80, 0, 0, 0}, 1), 300), append([]byte{'n'}, bytes.Repeat([]byte{'x'}, 300)...)...)...)
	crit := func(typ byte, data ...byte) []byte { s := tsub(typ, data...); s[1] |= 0x80; return s }
	for _, tc := range []struct {
		name             string
		hashed, unhashed []byte
		rule             string
		fpr              bool // accepted with the 33's fingerprint
	}{
		{"hashed 16", joined(tcreated, a16), nil, "", false},
		{"hashed 33 and 16, one key: go-crypto's layout", joined(tcreated, a16, a33), nil, "", true},
		{"hashed 33, unhashed 16, one key: gpg's layout", joined(a33, tcreated), a16, "", true},
		{"a critical hashed 16", joined(tcreated, crit(16, tkeyID(testIDA)...)), nil, "", false},
		{"a two-octet-length notation before the issuers", joined(tcreated, note, a33), joined(note, a16), "", true},
		{"a five-octet length for small subpackets", joined(tsub5(2, 0x65, 0, 0, 0), tsub5(16, tkeyID(testIDA)...)), nil, "", false},
		{"an unhashed creation time beside the hashed one", joined(tcreated, a16), tcreated, "", false},
		{"a critical key block of version 0, unhashed", joined(tcreated, a16), crit(38, 0, 1, 2), "", false},
		{"hashed 33 alone", joined(tcreated, a33), nil, "P4", false},
		{"hashed 33, unhashed 16, two keys", joined(tcreated, a33), b16, "P4", false},
		{"hashed 16, then a 33 naming another key", joined(tcreated, b16, a33), nil, "P4", false},
		{"a v6-form 33 in a v4 signature", joined(tcreated, a16, tsub(33, tv6fpr(testIDA)...)), nil, "P4", false},
		{"a first 33 in v6 form, then a v4 one", joined(tcreated, a16, tsub(33, tv6fpr(testIDB)...), a33), nil, "P4", false},
		{"a 33 of version 5, 20 octets", joined(tcreated, a16, tsub(33, append([]byte{5}, tv4fpr(testIDA)[1:]...)...)), nil, "P4", false},
		{"two hashed 16s", joined(tcreated, a16, b16), nil, "P4", false},
		{"the same 16 twice", joined(tcreated, a16), a16, "P4", false},
		{"a hashed 16 before an unhashed one", joined(tcreated, a16), b16, "P4", false},
		{"nothing hashed; unhashed 16, then 33", tcreated, joined(a16, b33), "P4", false},
		{"nothing hashed; two unhashed 16s", tcreated, joined(a16, b16), "P4", false},
		{"nothing hashed; unhashed 33", tcreated, a33, "P4", false},
		{"two 33s naming the key", joined(tcreated, a16, a33), a33, "P4", false},
		{"a 16 of 7 octets", joined(tcreated, tsub(16, tkeyID(testIDA)[:7]...)), nil, "P4", false},
		{"no creation time", a16, nil, "P4", false},
		{"two creation times", joined(tcreated, tcreated, a16), nil, "P4", false},
		{"a creation time of 5 octets", joined(tsub(2, 0x65, 0, 0, 0, 0), a16), nil, "P4", false},
		{"an embedded signature, unhashed", joined(tcreated, a16), tsub(32, 4, 0x19), "P4", false},
		{"an embedded signature, hashed", joined(tcreated, a16, tsub(32, 4, 0x19)), nil, "P4", false},
		{"a critical notation", joined(tcreated, a16, crit(20, 0x80, 0, 0, 0, 0, 1, 0, 1, 'n', 'v')), nil, "P4", false},
		{"a critical unknown subpacket, unhashed", joined(tcreated, a16), crit(100, 1), "P4", false},
		{"a critical signer's user ID, unhashed", joined(tcreated, a16), crit(28, 'u'), "P4", false},
		{"a critical key block of version 1, unhashed", joined(tcreated, a16), crit(38, 1, 1, 2), "P4", false},
	} {
		pkts, err := parseSigPackets(trsa(tc.hashed, tc.unhashed))
		if ruleOf(err) != tc.rule {
			t.Errorf("%s: %v (rule %q); want rule %q", tc.name, err, ruleOf(err), tc.rule)
			continue
		}
		if err == nil && (pkts[0].issuer != testIDA || (pkts[0].fpr != nil) != tc.fpr || tc.fpr && !bytes.Equal(pkts[0].fpr, tv4fpr(testIDA)[1:])) {
			t.Errorf("%s: issuer %016X, fingerprint %x; want %016X, a fingerprint %v", tc.name, pkts[0].issuer, pkts[0].fpr, testIDA, tc.fpr)
		}
	}
	// go-crypto's own v4 signature: one hashed 16 and one hashed 33 of
	// one key, accepted with that key's issuer.
	e := newTestEntity(t, "V4", "v4@example.com")
	pkts, err := parseSigPackets(detachSignWith(t, e, []byte(fakeReleasePlaintext), false))
	if err != nil || pkts[0].issuer != e.PrimaryKey.KeyId || !bytes.Equal(pkts[0].fpr, e.PrimaryKey.Fingerprint) {
		t.Errorf("go-crypto's v4 signature: %v; want its signer's issuer and fingerprint", err)
	}
}

// TestParseSigPackets_GoCryptoMustAgree pins P5: a packet P1–P4 pass but
// go-crypto does not parse, or reads otherwise, is refused. A critical
// revocation key subpacket (12) is one gpgv 2.2 handles and go-crypto does
// not know (it refuses an unknown critical hashed subpacket, which agrees
// with gpgv's BADSIG for one gpgv lacks); a primary user ID subpacket (25)
// of two octets, one go-crypto finds malformed.
func TestParseSigPackets_GoCryptoMustAgree(t *testing.T) {
	a16 := tsub(16, tkeyID(testIDA)...)
	revoker := tsub(12, append([]byte{0x80, 1}, bytes.Repeat([]byte{0x77}, 20)...)...)
	revoker[1] |= 0x80
	for name, pkt := range map[string][]byte{
		"a critical revocation key":   trsa(joined(tcreated, a16, revoker), nil),
		"a two-octet primary user ID": trsa(joined(tcreated, a16, tsub(25, 1, 1)), nil),
	} {
		if _, err := parseSigPackets(pkt); ruleOf(err) != "P5" {
			t.Errorf("%s: %v (rule %q); want P5", name, err, ruleOf(err))
		}
	}
}

// TestGpgvChecks: gpgv checks every packet when they share one signature
// type, else the first only (gnupg's proc_tree; the corpus' batch fx14).
func TestGpgvChecks(t *testing.T) {
	p := func(types ...byte) []sigPacket {
		var out []sigPacket
		for i, typ := range types {
			out = append(out, sigPacket{n: i + 1, sigType: typ})
		}
		return out
	}
	for _, tc := range []struct {
		types []byte
		want  int
	}{
		{[]byte{0x01}, 1},
		{[]byte{0x00, 0x00}, 2},
		{[]byte{0x01, 0x01, 0x01}, 3},
		{[]byte{0x00, 0x01}, 1},
		{[]byte{0x01, 0x00}, 1},
		{[]byte{0x01, 0x01, 0x00}, 1},
	} {
		if got := gpgvChecks(p(tc.types...)); len(got) != tc.want || got[0].n != 1 {
			t.Errorf("types % x: gpgv checks %d packets from packet %d; want %d from packet 1", tc.types, len(got), got[0].n, tc.want)
		}
	}
}
