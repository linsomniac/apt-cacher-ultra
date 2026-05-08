// Package tlsmitm implements the Phase 6 CA + leaf cert primitives that
// the CONNECT-method MITM handler depends on. It is intentionally
// self-contained: it has no dependencies on internal/handler,
// internal/fetch, or any other subsystem so that the generation,
// caching, and CA-on-disk semantics can be unit-tested in isolation.
//
// SPEC6 §5.1.1 / §5.1.3 / §9.1 are the binding contracts.
package tlsmitm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// LeafAlgorithm is the closed enum SPEC6 §5.1.3 defines for the leaf
// cert key algorithm. Phase 6 supports two values: ECDSA P-256 (default)
// and RSA-2048 (for pre-2018 client compatibility).
type LeafAlgorithm int

const (
	LeafECDSAP256 LeafAlgorithm = iota
	LeafRSA2048
)

// String renders the algorithm as the §10.2 mitm_cert_issued.algorithm
// log field and §10.3 acu_mitm_cert_issued_total{algorithm=…} label.
func (a LeafAlgorithm) String() string {
	switch a {
	case LeafECDSAP256:
		return "ecdsa-p256"
	case LeafRSA2048:
		return "rsa2048"
	default:
		return "unknown"
	}
}

// ParseLeafAlgorithm maps the config string form to the enum, rejecting
// any value not in the §5.1.3 closed list. Used at config validation.
func ParseLeafAlgorithm(s string) (LeafAlgorithm, error) {
	switch s {
	case "ecdsa-p256":
		return LeafECDSAP256, nil
	case "rsa2048":
		return LeafRSA2048, nil
	default:
		return 0, fmt.Errorf("tlsmitm: unsupported leaf_algorithm %q (want \"ecdsa-p256\" or \"rsa2048\")", s)
	}
}

// ErrUnsupportedCAKey is returned by signing-algorithm derivation and
// CA validation when the CA's private key is not in the §5.1.3 closed
// enum (ECDSA P-256/P-384, RSA 2048/3072/4096). All other types —
// Ed25519, ECDSA P-521, RSA <2048, DSA — are rejected.
var ErrUnsupportedCAKey = errors.New("tlsmitm: unsupported CA key type")

// SigningAlgFor returns the x509.SignatureAlgorithm Phase 6 uses to
// sign a leaf with the given CA private key. Closed enum per §5.1.3.
//
// Exported so the config-validation path (§5.2) can reject an
// operator-supplied CA key whose type is outside the enum BEFORE the
// daemon binds — failing at config validation rather than at first
// CONNECT.
func SigningAlgFor(caKey crypto.PrivateKey) (x509.SignatureAlgorithm, error) {
	switch k := caKey.(type) {
	case *ecdsa.PrivateKey:
		switch k.Curve {
		case elliptic.P256():
			return x509.ECDSAWithSHA256, nil
		case elliptic.P384():
			return x509.ECDSAWithSHA384, nil
		default:
			return 0, fmt.Errorf("%w: ECDSA curve %s", ErrUnsupportedCAKey, k.Curve.Params().Name)
		}
	case *rsa.PrivateKey:
		bits := k.N.BitLen()
		if bits != 2048 && bits != 3072 && bits != 4096 {
			return 0, fmt.Errorf("%w: RSA-%d (need 2048, 3072, or 4096)", ErrUnsupportedCAKey, bits)
		}
		return x509.SHA256WithRSA, nil
	default:
		return 0, fmt.Errorf("%w: %T", ErrUnsupportedCAKey, caKey)
	}
}

// GenerateLeaf issues a leaf cert for `host` (the lower-cased,
// IDNA-normalized literal CONNECT host) signed by `ca`. SPEC6 §5.1.3.
//
// The returned *tls.Certificate is ready to drop into a tls.Config's
// Certificates list or GetCertificate callback. Certificate[0] is the
// leaf DER, Certificate[1] is the CA DER (so apt clients receive the
// chain on the wire).
//
// `alg` governs the leaf's own key pair. The signing algorithm is
// derived from `ca.PrivateKey`'s type, NOT from `alg` — see SigningAlgFor.
func GenerateLeaf(host string, ca *tls.Certificate, alg LeafAlgorithm, lifetime time.Duration, now time.Time) (*tls.Certificate, error) {
	if ca == nil || ca.Leaf == nil || ca.PrivateKey == nil {
		return nil, errors.New("tlsmitm: GenerateLeaf: nil CA or missing Leaf/PrivateKey")
	}
	if host == "" {
		return nil, errors.New("tlsmitm: GenerateLeaf: empty host")
	}
	sigAlg, err := SigningAlgFor(ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	leafKey, leafPub, err := generateLeafKey(alg)
	if err != nil {
		return nil, err
	}
	// AIDEV-NOTE: 128-bit serial. RFC 5280 mandates positive serials, so
	// rand.Int with an upper bound of 2^128 (and 0 excluded by retry) is
	// sufficient. The probability of collision over a 256-entry leaf cache
	// is astronomically small — ~2^-100.
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, fmt.Errorf("tlsmitm: GenerateLeaf: serial: %w", err)
	}
	if serial.Sign() == 0 {
		serial = big.NewInt(1)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              []string{host},
		SignatureAlgorithm:    sigAlg,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Leaf, leafPub, ca.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("tlsmitm: GenerateLeaf: %w", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsmitm: GenerateLeaf: parse: %w", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, ca.Leaf.Raw},
		PrivateKey:  leafKey,
		Leaf:        parsed,
	}, nil
}

func generateLeafKey(alg LeafAlgorithm) (crypto.Signer, crypto.PublicKey, error) {
	switch alg {
	case LeafECDSAP256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("tlsmitm: ecdsa generate: %w", err)
		}
		return k, &k.PublicKey, nil
	case LeafRSA2048:
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, fmt.Errorf("tlsmitm: rsa generate: %w", err)
		}
		return k, &k.PublicKey, nil
	default:
		return nil, nil, fmt.Errorf("tlsmitm: unsupported leaf algorithm %v", alg)
	}
}
