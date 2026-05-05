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
