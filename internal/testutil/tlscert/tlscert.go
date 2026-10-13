// Package testutil provides helpers shared by tests across packages.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WriteSelfSignedCert generates an ECDSA P-256 self-signed server
// certificate valid for 127.0.0.1 / ::1 / localhost and writes
// cert.pem and key.pem into dir, returning their paths. ECDSA keeps
// test key generation fast (RSA-2048 keygen costs ~100ms+ per call);
// the admin listener accepts any TLS-valid pair, so the cheaper
// curve is fine. The certificate is its own CA (self-signed), so
// tests can trust it by adding the cert PEM to a x509.CertPool.
func WriteSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "apt-cacher-ultra test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("x509.MarshalECPrivateKey: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create %s: %v", certPath, err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		_ = certOut.Close()
		t.Fatalf("pem.Encode cert: %v", err)
	}
	if err := certOut.Close(); err != nil {
		t.Fatalf("close %s: %v", certPath, err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create %s: %v", keyPath, err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		_ = keyOut.Close()
		t.Fatalf("pem.Encode key: %v", err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatalf("close %s: %v", keyPath, err)
	}
	return certPath, keyPath
}

// CertPoolFromFile returns a x509.CertPool trusting the PEM
// certificate at path. Lets a test client verify the self-signed
// test certificate without InsecureSkipVerify.
func CertPoolFromFile(t *testing.T, certPath string) *x509.CertPool {
	t.Helper()
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read %s: %v", certPath, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("no PEM block in %s", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate %s: %v", certPath, err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}
