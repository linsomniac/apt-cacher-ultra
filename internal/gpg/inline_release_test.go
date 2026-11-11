package gpg

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
)

// Issue #30: splitting an adopted InRelease must preserve its message and
// every signature, including binary/text signatures and multiple signers.
// Check the accepted corpus with the actual verifier in both representations.
func TestInlineRelease_AcceptedCorpusRetainsSignatures(t *testing.T) {
	v := aptShapeVerifier(t)
	now := aptShapeMeasuredAt(t)
	v.now = func() time.Time { return now }
	accepted := 0
	for _, s := range loadAptShapes(t) {
		if !s.inline {
			continue
		}
		own, err := shapeVerifier(v, s)
		if err != nil {
			t.Fatal(err)
		}
		message, signers, err := own.VerifyInlineSigners(context.Background(), newSuite(), s.body)
		if err != nil {
			continue // conversion is only used on previously adopted anchors
		}
		accepted++
		release, signature, err := apttext.DetachedFromInline(s.body)
		if err != nil {
			t.Errorf("%s: conversion: %v", s.name, err)
			continue
		}
		got, detachedSigners, err := own.VerifyDetachedSigners(context.Background(), newSuite(), release, signature)
		if err != nil || !bytes.Equal(message, got) || !reflect.DeepEqual(signers, detachedSigners) {
			t.Errorf("%s: detached verification: %v; message equal=%v; signers=%+v, want %+v", s.name, err, bytes.Equal(message, got), detachedSigners, signers)
		}
	}
	if accepted < 100 {
		t.Fatalf("only %d accepted inline corpus fixtures exercised", accepted)
	}
	t.Logf("verified converted signatures for %d accepted inline corpus fixtures", accepted)
}

func TestInlineRelease_VendorSignatures(t *testing.T) {
	for _, va := range vendorAnchors {
		if !va.inline {
			continue
		}
		t.Run(va.dir, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(vendorDir, va.dir, "InRelease"))
			if err != nil {
				t.Fatal(err)
			}
			v := vendorVerifier(t, va.dir)
			original, err := v.VerifyInline(context.Background(), newSuite(), body)
			if err != nil {
				t.Fatal(err)
			}
			release, signature, err := apttext.DetachedFromInline(body)
			if err != nil {
				t.Fatal(err)
			}
			got, err := v.VerifyDetached(context.Background(), newSuite(), release, signature)
			if err != nil || !bytes.Equal(original, got) {
				t.Fatalf("derived pair: %v; message equal=%v", err, bytes.Equal(original, got))
			}
		})
	}
}
