package apttext

import "bytes"

// DetachedFromInline exposes the exact message and signatures apt extracts
// from an InRelease as a Release/Release.gpg pair. No signature is created or
// removed. In particular, do not append a newline to Message: the newline
// before BEGIN PGP SIGNATURE is framing, not signed content.
//
// This is a representation conversion, not verification. Callers must obtain
// the input from their existing trust anchor; clients still verify the pair.
func DetachedFromInline(inRelease []byte) (release, signature []byte, err error) {
	cs, err := Inline(inRelease)
	if err != nil {
		return nil, nil, err
	}
	return cs.Message, append(bytes.Join(cs.Armor, []byte{'\n'}), '\n'), nil
}
