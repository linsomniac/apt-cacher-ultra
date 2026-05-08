package tlsmitm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// newTestCA builds a self-signed CA suitable for unit-testing the
// leaf-generation path. The key argument selects the CA key shape so
// each test can verify SigningAlgFor's closed enum directly.
func newTestCA(t *testing.T, keyKind string) *tls.Certificate {
	t.Helper()
	var (
		caKey crypto.Signer
		pub   crypto.PublicKey
	)
	switch keyKind {
	case "ecdsa-p256":
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("ecdsa-p256 keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "ecdsa-p384":
		k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		if err != nil {
			t.Fatalf("ecdsa-p384 keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "ecdsa-p521":
		k, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		if err != nil {
			t.Fatalf("ecdsa-p521 keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "rsa-2048":
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("rsa-2048 keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "rsa-1024":
		k, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatalf("rsa-1024 keygen: %v", err)
		}
		caKey, pub = k, &k.PublicKey
	case "ed25519":
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("ed25519 keygen: %v", err)
		}
		caKey = k
		pub = k.Public()
	default:
		t.Fatalf("unknown keyKind %q", keyKind)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-CA"},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(1 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  caKey,
		Leaf:        parsed,
	}
}

func TestParseLeafAlgorithm(t *testing.T) {
	good := []struct {
		s    string
		want LeafAlgorithm
	}{
		{"ecdsa-p256", LeafECDSAP256},
		{"rsa2048", LeafRSA2048},
	}
	for _, tc := range good {
		got, err := ParseLeafAlgorithm(tc.s)
		if err != nil {
			t.Errorf("ParseLeafAlgorithm(%q) error: %v", tc.s, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLeafAlgorithm(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
	bad := []string{"", "ECDSA-P256", "rsa-2048", "ed25519", "rsa4096"}
	for _, s := range bad {
		if _, err := ParseLeafAlgorithm(s); err == nil {
			t.Errorf("ParseLeafAlgorithm(%q) should reject", s)
		}
	}
}

func TestSigningAlgFor_AcceptedKeys(t *testing.T) {
	cases := []struct {
		name string
		want x509.SignatureAlgorithm
	}{
		{"ecdsa-p256", x509.ECDSAWithSHA256},
		{"ecdsa-p384", x509.ECDSAWithSHA384},
		{"rsa-2048", x509.SHA256WithRSA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := newTestCA(t, tc.name)
			got, err := SigningAlgFor(ca.PrivateKey)
			if err != nil {
				t.Fatalf("SigningAlgFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("SigningAlgFor: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSigningAlgFor_RejectedKeys(t *testing.T) {
	cases := []string{"ecdsa-p521", "rsa-1024", "ed25519"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			ca := newTestCA(t, kind)
			_, err := SigningAlgFor(ca.PrivateKey)
			if err == nil {
				t.Fatalf("expected ErrUnsupportedCAKey for %s, got nil", kind)
			}
			if !errors.Is(err, ErrUnsupportedCAKey) {
				t.Fatalf("expected wrap of ErrUnsupportedCAKey, got %v", err)
			}
		})
	}
}

func TestGenerateLeaf_ECDSA_RoundTrip(t *testing.T) {
	ca := newTestCA(t, "ecdsa-p256")
	now := time.Now()
	leaf, err := GenerateLeaf("apt.example.com", ca, LeafECDSAP256, 30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("GenerateLeaf: %v", err)
	}

	if leaf.Leaf == nil {
		t.Fatal("Leaf is nil")
	}
	if leaf.Leaf.Subject.CommonName != "apt.example.com" {
		t.Errorf("CN: got %q, want apt.example.com", leaf.Leaf.Subject.CommonName)
	}
	if got := leaf.Leaf.DNSNames; len(got) != 1 || got[0] != "apt.example.com" {
		t.Errorf("DNSNames: got %v, want [apt.example.com]", got)
	}
	if leaf.Leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("sig alg: got %v, want ECDSAWithSHA256", leaf.Leaf.SignatureAlgorithm)
	}
	if leaf.Leaf.IsCA {
		t.Error("leaf must not have IsCA=true")
	}
	if !leaf.Leaf.NotBefore.Before(now.Add(-time.Minute)) {
		t.Errorf("NotBefore not in past: %s vs now %s", leaf.Leaf.NotBefore, now)
	}
	if leaf.Leaf.NotAfter.Sub(now) < 29*24*time.Hour {
		t.Errorf("NotAfter too short: %s", leaf.Leaf.NotAfter)
	}
	// Cert chain on the wire: leaf DER then CA DER.
	if len(leaf.Certificate) != 2 {
		t.Fatalf("chain length: got %d, want 2", len(leaf.Certificate))
	}

	// Cryptographic verification: the leaf signature must verify against
	// the CA's public key.
	caPool := x509.NewCertPool()
	caPool.AddCert(ca.Leaf)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:       caPool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not verify against CA: %v", err)
	}
}

func TestGenerateLeaf_RSA_SignedByECDSA(t *testing.T) {
	// A RSA-2048 leaf signed by an ECDSA P-256 CA: §5.1.3 says the
	// signing algorithm is derived from the CA, NOT the leaf — so the
	// leaf still gets ECDSAWithSHA256 even though its OWN key is RSA.
	ca := newTestCA(t, "ecdsa-p256")
	leaf, err := GenerateLeaf("rsa.example.com", ca, LeafRSA2048, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("GenerateLeaf: %v", err)
	}
	if leaf.Leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("sig alg should follow CA, got %v want ECDSAWithSHA256", leaf.Leaf.SignatureAlgorithm)
	}
	if _, ok := leaf.PrivateKey.(*rsa.PrivateKey); !ok {
		t.Errorf("leaf private key: got %T, want *rsa.PrivateKey", leaf.PrivateKey)
	}
}

func TestGenerateLeaf_RejectsUnsupportedCAKey(t *testing.T) {
	ca := newTestCA(t, "ed25519")
	_, err := GenerateLeaf("foo.example.com", ca, LeafECDSAP256, time.Hour, time.Now())
	if err == nil {
		t.Fatal("expected error for Ed25519 CA, got nil")
	}
	if !errors.Is(err, ErrUnsupportedCAKey) {
		t.Errorf("expected wrap of ErrUnsupportedCAKey, got %v", err)
	}
}

func TestGenerateLeaf_NilCA(t *testing.T) {
	_, err := GenerateLeaf("foo.example.com", nil, LeafECDSAP256, time.Hour, time.Now())
	if err == nil {
		t.Fatal("expected error for nil CA, got nil")
	}
}

func TestGenerateLeaf_EmptyHost(t *testing.T) {
	ca := newTestCA(t, "ecdsa-p256")
	_, err := GenerateLeaf("", ca, LeafECDSAP256, time.Hour, time.Now())
	if err == nil {
		t.Fatal("expected error for empty host, got nil")
	}
}
