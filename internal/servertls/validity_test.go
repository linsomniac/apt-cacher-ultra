package servertls

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

func TestIssuerExpiryLimitsAvailability(t *testing.T) {
	leaf := certfixture.New(t)
	issuer := certfixture.New(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(time.Hour) })
	// The holder receives an already verified chain; this fixture isolates its
	// time gate from chain signature validation (tested by Parse).
	cert := &tls.Certificate{Certificate: [][]byte{leaf.Leaf.Raw, issuer.Leaf.Raw}, Leaf: leaf.Leaf}
	var holder Holder
	holder.Install(cert)
	now := time.Now()
	if holder.Available(now) != nil {
		t.Fatal("valid chain unavailable")
	}
	if holder.Available(now.Add(2*time.Hour)) == nil {
		t.Fatal("expired issuer accepted with valid leaf")
	}
	snapshot := holder.Snapshot("files", "", now.Add(2*time.Hour))
	if snapshot.State != "expired" || snapshot.NotAfter != issuer.Leaf.NotAfter.Unix() {
		t.Fatalf("status %+v", snapshot)
	}
}
