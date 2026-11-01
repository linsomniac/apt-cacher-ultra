package gpg

import "github.com/linsomniac/apt-cacher-ultra/internal/apttext"

// Layer T of the hybrid verifier, apt's text layer ported, lives in
// internal/apttext, which internal/freshness's reconcile reads stored
// anchors with too (the re-review's V-RI1: both read the same M). Layers A
// and P refuse with its error type, so that every structural refusal reads
// "<layer> <rule>: detail" and tests read its rule alike.

// shapeError is a refusal by one of DN-11's structural rules: T1–T5, F0
// and T4/one-block (internal/apttext), A1–A6 (armor_canon.go) and P1–P5
// (sigpacket.go).
type shapeError = apttext.ShapeError

// refuse is a shapeError for rule, its detail formatted.
func refuse(rule, format string, args ...any) error {
	return apttext.Refuse(rule, format, args...)
}
