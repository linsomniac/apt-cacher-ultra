package tlsmitm

import (
	"errors"
	"fmt"
	"regexp/syntax"
	"strings"
)

// ErrNameConstraintsUnsupported is returned by TranslateRegex when the
// regex shape cannot be safely translated into RFC 5280 dNSName Name
// Constraints. Wrap-checked with errors.Is.
//
// "Safely" here means that the constraint set's preimage is a strict
// SUPERSET of the regex's preimage — every literal hostname the regex
// admits is permitted by the constraint. The translator's contract is
// to produce coarser-or-equal constraints, never narrower-than-regex
// (which would silently reject legitimate hosts).
var ErrNameConstraintsUnsupported = errors.New("tlsmitm: regex shape not supported by NameConstraints translator")

// TranslateRegex maps a `tls_mitm.allowed_host_regex` pattern (the
// signing-gate predicate, SPEC6 §5.1.2) into a list of dNSName
// permittedSubtrees suitable for an X.509 NameConstraints extension.
//
// SPEC6 §5.1.1.1 fixes the accepted RE2 fragment:
//  1. anchored literal hostname:                ^foo\.example\.com$
//  2. anchored single-label wildcard prefix:    ^[a-z0-9-]+\.foo\.com$ or ^[^.]+\.foo\.com$
//  3. anchored optional fixed-length region:    ^([a-z]{2}\.)?archive\.ubuntu\.com$
//  4. anchored alternation of literal hosts:    ^(foo\.com|bar\.com)$
//
// Anything else — including unanchored patterns, character classes
// admitting '.', alternation spanning multiple TLDs in non-trivial
// ways — yields ErrNameConstraintsUnsupported. The caller treats that
// the same way it treats an empty regex: fail closed unless
// `allow_unconstrained_ca = true`.
//
// Non-empty success is the only path that produces Name Constraints;
// every other return value (empty regex, parse failure, unsupported
// shape) means "no constraints can be derived".
//
// Over-approximation note. RFC 5280 dNSName matching is suffix-based:
// a permitted subtree of `foo.example.com` admits any subdomain such
// as `bar.foo.example.com`. The translator therefore produces a
// CONSTRAINT SET that is a SUPERSET of the regex's literal preimage —
// see SPEC6 §5.1.1.1 for the per-shape over-approximation table.
func TranslateRegex(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, fmt.Errorf("%w: empty regex", ErrNameConstraintsUnsupported)
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrNameConstraintsUnsupported, err)
	}
	re = re.Simplify()
	inner, err := stripAnchors(re)
	if err != nil {
		return nil, err
	}
	inner = unwrapCapture(inner)

	if inner.Op == syntax.OpAlternate {
		return translateAlternation(inner)
	}
	if host, ok := asLiteralHost(inner); ok {
		if err := validateHostname(host); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNameConstraintsUnsupported, err)
		}
		return []string{host}, nil
	}
	if host, err := translatePrefixSuffix(inner); err == nil {
		return []string{host}, nil
	}
	return nil, fmt.Errorf("%w: regex grammar contains constructs (quantifiers, character classes spanning labels, lookaround, etc.) the translator cannot prove a safe NameConstraints superset for", ErrNameConstraintsUnsupported)
}

// stripAnchors verifies the top-level Concat starts with BeginText and
// ends with EndText, returning the sub-expression between them. Without
// these anchors the regex matches arbitrary substrings of any hostname
// and cannot be safely bounded by NameConstraints.
func stripAnchors(re *syntax.Regexp) (*syntax.Regexp, error) {
	if re.Op != syntax.OpConcat || len(re.Sub) < 2 {
		return nil, fmt.Errorf("%w: pattern is not anchored with ^…$", ErrNameConstraintsUnsupported)
	}
	if re.Sub[0].Op != syntax.OpBeginText {
		return nil, fmt.Errorf("%w: missing leading ^ anchor", ErrNameConstraintsUnsupported)
	}
	if re.Sub[len(re.Sub)-1].Op != syntax.OpEndText {
		return nil, fmt.Errorf("%w: missing trailing $ anchor", ErrNameConstraintsUnsupported)
	}
	middle := re.Sub[1 : len(re.Sub)-1]
	if len(middle) == 0 {
		return nil, fmt.Errorf("%w: empty pattern between anchors", ErrNameConstraintsUnsupported)
	}
	if len(middle) == 1 {
		return middle[0], nil
	}
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: middle}, nil
}

// translateAlternation accepts shape 4 (and shape 5, the literal-list
// sugar) — every alternative must be a literal hostname.
func translateAlternation(re *syntax.Regexp) ([]string, error) {
	out := make([]string, 0, len(re.Sub))
	seen := make(map[string]struct{}, len(re.Sub))
	for _, sub := range re.Sub {
		sub = unwrapCapture(sub)
		host, ok := asLiteralHost(sub)
		if !ok {
			return nil, fmt.Errorf("%w: alternation alternative is not a literal hostname", ErrNameConstraintsUnsupported)
		}
		if err := validateHostname(host); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNameConstraintsUnsupported, err)
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: empty alternation", ErrNameConstraintsUnsupported)
	}
	return out, nil
}

// translatePrefixSuffix accepts shapes 2 and 3 ONLY in the exact
// syntactic forms SPEC6 §5.1.1.1 enumerates. Anything else — even
// shapes that are semantically a single-label prefix and would
// produce a sound over-approximation — is rejected. The fail-closed
// contract is "translate exactly what the spec lists or fall back to
// the unconstrained-CA refusal path"; broadening the accepted grammar
// would be a §5.1.1.1 spec change, not an implementation detail.
//
// Shape 2 — `[a-z0-9-]+\.suffix` or `[^.]+\.suffix`:
//
//	head  = OpPlus(CharClass) where the class is exactly LDH-no-dot
//	        OR exactly the negated-dot class
//	tail  = OpLiteral starting with '.'
//	result = tail without the leading '.'
//
// Shape 3 — `([a-z]{2}\.)?suffix` (two-letter region prefix):
//
//	head  = OpQuest(Concat(CharClass[a-z], CharClass[a-z], OpLiteral(".")))
//	tail  = OpLiteral NOT starting with '.'  (the '.' was donated by
//	         the optional prefix)
//	result = tail as-is
func translatePrefixSuffix(inner *syntax.Regexp) (string, error) {
	if inner.Op != syntax.OpConcat {
		return "", ErrNameConstraintsUnsupported
	}
	subs := flattenConcat(inner)
	if len(subs) < 2 {
		return "", ErrNameConstraintsUnsupported
	}
	head := unwrapCapture(subs[0])
	tailHost, ok := asLiteralHost(concatOf(subs[1:]))
	if !ok {
		return "", ErrNameConstraintsUnsupported
	}

	// Shape 2.
	if head.Op == syntax.OpPlus && len(head.Sub) == 1 {
		body := unwrapCapture(head.Sub[0])
		if body.Op == syntax.OpCharClass && isShape2CharClass(body.Rune) {
			if !strings.HasPrefix(tailHost, ".") {
				return "", ErrNameConstraintsUnsupported
			}
			host := tailHost[1:]
			if err := validateHostname(host); err != nil {
				return "", err
			}
			return host, nil
		}
	}

	// Shape 3.
	if head.Op == syntax.OpQuest && len(head.Sub) == 1 {
		body := unwrapCapture(head.Sub[0])
		if body.Op == syntax.OpConcat {
			bodySubs := flattenConcat(body)
			// Exactly two [a-z] char classes followed by a literal ".".
			if len(bodySubs) == 3 {
				c1 := unwrapCapture(bodySubs[0])
				c2 := unwrapCapture(bodySubs[1])
				last := unwrapCapture(bodySubs[2])
				if isAZCharClass(c1) && isAZCharClass(c2) &&
					last.Op == syntax.OpLiteral && string(last.Rune) == "." {
					if err := validateHostname(tailHost); err != nil {
						return "", err
					}
					return tailHost, nil
				}
			}
		}
	}
	return "", ErrNameConstraintsUnsupported
}

// isShape2CharClass reports whether rs is exactly one of the two
// shape-2 character classes SPEC6 §5.1.1.1 lists: `[a-z0-9-]` or
// `[^.]`. Other non-dot-admitting classes (e.g. `[a-z]`, `[A-Z]`,
// `[a-zA-Z0-9-]`) are rejected — even though they would be safe
// over-approximations — because the spec enumerates the accepted
// grammar exhaustively.
func isShape2CharClass(rs []rune) bool {
	// `[a-z0-9-]` → ['-', '-', '0', '9', 'a', 'z']
	ldh := []rune{'-', '-', '0', '9', 'a', 'z'}
	if runesEqual(rs, ldh) {
		return true
	}
	// `[^.]` → complement of '.': [0, '.'-1] ∪ ['.'+1, MaxRune]
	// Go's regexp/syntax encodes MaxRune as 0x10FFFF (unicode.MaxRune).
	const maxRune rune = 0x10FFFF
	notDot := []rune{0, '.' - 1, '.' + 1, maxRune}
	return runesEqual(rs, notDot)
}

// isAZCharClass reports whether re is exactly the `[a-z]` char class
// (Rune slice ['a', 'z']). Used to vet the body of a shape-3 prefix.
func isAZCharClass(re *syntax.Regexp) bool {
	if re.Op != syntax.OpCharClass {
		return false
	}
	expected := []rune{'a', 'z'}
	return runesEqual(re.Rune, expected)
}

func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// asLiteralHost returns the concatenation of every OpLiteral in the
// (possibly nested-Concat) subtree rooted at `re`. Returns ok=false if
// any leaf is not OpLiteral.
func asLiteralHost(re *syntax.Regexp) (string, bool) {
	re = unwrapCapture(re)
	if re.Op == syntax.OpLiteral {
		return string(re.Rune), true
	}
	if re.Op == syntax.OpConcat {
		var sb strings.Builder
		for _, sub := range flattenConcat(re) {
			sub = unwrapCapture(sub)
			switch sub.Op {
			case syntax.OpLiteral:
				sb.WriteString(string(sub.Rune))
			case syntax.OpEmptyMatch:
				// Go's regex simplifier rewrites duplicate alternation
				// alternatives into Concat(Literal, OpEmptyMatch). The
				// empty match contributes nothing to the literal value.
			default:
				return "", false
			}
		}
		return sb.String(), true
	}
	return "", false
}

// flattenConcat returns the sub-nodes of a (possibly nested) OpConcat
// as one flat slice. Leaves non-Concat input untouched.
func flattenConcat(re *syntax.Regexp) []*syntax.Regexp {
	if re.Op != syntax.OpConcat {
		return []*syntax.Regexp{re}
	}
	out := make([]*syntax.Regexp, 0, len(re.Sub))
	for _, sub := range re.Sub {
		if sub.Op == syntax.OpConcat {
			out = append(out, flattenConcat(sub)...)
		} else {
			out = append(out, sub)
		}
	}
	return out
}

// concatOf builds a synthetic OpConcat from a slice. Single-element
// slices are returned bare so asLiteralHost sees the expected shape.
func concatOf(subs []*syntax.Regexp) *syntax.Regexp {
	if len(subs) == 1 {
		return subs[0]
	}
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: subs}
}

// unwrapCapture peels OpCapture layers off a regex node. Captures are
// just grouping syntax in our context — the index does not affect
// constraint translation.
func unwrapCapture(re *syntax.Regexp) *syntax.Regexp {
	for re.Op == syntax.OpCapture && len(re.Sub) == 1 {
		re = re.Sub[0]
	}
	return re
}

// validateHostname checks that s is a syntactically-valid DNS host
// suitable as a dNSName Name Constraint subtree. It does NOT call
// IDNA — the constraint string MUST be ASCII LDH, which IDNA-normalized
// hostnames already are.
func validateHostname(s string) error {
	if s == "" {
		return errors.New("empty hostname")
	}
	if len(s) > 253 {
		return fmt.Errorf("hostname too long: %d > 253", len(s))
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return errors.New("hostname must not begin or end with '.'")
	}
	if strings.Contains(s, "..") {
		return errors.New("hostname must not contain consecutive '.'")
	}
	for _, label := range strings.Split(s, ".") {
		if err := validateLabel(label); err != nil {
			return fmt.Errorf("label %q: %w", label, err)
		}
	}
	return nil
}

// validateLabel enforces RFC 1035 / 5891 LDH label syntax.
func validateLabel(label string) error {
	if label == "" {
		return errors.New("empty label")
	}
	if len(label) > 63 {
		return fmt.Errorf("label too long: %d > 63", len(label))
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errors.New("label must not begin or end with '-'")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("illegal character %q", r)
		}
	}
	return nil
}
