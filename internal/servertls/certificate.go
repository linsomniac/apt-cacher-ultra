// Package servertls validates and publishes server certificates. It is separate
// from the proxy's MITM CA: these certificates identify the daemon itself.
package servertls

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const MaxPEMBytes = 1 << 20

// Parse validates the entire candidate before it can replace a working pair.
// dnsName, when nonempty, restricts managed certificates to exactly that name.
// Trust in public roots is deliberately not required: private PKI and ACME
// staging certificates are supported. Clients still verify their own trust.
func Parse(certPEM, keyPEM []byte, dnsName string, now time.Time) (*tls.Certificate, error) {
	if len(certPEM) > MaxPEMBytes || len(keyPEM) > MaxPEMBytes {
		return nil, errors.New("server TLS: certificate or key exceeds size limit")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, errors.New("server TLS: invalid or mismatched certificate/key")
	}
	if len(cert.Certificate) == 0 || len(cert.Certificate) > 10 {
		return nil, errors.New("server TLS: invalid chain length")
	}
	chain := make([]*x509.Certificate, len(cert.Certificate))
	for i, der := range cert.Certificate {
		chain[i], err = x509.ParseCertificate(der)
		if err != nil {
			return nil, errors.New("server TLS: invalid certificate in chain")
		}
		if now.Before(chain[i].NotBefore) || !now.Before(chain[i].NotAfter) {
			return nil, errors.New("server TLS: certificate is not currently valid")
		}
		if i > 0 {
			if err := chain[i-1].CheckSignatureFrom(chain[i]); err != nil {
				return nil, errors.New("server TLS: invalid certificate chain link")
			}
		}
	}
	leaf := chain[0]
	if leaf.IsCA || (leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0) {
		return nil, errors.New("server TLS: certificate is not a server signing leaf")
	}
	if dnsName != "" && (len(leaf.DNSNames) != 1 || !strings.EqualFold(leaf.DNSNames[0], dnsName) ||
		len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 0 || len(leaf.EmailAddresses) != 0) {
		return nil, errors.New("server TLS: certificate identifiers differ from requested name")
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(chain[len(chain)-1])
	for i := 1; i < len(chain)-1; i++ {
		intermediates.AddCert(chain[i])
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates,
		CurrentTime: now, DNSName: dnsName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, errors.New("server TLS: invalid server usage or chain constraints")
	}
	cert.Leaf = leaf
	return &cert, nil
}

// ReadFile permits operator-managed symlinks, but not devices/FIFOs or
// unbounded material. O_NONBLOCK prevents opening a FIFO from hanging startup.
func ReadFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("server TLS: open file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxPEMBytes {
		return nil, errors.New("server TLS: expected a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxPEMBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPEMBytes {
		return nil, errors.New("server TLS: file exceeds size limit")
	}
	return b, nil
}

// Holder publishes immutable, prevalidated certificates to concurrent readers.
// A caller must not modify a certificate after installing it.
type Holder struct {
	cert atomic.Pointer[selection]
}

type selection struct {
	cert                *tls.Certificate
	notBefore, notAfter time.Time
}

// Validity returns the intersection of the served chain's validity periods.
// It is used at publication and renewal scheduling, never during a handshake.
func Validity(cert *tls.Certificate) (time.Time, time.Time) {
	if cert == nil || cert.Leaf == nil {
		return time.Time{}, time.Time{}
	}
	before, after := cert.Leaf.NotBefore, cert.Leaf.NotAfter
	for i, der := range cert.Certificate {
		if i == 0 {
			continue
		}
		issuer, err := x509.ParseCertificate(der)
		if err != nil {
			return time.Time{}, time.Time{}
		}
		if issuer.NotBefore.After(before) {
			before = issuer.NotBefore
		}
		if issuer.NotAfter.Before(after) {
			after = issuer.NotAfter
		}
	}
	return before, after
}

func (h *Holder) Install(cert *tls.Certificate) {
	before, after := Validity(cert)
	if before.IsZero() || after.IsZero() {
		h.cert.Store(nil)
		return
	}
	h.cert.Store(&selection{cert: cert, notBefore: before, notAfter: after})
}
func (h *Holder) Current() *tls.Certificate {
	if c := h.cert.Load(); c != nil {
		return c.cert
	}
	return nil
}

func (h *Holder) Available(now time.Time) error {
	c := h.cert.Load()
	if c == nil || now.Before(c.notBefore) || !now.Before(c.notAfter) {
		return errors.New("server TLS: no currently valid certificate")
	}
	return nil
}

// TLSConfig's ClientHello gate also runs for session resumptions, which may
// bypass GetCertificate. Neither callback does disk or network I/O.
func (h *Holder) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return nil, h.Available(time.Now())
		},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if err := h.Available(time.Now()); err != nil {
				return nil, err
			}
			return h.Current(), nil
		},
	}
}

type Snapshot struct {
	Source      string `json:"source"`
	State       string `json:"state"`
	Fingerprint string `json:"fingerprint_sha256,omitempty"`
	NotBefore   int64  `json:"not_before_unixtime,omitempty"`
	NotAfter    int64  `json:"not_after_unixtime,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (h *Holder) Snapshot(source, problem string, now time.Time) Snapshot {
	s := Snapshot{Source: source, State: "unavailable", Error: problem}
	if selected := h.cert.Load(); selected != nil {
		c := selected.cert
		sum := sha256.Sum256(c.Leaf.Raw)
		s.Fingerprint = hex.EncodeToString(sum[:])
		s.NotBefore, s.NotAfter = selected.notBefore.Unix(), selected.notAfter.Unix()
		if now.Before(selected.notBefore) {
			s.State = "not_yet_valid"
		} else if !now.Before(selected.notAfter) {
			s.State = "expired"
		} else if problem != "" {
			s.State = "degraded"
		} else {
			s.State = "usable"
		}
	}
	return s
}
