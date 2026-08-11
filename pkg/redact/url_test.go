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

// TestURL_OpaqueURIIsCollapsedToItsScheme covers the shape that walks past every other rule in
// this function. An opaque URI — no `//` after the scheme — keeps EVERYTHING after the colon in
// u.Opaque, credentials included, and net/url never populates User, Host or Path from it. So
// clearing User touches nothing and String() renders the value verbatim.
//
// It is reachable: auth.New refuses a JWKS URL whose scheme is not http(s) and formats the
// refused value with this function, so the very value being rejected is the one logged.
func TestURL_OpaqueURIIsCollapsedToItsScheme(t *testing.T) {
	for _, in := range []string{
		"ftp:svc:s3cret@idp.example.com/jwks", // secretlint-disable-line
		"mailto:svc:s3cret@idp.example.com",   // secretlint-disable-line
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if u.Opaque == "" {
			t.Fatalf("%q did not parse as opaque; the test no longer covers what it claims", in)
		}
		got := URL(u)
		if want := u.Scheme + ":***"; got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
		for _, secret := range []string{"svc", "s3cret"} {
			if strings.Contains(got, secret) {
				t.Errorf("URL(%q) = %q, which still carries %q", in, got, secret)
			}
		}
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
		// Path-less, and its only '@' is past the '?'. This is either a password containing a
		// '?' or an ordinary query '@', and the value cannot say which — so nothing after the
		// scheme is printed. The host is lost; a password never is. The case below keeps its
		// host because a '/' closed the authority before the query, which settles it.
		{"nats://u:p?x@host:4222", "nats://***"}, // secretlint-disable-line
		{"https://idp.example.com/jwks#s3cret", "https://idp.example.com/jwks"},
		// A comma is legal in a QUERY too, and the query is where the other credential
		// shape lives. Neither piece of this value carries an '@', so the every-or-none
		// rule alone calls it a list — and the second piece is then 'b64tail', a bare
		// fragment of the token with no '?' to trim it, joined straight back into the
		// output. Requiring every segment to begin its own 'scheme://' is what stops it.
		{"https://idp.example.com/jwks?access_token=s3cret,b64tail", "https://idp.example.com/jwks"},
		// Same rule, credential-free: the schemeless NATS list form does not split either,
		// but the whole-value rule returns it unchanged because there is nothing to redact.
		{"nats://a:4222,b:4222", "nats://a:4222,b:4222"},
		// An '@' AFTER the authority is not a userinfo delimiter. Scanning the whole string
		// for the last '@' redacts this to 'https://***@b.example' — no leak, but the host
		// and path that are the only reason to log a URL are gone, and the operator reading
		// it concludes the endpoint is misconfigured.
		{"https://idp.example.com/jwks?contact=ops@b.example", "https://***"},
		// The host goes with it. `idp.example.com` LOOKS like a well-formed authority, and
		// that was once taken as proof the `/` could only start a path — but userinfo does not
		// have to contain a `:`, so a colonless region is just as legal a token-only userinfo
		// (`nats://token@host`, a documented NATS form) as it is a host. Neither reading is
		// disqualified, so neither is chosen. An explicit port changes nothing either way.
		{"https://idp.example.com:8443/jwks?contact=ops@b.example", "https://***"},
		// The diagnostic cost is narrow: it is paid only by a value that ALSO has an `@` past
		// its `?`, which is why the far commoner `…/jwks?access_token=…` keeps its host.
		{"https://idp.example.com/jwks?access_token=s3cret", "https://idp.example.com/jwks"},
		{"https://idp.example.com:8443/jwks?access_token=s3cret", "https://idp.example.com:8443/jwks"},
		{"https://[::1]:8443/jwks?contact=ops@b.example", "https://[::1]:8443/jwks"},
		// The bracketed forms a real authority takes, all of which must still keep their host
		// now that the literal is validated rather than sniffed by its last byte: no port, an
		// IPv4-mapped tail, and a zone id, whose interface name is not hex and so needs a
		// looser rule than the address in front of it.
		{"https://[::1]/jwks?contact=ops@b.example", "https://[::1]/jwks"},
		{"https://[::ffff:127.0.0.1]/jwks?contact=ops@b.example", "https://[::ffff:127.0.0.1]/jwks"},
		{"https://[fe80::1%25eth0]/jwks?contact=ops@b.example", "https://[fe80::1%25eth0]/jwks"},
		{"https://svc:pw@idp.example.com/jwks?contact=ops@b.example", "https://***@idp.example.com/jwks"}, // secretlint-disable-line
		// The scheme must START a segment, not merely appear in it — the narrowing that closes
		// `allSchemed`'s hole directly. Here `x/y://b@c` carries a `://` but begins with a path
		// character, so it is the tail of a value rather than the head of a URL: no split, and
		// the whole-value rule loses the first host rather than treating the tail as an entry.
		{"nats://u:p@a:4222,x/y://b@c", "nats://***@c"}, // secretlint-disable-line
		{"", ""},
	} {
		if got := URLUserinfo(tc.in); got != tc.want {
			t.Errorf("URLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestURLUserinfo_NeverEmitsACredential covers the three values that got a live secret past
// this package. Each is a case where a rule added to close one leak opened another, so they
// are asserted twice over: the exact output, and — separately — that the secret's own text is
// nowhere in it. The second assertion is the one that generalises. An exact-output test only
// fails for the shape someone thought of; `strings.Contains` fails for any rewriting that
// happens to carry the secret through, which is how all three of these arrived.
func TestURLUserinfo_NeverEmitsACredential(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		secrets        []string
	}{
		{
			// A `://` inside the QUERY is not a second URL. Read as one, the output was
			// rebuilt from the `@` in `x@y.example` — which discards the `?` along with the
			// prefix, so trimQueryAndFragment had no query left to find and the token rode
			// out on `&access_token=`. A redirect parameter is ordinary in an OIDC
			// deployment, and `Config.String` sends JWKS URLs through here.
			name: "query holding a URL with an @ is one URL, not two",
			// The host now goes too, because an unbracketed region before the `/` is as legal
			// a token-only userinfo as it is an authority (see "a token-only userinfo is not
			// a host"). What this case pins is unchanged and is the part that matters: the
			// `://` in the query does not make this two URLs, so the value is never rebuilt
			// from the `@` in `x@y.example` — which is what let the token ride out.
			in:      "https://idp.example.com/jwks?redirect=https://x@y.example&access_token=s3cret", // secretlint-disable-line
			want:    "https://***",
			secrets: []string{"s3cret", "access_token"},
		},
		{
			// The multi-URL fallback used to be reachable only when the FIRST authority had
			// no '@'. This value is the mix URLUserinfo deliberately refuses to split, so it
			// is the likeliest thing to arrive whole — and its first authority does have an
			// '@', so the fallback never ran. `u:p` was redacted and `u2:p2` was printed.
			name:    "mixed list redacts every credential, not just the first",
			in:      "nats://u:p@a:4222,nats://u2:p2@b:4222,nats://c:4222", // secretlint-disable-line
			want:    "nats://***@b:4222,nats://c:4222",
			secrets: []string{"u:p", "u2:p2"},
		},
		{
			// A token tail can be scheme-shaped, so no test applied to a SEGMENT catches this:
			// by the time the segments exist the value has already been cut in the wrong place.
			// The middle entry trimmed at its `?` and `secret://b64tail` — half a token — was
			// joined straight back in. Bounding the CUT at the first `?` is what closes it.
			name:    "a scheme-shaped token tail is not a list entry",
			in:      "nats://a:4222,nats://b:4222?access_token=s3cret,secret://b64tail", // secretlint-disable-line
			want:    "nats://a:4222,nats://b:4222",
			secrets: []string{"s3cret", "b64tail"},
		},
		{
			// No path, so the old authority bound ran to the end of the string and the last
			// `@` — inside the query — was taken for userinfo. Rebuilding around it deleted the
			// `?` along with the prefix, leaving trimQueryAndFragment nothing to cut, so the
			// token rode out on `&access_token=`.
			name:    "a pathless url with an @ in its query keeps no query",
			in:      "https://idp.example.com?contact=ops@b.example&access_token=s3cret", // secretlint-disable-line
			want:    "https://***",
			secrets: []string{"s3cret", "access_token"},
		},
		{
			// A `/` inside userinfo makes this malformed, which is exactly the input this
			// helper exists for. An authority bounded at that `/` saw `u:p`, found no `@`, and
			// returned the value — credential included. The search now runs to the query.
			name:    "a slash inside userinfo does not hide the credential",
			in:      "nats://u:p/x@host:4222", // secretlint-disable-line
			want:    "nats://***@host:4222",
			secrets: []string{"u:p"},
		},
		{
			// The mirror of the case above. Once the search ran to the query, a `/` BEFORE the
			// delimiter was taken as proof that the delimiter began a genuine query — but the
			// `/` here is inside the password too, and `u:p` is no authority for it to close
			// (`p` is not a port). The value was trimmed at the `?` and printed `nats://u:p`.
			// Only an authority a path really closes settles it.
			name:    "a slash inside userinfo does not make a query genuine",
			in:      "nats://u:p/path?x@host:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"u:p", "/path"},
		},
		{
			// The same hole in the multi-URL branch, which asks its own narrower question and
			// so had to be fixed on the same terms rather than inheriting the answer.
			name:    "a slash inside userinfo does not make a list's query genuine",
			in:      "nats://u:p/path?x@a,nats://b:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"u:p", "/path"},
		},
		{
			// Requiring every segment to carry its own `://` was meant to stop a token being
			// split at a comma. A token whose tail happens to contain `://` satisfies it: the
			// first segment trimmed at its `?` and the second was joined straight back in. A
			// `?` before the first comma proves the comma is inside the query.
			name:    "a comma inside a query is never a list delimiter",
			in:      "https://idp.example.com/jwks?access_token=s3cret,secret://b64tail", // secretlint-disable-line
			want:    "https://idp.example.com/jwks",
			secrets: []string{"s3cret", "b64tail"},
		},
		{
			// Multi-URL fallback must not mistake @ in a query for userinfo. The mix
			// `URLUserinfo` refuses to split arrives here whole; searching the whole
			// string for @ would find the query's @, delete the ?, and leave the token.
			// The hosts go with it, for pathClosesAuthority's reason: `idp/jwks` is an
			// unbracketed region, so `idp` is as legally a token-only userinfo as it is a
			// host. What this still pins is the thing it was written for — that no `***@`
			// rebuild happens off the query's `@`, and that `s3cret` never survives.
			name:    "multi-URL fallback does not leak a query @ as userinfo",
			in:      "https://idp/jwks?contact=ops@b.example,https://x&access_token=s3cret", // secretlint-disable-line
			want:    "https://***",
			secrets: []string{"s3cret", "access_token"},
		},
		{
			// The sibling of the case above, and the shape Copilot asked for on #102: the
			// query's `@` is not at the END of the value, so a whole-string LastIndexByte
			// finds it, rebuilds from it, and deletes the `?` along with everything before —
			// leaving `nats://***@live-secret,nats://c`, i.e. the credential SUFFIX printed
			// verbatim with `***` standing in for the harmless part. Bounding the search at
			// the first `?` means the `@` is never seen: nothing before the query holds one,
			// so the value falls through to trimQueryAndFragment and both hosts survive.
			// `nats://a,nats://b` was the old expectation and it is now `nats://***`: the
			// segment owning the `?` is `b`, which carries no `/`, so nothing proves it is a
			// host rather than a whole token. The property under test is unchanged — the
			// credential SUFFIX must never be printed — and redacting whole satisfies it
			// strictly more than keeping the hosts did.
			name:    "multi-URL fallback does not leak a credential suffix after a query @",
			in:      "nats://a,nats://b?access_token=prefix@live-secret,nats://c", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"live-secret", "access_token", "prefix"},
		},
		{
			// Bounding the multi-URL search at the `?` closed the case above and opened its
			// mirror image. With no `@` before the bound the value fell through to
			// trimQueryAndFragment, which cuts at the `?` — and here the `?` is INSIDE the
			// password, so the cut printed `nats://u:p`. The single-URL path already refuses
			// this shape; a comma in the password is not a reason to stop refusing it.
			name:    "multi-URL fallback refuses a password containing a ?",
			in:      "nats://u:p?x@a:4222,nats://b:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"u:p", "p?x"},
		},
		{
			// Which authority owns the `?` is what decides whether it begins a query, and in a
			// list only the LAST segment can own it. Scanning from the start of the value finds
			// the `/` in an EARLIER segment's path — here `https://a/b` — and calls the query
			// genuine, which is the fallthrough the case above exists to prevent. The scan
			// starts at the comma before the delimiter for exactly this input.
			name:    "an earlier segment's slash does not make a later query genuine",
			in:      "https://a/b,nats://u:p?x@host:4222", // secretlint-disable-line
			want:    "https://***",
			secrets: []string{"u:p", "p?x"},
		},
		{
			// The other side of the same rule, and after round 7 there is exactly one side
			// left. A PATH used to be enough — `https://idp.example/jwks?contact=ops@…` kept
			// both hosts — until it turned out `idp.example` is as legally a token-only
			// userinfo as it is a host. A BRACKETED last segment still keeps them, because
			// RFC 3986 §3.2.1 excludes `[` from userinfo, so `[2001:db8::1]` cannot be a
			// credential. This is what stops the branch from being "refuse every multi-URL
			// query outright" — a claim the previous version of this case no longer supported.
			name:    "a list whose last segment is a bracketed host keeps its hosts",
			in:      "nats://a:4222,https://[2001:db8::1]/jwks?contact=ops@b.example&access_token=s3cret", // secretlint-disable-line
			want:    "nats://a:4222,https://[2001:db8::1]/jwks",
			secrets: []string{"s3cret", "access_token"},
		},
		{
			// The IPv6 exemption in pathClosesAuthority was a `HasSuffix(host, "]")`, and a
			// password is free to end in a bracket. `u:secret]` then read as an IPv6 host, so
			// the `/` looked like it closed an authority, so the `?` looked like a genuine
			// query — and cutting there printed the password. The opening bracket is what
			// makes it a literal; requiring it costs nothing a real IPv6 authority has.
			name:    "a password ending in a bracket is not an IPv6 host",
			in:      "nats://u:secret]/path?x@host:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"secret]", "/path"},
		},
		{
			// Requiring the opening bracket alone would have swapped one suffix test for one
			// prefix test. A userinfo that merely OPENS with a bracket has to be refused on the
			// same terms, and the address is what tells them apart: every IPv6 literal has a
			// colon in it and this does not.
			name:    "a bracketed value with no colon is not an IPv6 host",
			in:      "nats://[secret]/path?x@host:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"secret]", "/path"},
		},
		{
			// Reading a NON-numeric right-hand side as the tell left the numeric one, and a
			// numeric PASSWORD is just as legal as a port: `u:1234` is `u`-host-port-1234 and
			// `u`-user-password-1234 in the same eleven bytes. The authority reading won by
			// default, so the `?` was called genuine, the value cut there, and `nats://u:1234`
			// went to the log. An unbracketed `:` before the path is now refused whatever
			// follows it — the one position that cannot ask `isPort` a meaningful question.
			name:    "a numeric password is not a port",
			in:      "nats://u:1234/path?x@host:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"u:1234", ":1234", "/path"},
		},
		{
			// The mirror of the case above, and the reason the colon test was not the fix
			// either. Userinfo does not need a `:`: `nats://token@host:4222` is a documented
			// NATS form that this service's own config accepts, so a colonless `s3cret` is a
			// WHOLE credential rather than a username missing its other half. "No colon, so
			// nothing that could be a password" read it as a bare host, called the `?`
			// genuine, and logged `nats://s3cret/path`.
			//
			// Both readings put a `/` and a `?` inside userinfo, which RFC 3986 §3.2.1
			// excludes — neither is more malformed than the other, so an unbracketed region
			// is refused whatever it contains. A bracketed host still keeps its host (rows
			// above): `[` and `]` are gen-delims §3.2.1 excludes from userinfo, which is a
			// proof about the grammar rather than a guess about the bytes.
			name:    "a token-only userinfo is not a host",
			in:      "nats://s3cret/path?x@host:4222", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"s3cret", "/path"},
		},
		{
			// The multi-URL twin of the case above, and the reason round 6 did not end this.
			// The fix landed in pathClosesAuthority, but the multi-URL caller ANDed its own
			// `ContainsRune(seg, ':')` on top of it, so the colonless reading survived one
			// caller away and this still printed `nats://s3cret/path`. A rule corrected in one
			// place and independently re-derived in another is corrected nowhere; both branches
			// now ask the identical question through queryIsGenuine.
			name:    "a token-only userinfo is not a host in a list either",
			in:      "nats://s3cret/path?x@host:4222,nats://c", // secretlint-disable-line
			want:    "nats://***",
			secrets: []string{"s3cret", "/path"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URLUserinfo(tc.in)
			if got != tc.want {
				t.Errorf("URLUserinfo(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Errorf("URLUserinfo(%q) = %q, which still carries %q", tc.in, got, s)
				}
			}
		})
	}
}
