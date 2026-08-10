// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package redact

import (
	"net/url"
	"strings"
	"testing"
)

// The username is the point of this package. url.URL.Redacted() passes every password case
// below and fails every username one, which is exactly the gap that put a JWKS username into
// this service's error strings.
func TestURL_RemovesTheUsernameAsWellAsThePassword(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"user and password", "https://svc:s3cret@idp.example.com/.well-known/jwks.json", "https://idp.example.com/.well-known/jwks.json"}, // secretlint-disable-line
		{"user only", "https://svc@idp.example.com/.well-known/jwks.json", "https://idp.example.com/.well-known/jwks.json"},
		{"empty password", "https://svc:@idp.example.com/jwks", "https://idp.example.com/jwks"}, // secretlint-disable-line
		{"nats scheme", "nats://user:pw@nats.svc:4222", "nats://nats.svc:4222"},                 // secretlint-disable-line
		// A JWKS endpoint is operator-supplied, and some identity providers take the
		// credential as a query parameter rather than as userinfo. Clearing User and
		// printing the query would satisfy the type system's idea of a credential and
		// miss the actual one.
		{"query is dropped", "https://idp.example.com/jwks?access_token=s3cret", "https://idp.example.com/jwks"},
		{"query dropped even when harmless", "https://idp.example.com/jwks?x=1", "https://idp.example.com/jwks"},
		{"fragment is dropped", "https://idp.example.com/jwks#s3cret", "https://idp.example.com/jwks"},
		{"userinfo and query together", "https://svc:pw@idp.example.com/jwks?access_token=s3cret", "https://idp.example.com/jwks"}, // secretlint-disable-line
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.in)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.in, err)
			}
			got := URL(u)
			if got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The host and path are the whole reason the URL is printed; a redactor that
			// eats them turns a diagnostic into noise and gets replaced by a raw print.
			if !strings.Contains(got, u.Hostname()) {
				t.Errorf("URL(%q) = %q, which lost the host", tc.in, got)
			}
		})
	}
}

// The caller's URL is what the next request is built from. A formatter that clears User in
// place would change what gets SENT, which is a side effect on the credential it is hiding.
func TestURL_DoesNotMutateTheCaller(t *testing.T) {
	u, err := url.Parse("https://svc:s3cret@idp.example.com/jwks") // secretlint-disable-line
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_ = URL(u)
	if u.User == nil {
		t.Fatal("URL cleared the caller's userinfo; the next request would go out unauthenticated")
	}
	if pw, _ := u.User.Password(); pw != "s3cret" {
		t.Errorf("password is now %q, want it untouched", pw)
	}
}

func TestURL_NilIsEmptyNotAPanic(t *testing.T) {
	if got := URL(nil); got != "" {
		t.Errorf("URL(nil) = %q, want \"\"", got)
	}
}

// The string form exists for the one site that has no *url.URL to work with: the path where
// url.Parse FAILED, whose error embeds the whole raw URL.
func TestURLUserinfo_Shapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://svc:s3cret@idp.example.com/jwks", "https://***@idp.example.com/jwks"}, // secretlint-disable-line
		{"https://idp.example.com/jwks", "https://idp.example.com/jwks"},
		// A NATS server LIST is redacted entry by entry when EVERY entry carries an '@'.
		// Treating it as one URL would take everything up to the final credential as
		// userinfo and drop the earlier servers — safe, but it hides the hosts this helper
		// exists to keep visible.
		{"nats://u:p@a:4222,nats://u:p@b:4222", "nats://***@a:4222,nats://***@b:4222"}, // secretlint-disable-line
		// No entry carries an '@', so there is no credential anywhere and every host stays.
		{"nats://a:4222,nats://b:4222", "nats://a:4222,nats://b:4222"},
		// MIXED, and therefore ambiguous: 'nats://a:4222' is either a host and port or a
		// user and password, and this value cannot say which. So it does NOT split — it
		// falls back to the whole-value rule, which loses the first host but cannot leak.
		// Preferring the prettier per-entry output here is what the case below punishes.
		{"nats://a:4222,nats://u:p@b:4222", "nats://***@b:4222"}, // secretlint-disable-line
		// The reason the mixed case must not split. ',' is an RFC 3986 sub-delim and legal
		// unescaped in userinfo, so this is ONE url whose password contains a comma. Split
		// it and the first piece is 'nats://u:p' — no '@', nothing redacted, half the
		// password in the log.
		{"nats://u:p,x@host:4222", "nats://***@host:4222"}, // secretlint-disable-line
		// A password may percent-encode an '@'. Splitting on the FIRST one would leave the
		// tail of the password in the output.
		{"https://svc:p%40ss@idp.example.com/jwks", "https://***@idp.example.com/jwks"}, // secretlint-disable-line
		{"svc:pw@host:4222", "***@host:4222"},
		// The query goes too: a credential can ride in it, and userinfo removal alone would
		// print it. Stripping happens AFTER userinfo removal — cutting at the first '?'
		// first would truncate the case below to 'nats://u:p' and log half a password.
		{"https://idp.example.com/jwks?access_token=s3cret", "https://idp.example.com/jwks"},
		{"https://svc:pw@idp.example.com/jwks?access_token=s3cret", "https://***@idp.example.com/jwks"}, // secretlint-disable-line
		{"nats://u:p?x@host:4222", "nats://***@host:4222"},                                              // secretlint-disable-line
		{"https://idp.example.com/jwks#s3cret", "https://idp.example.com/jwks"},
		{"", ""},
	} {
		if got := URLUserinfo(tc.in); got != tc.want {
			t.Errorf("URLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
