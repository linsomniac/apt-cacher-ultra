package main

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/acmedns"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
)

func TestDNSModeDoesNotBindHTTPOrNeedCredentialsForSavedCertificate(t *testing.T) {
	providers := acmedns.Providers()
	if len(providers) == 0 {
		t.Skip("no providers compiled")
	}
	ca := fakeacme.New(t)
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	cfg := minimalCfg(t.TempDir(), nil)
	cfg.Admin.Enabled = true
	cfg.Admin.UseACME = true
	cfg.ACME = config.ACMEConfig{Enabled: true, DirectoryURL: ca.DirectoryURL(), DNSName: "cache.example.net", Listen: address, AcceptTerms: true}
	ca.SetChallengeBaseURL("http://" + address)
	original, err := prepareServerTLS(context.Background(), cfg, slog.New(slog.DiscardHandler), ca.Transport())
	if err != nil {
		t.Fatal(err)
	}
	certificate := original.manager.Current().Leaf
	original.Close()
	occupied, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	cfg.ACME.ChallengeType = "dns-01"
	cfg.ACME.DNSProvider = providers[0]
	// No DNS credentials are configured: saved material must start offline.
	next, err := prepareServerTLS(context.Background(), cfg, slog.New(slog.DiscardHandler), ca.Transport())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.challenge != nil || !next.manager.Current().Leaf.Equal(certificate) || ca.IssueCount() != 1 {
		t.Fatal("DNS mode bound HTTP or reissued certificate")
	}
}
