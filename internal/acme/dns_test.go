package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/acmedns"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
	xacme "golang.org/x/crypto/acme"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == acmedns.WorkerArgument {
		os.Exit(acmedns.RunWorker())
	}
	os.Exit(m.Run())
}

type failingDNS struct {
	presentErr, waitErr, cleanupErr error
	cleaned                         bool
	cleanupCancelled                bool
}

func (p *failingDNS) Present(context.Context, string, string, string) error { return p.presentErr }
func (p *failingDNS) Wait(context.Context, string, string, string) error    { return p.waitErr }
func (p *failingDNS) CleanUp(ctx context.Context, _, _, _ string) error {
	p.cleaned = true
	p.cleanupCancelled = ctx.Err() != nil
	return p.cleanupErr
}
func (p *failingDNS) Close() {}

func TestDNSCleanupAfterPartialFailureAndCancellation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &xacme.Client{Key: key}
	auth := &xacme.Authorization{Challenges: []*xacme.Challenge{{Type: "dns-01", Token: "abcdefghijklmnop0123456789"}}}
	for _, tt := range []struct {
		name     string
		provider failingDNS
		want     string
	}{
		{"partial present", failingDNS{presentErr: acmedns.Failure("dns_present_failed")}, "dns_present_failed"},
		{"cancelled propagation", failingDNS{waitErr: context.Canceled}, "cancelled"},
		{"cleanup failure", failingDNS{presentErr: errors.New("SECRET-API-DETAIL"), cleanupErr: errors.New("SECRET-KEY")}, "dns_cleanup_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{cfg: Config{DNSName: testName}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := m.solveAuthorization(ctx, client, auth, &tt.provider)
			if err == nil || classify(err) != tt.want {
				t.Fatalf("got %v category %s", err, classify(err))
			}
			if !tt.provider.cleaned || tt.provider.cleanupCancelled {
				t.Fatal("cleanup did not receive a fresh context")
			}
		})
	}
}

func TestDNSConfigurationFailureBeforeAuthorityActivity(t *testing.T) {
	srv := fakeacme.New(t)
	m, _ := testManager(t, srv)
	m.cfg.Challenge = nil
	m.cfg.DNSFactory = func(context.Context) (DNSProvider, error) {
		var reserved retryRecord
		if err := m.store.read(m.store.retryName, &reserved); err != nil {
			t.Fatal(err)
		}
		if reserved.Next.Before(time.Now().Add(time.Hour)) {
			t.Fatal("DNS crash reservation missing before provider initialization")
		}
		return nil, acmedns.Failure("dns_provider_configuration_failed")
	}
	m.cfg.DNSProviderName = "cloudflare"
	if err := m.EnsureCertificate(context.Background()); err == nil {
		t.Fatal("bad DNS configuration accepted")
	}
	if m.retry.Next.Before(time.Now().Add(50*time.Second)) || m.retry.Next.After(time.Now().Add(2*time.Minute)) {
		t.Fatal("completed failure retained crash reservation or lost backoff")
	}
	if srv.AccountCount() != 0 || srv.IssueCount() != 0 {
		t.Fatal("CA activity before DNS provider initialization")
	}
	if m.Status().ChallengeType != "dns-01" || m.Status().DNSProvider != "cloudflare" || m.Snapshot().Error != "dns_provider_configuration_failed" {
		t.Fatalf("status %+v", m.Status())
	}
}
