package freshness

// Exported for the external tests (package freshness_test), which import
// internal/gpg: an internal test cannot, as internal/gpg imports this
// package.

// InReleaseMessage is reconcile's reading of a stored InRelease
// (inReleaseMessage).
var InReleaseMessage = inReleaseMessage
