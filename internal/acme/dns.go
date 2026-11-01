package acme

import (
	"context"
	"errors"
	"time"

	xacme "golang.org/x/crypto/acme"
)

// DNSProvider retains provider-specific state between Present and CleanUp.
// Factory creates a fresh instance per attempt, before account/order activity.
type DNSProvider interface {
	Present(context.Context, string, string, string) error
	Wait(context.Context, string, string, string) error
	CleanUp(context.Context, string, string, string) error
	Close()
}

type dnsFailure string

func (e dnsFailure) Error() string          { return "acme: " + string(e) }
func (e dnsFailure) DNSFailureCode() string { return string(e) }

func (m *Manager) solveAuthorization(ctx context.Context, client *xacme.Client, auth *xacme.Authorization, provider DNSProvider) (err error) {
	kind := "http-01"
	if provider != nil {
		kind = "dns-01"
	}
	var selected *xacme.Challenge
	for _, challenge := range auth.Challenges {
		if challenge.Type == kind {
			selected = challenge
			break
		}
	}
	if selected == nil {
		return errors.New("selected ACME challenge unavailable")
	}
	// HTTP01ChallengeResponse computes the RFC 8555 key authorization. Lego's
	// DNS providers hash that authorization to obtain the DNS-01 TXT value.
	keyAuth, err := client.HTTP01ChallengeResponse(selected.Token)
	if err != nil {
		return err
	}
	if provider == nil {
		if err := m.cfg.Challenge.set(selected.Token, keyAuth); err != nil {
			return err
		}
		defer m.cfg.Challenge.clear()
	} else {
		// Clean up even a failed Present: the provider's API might have created
		// the record before the response failed. Cancellation needs a fresh budget.
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if cleanupErr := provider.CleanUp(cleanupCtx, m.cfg.DNSName, selected.Token, keyAuth); cleanupErr != nil {
				err = dnsFailure("dns_cleanup_failed")
			}
		}()
		if err := provider.Present(ctx, m.cfg.DNSName, selected.Token, keyAuth); err != nil {
			return err
		}
		if err := provider.Wait(ctx, m.cfg.DNSName, selected.Token, keyAuth); err != nil {
			return err
		}
	}
	if _, err := client.Accept(ctx, selected); err != nil {
		return err
	}
	_, err = client.WaitAuthorization(ctx, auth.URI)
	return err
}
