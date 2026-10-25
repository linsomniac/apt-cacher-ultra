package freshness

// Spec C4 "Response classification", defined once and used by every anchor
// probe: present, absent or unknown.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// probeClass is spec C4's response class of one anchor probe.
type probeClass int

const (
	// classUnknown: a transport error, any status other than those below
	// (408, 429, 5xx, other 4xx, an unfollowed 3xx), a body over its
	// limit, or the check deadline. No form decision; the check fails.
	classUnknown probeClass = iota
	// classPresent: a 200 whose body passes the file's sniff, or a 304 to
	// a request that carried validators (the cached anchor still stands).
	classPresent
	// classAbsent: 401, 403, 404, 410, or a 200 failing the sniff.
	classAbsent
)

func (p probeClass) String() string {
	switch p {
	case classPresent:
		return "present"
	case classAbsent:
		return "absent"
	}
	return "unknown"
}

// probeOutcome is one classified anchor probe.
type probeOutcome struct {
	class probeClass
	// res is the response of a present probe (status 200 or 304), and of a
	// 200 that failed its sniff; nil for an error.
	res *fetch.ConditionalResult
	// status is the upstream status (0 for a transport error, a deadline
	// or a cancelled caller).
	status int
	// err says what ended an unknown or error-status probe.
	err error
}

// cancelled reports a probe the caller's own context ended (shutdown):
// it says nothing about upstream, so the check writes nothing.
func (o probeOutcome) cancelled() bool { return errors.Is(o.err, context.Canceled) }

// authDenied reports a 401 or 403: on a signature file it stretches the
// unsigned confirmation gap to an hour (spec C5 safeguard 2).
func (o probeOutcome) authDenied() bool { return o.status == 401 || o.status == 403 }

// classify maps one fetch.Conditional result to its spec C4 class. sniff
// is the probed file's sniff; conditional reports whether the request
// carried validators — a 304 to a request without any is a protocol
// violation, unknown.
//
// AIDEV-NOTE: the one place a status becomes a class. 401/403/410 count as
// absent (more lenient than apt's 404-only signed fallback; for a signed
// form this can only cause a lateral move), so they never extend the
// check_fail_count streak (ruling R-E4). An unfollowed 3xx arrives as
// ErrRedirectBlocked wrapping a *fetch.StatusError with the 3xx code:
// unknown, like every code not listed.
func classify(res *fetch.ConditionalResult, err error, sniff func([]byte) bool, conditional bool) probeOutcome {
	if err != nil {
		var se *fetch.StatusError
		if !errors.As(err, &se) {
			return probeOutcome{class: classUnknown, err: err}
		}
		switch se.Code {
		case 401, 403, 404, 410:
			return probeOutcome{class: classAbsent, status: se.Code, err: err}
		}
		return probeOutcome{class: classUnknown, status: se.Code, err: err}
	}
	switch {
	case res.Status == 200 && sniff(res.Body):
		return probeOutcome{class: classPresent, res: res, status: 200}
	case res.Status == 200:
		return probeOutcome{class: classAbsent, res: res, status: 200}
	case res.Status == 304 && conditional:
		return probeOutcome{class: classPresent, res: res, status: 304}
	}
	return probeOutcome{class: classUnknown, status: res.Status, err: errUnexpectedStatus}
}

// anchorAbsentKey is the content key of an anchor_absent record: the
// absence has no content, and the constant can never equal a real content
// key, so whatever upstream serves next is new content. A never-adopted
// suite whose probe tree finds no anchor records it (ruling PE-2), and so
// does an adopted suite whose current anchor is absent (anchorAbsent).
var anchorAbsentKey = func() string {
	s := sha256.Sum256([]byte("anchor_absent"))
	return hex.EncodeToString(s[:])
}()

// Each anchor file's sniff (spec C3) for classify.
var (
	sniffInRelease  = aptpath.SniffInRelease
	sniffRelease    = aptpath.SniffRelease
	sniffReleaseGPG = aptpath.SniffReleaseGPG
)

// probe GETs the anchor file name ("InRelease", "Release", "Release.gpg")
// of suite without validators, under fetchCtx (the check deadline), and
// classifies the answer. The caller holds the host slot.
func (c *Checker) probe(fetchCtx context.Context, suite SuiteRef, urls anchorURLs, name string) probeOutcome {
	limit, sniff := c.maxBody, sniffInRelease
	switch name {
	case "Release":
		sniff = sniffRelease
	case "Release.gpg":
		limit, sniff = defaultMaxReleaseGPGBytes, sniffReleaseGPG
	}
	target := &fetch.Target{CanonicalHost: suite.CanonicalHost, URL: urls.of(name)}
	res, err := c.fetcher.Conditional(fetchCtx, target, "", "", limit)
	// false: a probe sends no validators, so a 304 is unknown (deferred
	// minor E-27).
	return classify(res, err, sniff, false)
}

// anchorURLs builds the upstream URLs of a suite's anchor files. A cached
// anchor row's stored upstream_url is the model — or, for a flat suite
// with no cached anchor row, its client-created index row's (checkFirst,
// ruling PG-20) — it keeps a non-default port or a mirror prefix that the
// canonical host and path do not carry — and a sibling is that URL with
// its last segment replaced; without one the checker's urlReconstruct
// builds the URL from the suite identity.
type anchorURLs struct {
	model       string // a cached anchor row's upstream_url (or a flat index row's: PG-20), or ""
	modelName   string // that row's file name
	reconstruct func(name string) string
}

func (u anchorURLs) of(name string) string {
	if u.model != "" {
		if prefix, ok := strings.CutSuffix(u.model, "/"+u.modelName); ok {
			return prefix + "/" + name
		}
	}
	return u.reconstruct(name)
}

// anchorURLsFor returns the URL builder for suite, modelled on the first
// of rows (InRelease, Release, Release.gpg order) that is cached.
func (c *Checker) anchorURLsFor(suite SuiteRef, rows map[string]string) anchorURLs {
	u := anchorURLs{reconstruct: func(name string) string {
		return c.urlReconstruct(suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath+"/"+name)
	}}
	for _, name := range []string{"InRelease", "Release", "Release.gpg"} {
		if url := rows[name]; url != "" {
			u.model, u.modelName = url, name
			break
		}
	}
	return u
}
