package fetch

import (
	"fmt"
	"regexp"
)

// compileAllow turns the user-supplied regex patterns into compiled
// *regexp.Regexp values. SPEC §6.6: an empty list means "deny everything"
// — that semantic is preserved (the returned slice is empty).
func compileAllow(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("fetch: allowed_host_regex[%d] %q: %w", i, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// checkAllowed reports whether host (the canonical hostname with no port)
// matches any allowlist regex. Empty allowlist denies everything.
func (c *Client) checkAllowed(host string) error {
	for _, re := range c.allow {
		if re.MatchString(host) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
}

// HostAllowed reports whether host (the canonical hostname with no port)
// is permitted by the allowlist. Exported so the handler layer can reject
// disallowed hosts before allocating per-host bookkeeping (singleflight
// entries, semaphore slots) — without this pre-check, an unauthenticated
// client could grow handler-side maps indefinitely by sending requests
// for many distinct disallowed hostnames.
//
// The empty-allowlist semantic from SPEC §6.6 (deny everything) is
// preserved.
func (c *Client) HostAllowed(host string) bool {
	return c.checkAllowed(host) == nil
}
