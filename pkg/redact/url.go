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
// A comma-separated list is redacted ENTRY BY ENTRY. NATS accepts a server list in one URL
// value, and treating such a value as a single URL is safe but lossy: the last-`@` rule below
// would take everything before the final credential as userinfo and drop it, so a three-server
// list diagnoses as one server. Nothing here leaks, but a formatter whose whole justification
// is that the host stays visible should not silently eat two of the three hosts. A value with
// no comma takes the same path with one entry.
func URLUserinfo(u string) string {
	if strings.ContainsRune(u, ',') {
		parts := strings.Split(u, ",")
		for i, p := range parts {
			parts[i] = redactOne(p)
		}
		return strings.Join(parts, ",")
	}
	return redactOne(u)
}

// redactOne is URLUserinfo for a value known to hold at most one URL.
//
// The split is on the LAST `@`, not the first: a password may legally contain a percent-
// encoded `@`, and splitting on the first would leave the tail of it in the output.
func redactOne(u string) string {
	at := strings.LastIndexByte(u, '@')
	if at < 0 {
		return u // no userinfo: nothing to redact
	}
	if scheme := strings.Index(u, "://"); scheme >= 0 && scheme+3 <= at {
		return u[:scheme+3] + "***@" + u[at+1:]
	}
	return "***@" + u[at+1:]
}

// URL is URLUserinfo for an already-parsed URL, and is what every logging and error site with
// a *url.URL should call instead of Redacted().
//
// It clears User on a COPY. Mutating the caller's URL would be a redaction that changes what
// the next request sends — a formatter with a side effect on the credential it is hiding.
func URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	return c.String()
}
