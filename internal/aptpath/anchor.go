package aptpath

// AnchorPaths returns the anchor file(s) of a snapshot of the given anchor
// form, relative to its suite (spec C5 "Anchor forms and storage"):
// "inline" has InRelease, "detached" Release and Release.gpg, "unsigned"
// Release alone. Any other form has none. form is cache.AnchorForm's
// value (aptpath imports nothing outside the standard library).
//
// AIDEV-NOTE: an unsigned snapshot stores its Release hash in the
// single-anchor slot (suite_snapshot.inrelease_hash), so no reader may
// derive an anchor file from the hash columns alone; go through the form.
func AnchorPaths(form string) []string {
	switch form {
	case "inline":
		return []string{"InRelease"}
	case "detached":
		return []string{"Release", "Release.gpg"}
	case "unsigned":
		return []string{"Release"}
	}
	return nil
}
