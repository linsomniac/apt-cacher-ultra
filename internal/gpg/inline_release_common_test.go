package gpg

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
)

func TestInlineRelease_SignedTextBoundaries(t *testing.T) {
	signer := newTestEntity(t, "Release compatibility", "fixture@example.invalid")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"no final LF": "Origin: fixture\nSuite: testing",
		"final LF":    "Origin: fixture\nSuite: testing\n",
		"blank line":  "Origin: fixture\nSuite: testing\n\n",
		"CRLF":        "Origin: fixture\r\nSuite: testing\r\n",
		"whitespace":  "Origin: fixture \t\nSuite: testing\t\n",
		"dash escape": "Origin: fixture\n- dash-escaped signed line\nSuite: testing\n",
	} {
		t.Run(name, func(t *testing.T) {
			inline := clearsignWith(t, signer, []byte(text))
			if _, err := v.VerifyInline(context.Background(), newSuite(), inline); err != nil {
				t.Fatalf("fixture must verify inline: %v", err)
			}
			release, signature, err := apttext.DetachedFromInline(inline)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.VerifyDetached(context.Background(), newSuite(), release, signature); err != nil {
				t.Fatalf("converted signature must verify: %v", err)
			}
			tampered := append([]byte("Origin: substituted\n"), release...)
			if _, err := v.VerifyDetached(context.Background(), newSuite(), tampered, signature); err == nil {
				t.Fatal("converted signature accepted substituted metadata")
			}
		})
	}
}
