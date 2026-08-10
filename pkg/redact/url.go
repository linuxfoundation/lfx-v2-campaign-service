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
func URLUserinfo(u string) string {
	if strings.ContainsRune(u, ',') {
		if parts := strings.Split(u, ","); unambiguousList(parts) {
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

// redactOne is URLUserinfo for a value known to hold at most one URL.
//
// The split is on the LAST `@`, not the first: a password may legally contain a percent-
// encoded `@`, and splitting on the first would leave the tail of it in the output.
func redactOne(u string) string {
	at := strings.LastIndexByte(u, '@')
	if at < 0 {
		return trimQueryAndFragment(u) // no userinfo, but a query can still carry a token
	}
	if scheme := strings.Index(u, "://"); scheme >= 0 && scheme+3 <= at {
		return trimQueryAndFragment(u[:scheme+3] + "***@" + u[at+1:])
	}
	return trimQueryAndFragment("***@" + u[at+1:])
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
