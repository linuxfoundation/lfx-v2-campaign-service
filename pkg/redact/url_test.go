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
		{"user and password", "https://svc:s3cret@idp.example.com/.well-known/jwks.json", "https://idp.example.com/.well-known/jwks.json"},
		{"user only", "https://svc@idp.example.com/.well-known/jwks.json", "https://idp.example.com/.well-known/jwks.json"},
		{"empty password", "https://svc:@idp.example.com/jwks", "https://idp.example.com/jwks"},
		{"no userinfo is untouched", "https://idp.example.com/jwks?x=1", "https://idp.example.com/jwks?x=1"},
		{"nats scheme", "nats://user:pw@nats.svc:4222", "nats://nats.svc:4222"},
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
	u, err := url.Parse("https://svc:s3cret@idp.example.com/jwks")
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
		{"https://svc:s3cret@idp.example.com/jwks", "https://***@idp.example.com/jwks"},
		{"https://idp.example.com/jwks", "https://idp.example.com/jwks"},
		// A NATS server LIST is redacted entry by entry. Treating it as one URL would take
		// everything up to the final '@' as userinfo and drop the earlier servers — safe,
		// but it hides the hosts this helper exists to keep visible.
		{"nats://u:p@a:4222,nats://u:p@b:4222", "nats://***@a:4222,nats://***@b:4222"},
		{"nats://a:4222,nats://u:p@b:4222", "nats://a:4222,nats://***@b:4222"},
		// A password may percent-encode an '@'. Splitting on the FIRST one would leave the
		// tail of the password in the output.
		{"https://svc:p%40ss@idp.example.com/jwks", "https://***@idp.example.com/jwks"},
		{"svc:pw@host:4222", "***@host:4222"},
		{"", ""},
	} {
		if got := URLUserinfo(tc.in); got != tc.want {
			t.Errorf("URLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
