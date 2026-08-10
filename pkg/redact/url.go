// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package redact renders values that may carry credentials into forms that are safe to log.
//
// It exists because the standard library's nearest equivalent is not safe enough for this
// service's contract. `url.URL.Redacted()` masks the PASSWORD and preserves the username, on
// the reasonable general view that a username is an identifier rather than a secret. Every
// credential-bearing URL this service accepts — a JWKS endpoint behind a basic-auth gateway,
// a NATS URL with inline credentials — carries a username issued alongside the password as
// half of one credential, so leaking it narrows an attacker's search rather than telling them
// nothing. The contract here is therefore stricter: userinfo goes entirely, host and path
// stay, because the host and path are the whole diagnostic value of printing the URL at all.
//
// One implementation, in one place, deliberately. The bug that produced this package was two
// formatting sites in different packages disagreeing about what "redacted" meant.
package redact

import (
	"net/url"
	"strings"
)

// URLUserinfo removes any `user:pass@` from a URL STRING, keeping scheme, host and path.
//
// It is string-based rather than parse-based because its callers include the path where
// `url.Parse` itself FAILED: the parse error embeds the whole raw URL, so the raw string is
// exactly what must not be printed, and there is no *url.URL to work with. A string with no
// `@` is returned unchanged.
//
// A comma-separated list is redacted ENTRY BY ENTRY, but only when splitting is PROVABLY
// safe. NATS accepts a server list in one URL value, and treating such a value as a single
// URL is safe yet lossy: the last-`@` rule below takes everything before the final credential
// as userinfo and drops it, so a three-server list diagnoses as one server.
//
// The catch is that `,` is an RFC 3986 sub-delim and legal UNESCAPED inside userinfo, so a
// password may contain one. Split `nats://u:p,x@host` blindly and the first piece is
// `nats://u:p` — no `@`, nothing to redact, and half the password goes to the log. That is
// strictly worse than the lossy behaviour the split was meant to improve on.
//
// It is not decidable which a comma is: `nats://a:4222` is either a host and port or a user
// and password, and the value alone cannot say. So the split happens only when the answer
// does not matter — when EVERY segment carries an `@` (each is a credential-bearing server,
// so every one gets redacted) or NONE does (there is no credential anywhere to leak). Any
// mix is ambiguous and falls back to the whole-value rule, which is lossy but cannot leak.
//
// The `@` test alone is not sufficient, because a comma is also legal inside a QUERY, and a
// query is exactly where the other credential shape lives. `https://idp/jwks?access_token=a,b`
// has no `@` in either piece, so the count rule calls it unambiguous, and the second piece is
// then `b` — a bare fragment of the token with nothing to trim it, joined straight back into
// the output. Every segment must therefore also carry its own `://`: a list of servers is a
// list of URLs, and a comma that does not begin one is a character inside the value.
// The `://` test is in turn not sufficient on its own, because a query can contain one:
// `https://idp/jwks?access_token=a,secret://tail` passes both rules, and the first segment
// trims at its `?` while the second is joined back in whole — the same leak the `://` rule
// was added to close, one shape further out. A `?` or `#` BEFORE the first comma settles it
// without guessing: everything after the start of a query or fragment belongs to it
// (RFC 3986 §3.4, §3.5), so no comma past that point can be a list delimiter.
func URLUserinfo(u string) string {
	if i := strings.IndexRune(u, ','); i >= 0 && !strings.ContainsAny(u[:i], "?#") {
		if parts := strings.Split(u, ","); unambiguousList(parts) && allSchemed(parts) {
			for i, p := range parts {
				parts[i] = redactOne(p)
			}
			return strings.Join(parts, ",")
		}
	}
	return redactOne(u)
}

// unambiguousList reports whether every segment has userinfo or none does — the only two
// cases where a comma cannot be a character inside a password.
func unambiguousList(parts []string) bool {
	withAt := 0
	for _, p := range parts {
		if strings.ContainsRune(p, '@') {
			withAt++
		}
	}
	return withAt == 0 || withAt == len(parts)
}

// allSchemed reports whether every segment begins a URL of its own. A comma that does not
// start a new `scheme://` is a character inside the preceding value — a query separator, or a
// sub-delim in a password — not a list delimiter.
//
// This rejects the schemeless NATS list form (`nats://h1:4222,h2:4222`), deliberately. That
// value falls back to the whole-value rule, which for a credential-free list returns it
// unchanged and for a credential-bearing one is lossy without leaking. Both are correct
// outcomes; admitting the form would mean deciding, from the value alone, whether `h2:4222`
// is a server or the tail of a password.
func allSchemed(parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(p, "://") {
			return false
		}
	}
	return true
}

// redactOne is URLUserinfo for a value known to hold at most one URL.
//
// The split is on the LAST `@` WITHIN THE AUTHORITY, not the last `@` in the string, and not
// the first. Last-within-the-authority because a password may legally contain a percent-
// encoded `@` and splitting on the first would leave the tail of it in the output. Bounded to
// the authority because userinfo can appear nowhere else (RFC 3986 §3.2): scanning the whole
// string mistakes an ordinary `https://idp/jwks?contact=a@b.example` for a credential and
// prints `https://***@b.example`, discarding the host and path that are the only reason to log
// the URL at all.
//
// The authority is bounded at the first `/` after `://`, and at nothing else. `/`, `?` and `#`
// are all illegal unescaped in userinfo, so any of them would be a conforming bound — but a
// value that reaches this function may be exactly the malformed one that failed to parse.
// Bounding at `?` would cut `nats://u:p?x@host` down to `nats://u:p` and log half a password,
// the same defect as splitting on a comma too eagerly. Bounding at `/` cannot: it leaves such
// a value inside the authority region, where the `@` rule redacts it.
//
// The cost is over-redaction of a path-less URL whose query holds an `@` (`https://idp?q=a@b`
// prints `https://***@b`). That is this package's stated preference throughout — lossy beats
// leaky — and no credential escapes it.
//
// The authority bound is trusted only while the value holds ONE url. An ambiguous NATS list
// that `URLUserinfo` declined to split arrives here whole, and its later servers' credentials
// sit outside the first authority. There the conservative whole-string rule applies again:
// redact from the LAST `@` anywhere, losing the earlier hosts rather than printing a password.
//
// Two things about that test, both of which were wrong before and each of which leaked:
//
// It is checked FIRST, not only when the first authority has no `@`. A list where SOME entries
// carry credentials is exactly the mix `URLUserinfo` refuses to split, so it is the likeliest
// value to arrive here — and for `nats://u:p@a,nats://u2:p2@b,nats://c` the first authority
// DOES have an `@`, so a test reached only in the `at < 0` branch never runs. The first
// credential would be redacted, the second printed verbatim.
//
// It requires a COMMA as well as a second `://`. Without a comma there is one URL, and a later
// `://` is inside its query or path — a redirect parameter, most obviously. Treating that as a
// second URL rebuilds the output from an `@` in the query, which drops the `?` along with the
// prefix, so `trimQueryAndFragment` finds nothing to cut and every parameter after that `@`
// survives into the log. One JWKS URL of the form `?redirect=https://x@y&access_token=…` was
// enough to print the token in full.
func redactOne(u string) string {
	scheme := strings.Index(u, "://")
	authStart := 0
	if scheme >= 0 {
		authStart = scheme + 3
	}
	if strings.ContainsRune(u, ',') && strings.Contains(u[authStart:], "://") {
		// More than one URL in the value: the authority bound proves nothing about what
		// follows, so fall back to the whole-string rule.
		if last := strings.LastIndexByte(u, '@'); last >= 0 {
			return trimQueryAndFragment(u[:authStart] + "***@" + u[last+1:])
		}
		return trimQueryAndFragment(u)
	}
	authEnd := len(u)
	if i := strings.IndexByte(u[authStart:], '/'); i >= 0 {
		authEnd = authStart + i
	}
	at := strings.LastIndexByte(u[authStart:authEnd], '@')
	if at < 0 {
		return trimQueryAndFragment(u) // no userinfo, but a query can still carry a token
	}
	at += authStart
	return trimQueryAndFragment(u[:authStart] + "***@" + u[at+1:])
}

// trimQueryAndFragment drops everything from the first `?` or `#`.
//
// A JWKS endpoint is operator-supplied and absolute, so it can carry a credential in the
// QUERY rather than in userinfo — `https://idp/jwks?access_token=…` is a shape real identity
// providers accept. Clearing userinfo and printing the query anyway would leave the package
// honouring the letter of its contract and not the point of it.
//
// It runs AFTER userinfo removal, never before. A password may contain a `?` or a `#`, so
// cutting first could truncate `nats://u:p?x@host` to `nats://u:p` and log half a secret —
// the same defect as splitting on a comma too eagerly. Once userinfo is gone, every
// remaining `?` and `#` is at or after the host, and nothing before it is a credential.
//
// Nothing diagnostic is lost that the host and path do not already give: an endpoint is
// identified by where it points, not by its parameters.
func trimQueryAndFragment(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

// URL is URLUserinfo for an already-parsed URL, and is what every logging and error site with
// a *url.URL should call instead of Redacted().
//
// It clears User on a COPY. Mutating the caller's URL would be a redaction that changes what
// the next request sends — a formatter with a side effect on the credential it is hiding.
//
// RawQuery and Fragment go with it, for the reason given on trimQueryAndFragment: a JWKS URL
// is operator-supplied, and `?access_token=…` is a credential in every sense that matters
// even though `net/url` does not model it as one.
func URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.ForceQuery = false
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}
