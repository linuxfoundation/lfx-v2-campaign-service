// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// The snapshot redactors below reduce caller-supplied links to a form that is safe to
// persist in campaigns.config_snapshot, which is stored UNENCRYPTED and indexed. They
// were moved here unchanged from internal/dispatch (which keeps thin
// sanitizeSnapshotURL/sanitizeSnapshotText wrappers for its per-adapter scrubbing) so the
// service layer's update path — which cannot import internal/dispatch without a cycle —
// redacts with the exact same rules rather than a second copy that could drift.

// SnapshotURL strips the PATH, query and fragment from a URL before it is stored
// in config_snapshot (which is persisted UNENCRYPTED). The snapshot keeps only
// scheme+host. An absolute URL is reduced to that; an http(s)-scheme value, or any value
// opening with `scheme://`, that will not reduce — it does not parse, has no host, or
// carries userinfo — is dropped entirely. A
// value that never claimed to be a URL is truncated at the first '?'/'#' and dropped if
// it still contains a credential delimiter '@', mirroring the reddit client's redactURL
// fail-closed behavior. An empty input stays empty.
//
// The path used to be kept, on the reasoning that a path segment is a route and not a
// secret. `caller-url-must-be-redacted-before-errors-steps-and-snapshots` says otherwise
// in as many words: `https://litellm.example.com/sup3r-s3cret/v1` parses with the token
// as a PATH segment, and `redactAIProxyURL` took four rounds to stop making exactly that
// assumption. A one-time password reset or magic-link URL pasted into tweetText —
// `https://example.org/reset/SECRET` — is the realistic shape here, and it survives the
// query-and-fragment strip untouched.
//
// The HOST stays, and that is not the same call. The knowledge-base rule is a two-part
// test — reproduce a component only when it is BOTH structurally incapable of holding a
// secret AND load-bearing. This snapshot's only reader is a human reconstructing what a
// campaign was configured with, and "which site did this link point at" is the whole of
// what a redacted URL can still tell them. The path is not load-bearing for that, so it
// fails the test's second half and goes; over-redacting a path costs nothing here,
// because unlike an operator-facing error this value is never used to diagnose anything
// in the moment.
func SnapshotURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if u, err := url.Parse(trimmed); err == nil && u.IsAbs() && u.Host != "" && u.User == nil {
		// Rebuilt through url.URL.String() rather than concatenated: Host holds the
		// DECODED authority, so a zone-scoped IPv6 literal comes back as
		// `[fe80::1%eth0]` — a bare `%` that is not a valid escape, turning a
		// well-formed URL into one that no longer parses. String() re-escapes it.
		redacted := url.URL{Scheme: u.Scheme, Host: u.Host}
		return redacted.String()
	}
	// A value that ANNOUNCED itself as http(s) and did not reduce above fails closed
	// here, rather than falling through to the truncating branch below. Reaching that
	// branch means the parse failed, or produced no host, or carried userinfo — and in
	// the first two cases the truncating branch keeps the PATH, which is the exact
	// exposure the scheme+host reduction exists to close. `https:///reset/SECRET`
	// parses cleanly with an EMPTY host and no '?', '#' or '@' to truncate at, so it
	// was returned whole; `https://example.org/reset/SEC%zz` fails to parse on the bad
	// escape and was likewise returned whole.
	//
	// Nothing legitimate is lost: every caller (reddit PostURL/ImageURL, meta
	// ImageURL, and the runs SnapshotText feeds in, which the regex only
	// matches from an http/https scheme) supplies a URL, so an http-shaped value that
	// will not reduce is malformed input, not data with another meaning. The branch
	// below still exists for a value that never claimed to be a URL — a reddit thing
	// id, say — where truncating and dropping on '@' is the conservative answer.
	if isHTTPScheme(trimmed) || hasAuthorityScheme(trimmed) {
		return ""
	}
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	if strings.Contains(trimmed, "@") {
		return "" // fail closed: don't store a value that may embed userinfo credentials
	}
	// A scheme-LESS value can still be a link with its secret in the path —
	// `example.org/reset/SECRET` announces no scheme, so the reduction above declined it
	// and there is no '?', '#' or '@' left to truncate at. Reduce it the same way
	// SnapshotText's fourth pass does, through the same helper, so a field and the
	// same link written inside tweetText do not redact differently. A value that is not
	// link-shaped (a reddit thing id, say) has no dotted TLD-shaped host followed by a
	// slash and falls through untouched, which is what the truncating branch is for.
	if m := schemelessPathSnapshotRunRe.FindString(trimmed); m == trimmed {
		return sanitizeSchemelessPathSnapshotRun(trimmed)
	}
	return trimmed
}

// isHTTPScheme reports whether raw ANNOUNCES an http or https scheme. Case-insensitive
// because RFC 3986 §3.1 makes schemes case-insensitive and snapshotURLRunRe matches
// `HTTPS://` runs accordingly — a case-sensitive test here would let exactly those runs
// fall through to the truncating fallback the check exists to keep them out of.
//
// The `//` is NOT required, and that is the whole point of the test. It used to be, so a
// value with the scheme and a malformed authority — `http:/reset/SECRET`, one slash,
// which is how a hand-typed or line-wrapped link arrives — failed the scheme+host
// reduction above, failed this check, and then fell into the truncating branch with no
// '?', '#' or '@' to truncate at. It was stored whole, path and all: the exact exposure
// the reduction exists to close, reached by dropping a single character. `http:` with no
// slashes at all (the opaque form, `http:reset/SECRET`) has the same shape and the same
// answer.
//
// Requiring only the scheme costs nothing a caller has: every caller here supplies a URL
// (see SnapshotURL), so an http-announcing value that will not reduce is
// malformed input rather than data with another meaning. A value that never claimed to be
// a URL does not start with `http:` and is untouched by this.
func isHTTPScheme(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http:") || strings.HasPrefix(lower, "https:")
}

// authoritySchemePrefixRe matches a value that opens with ANY RFC 3986 scheme followed by
// `//` — `ftp://`, `file://`, `s3://`.
var authoritySchemePrefixRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

// hasAuthorityScheme reports whether raw announces a scheme with an authority, of any kind.
// It extends isHTTPScheme's fail-closed rule to every such scheme: a value that announced
// `ftp://` or `file://` and did not reduce to scheme+host above has no host to keep
// (`file:///private/RESET_TOKEN`) or will not parse, and the truncating branch below would
// keep its path — the same exposure, reached by a scheme the http check does not name.
func hasAuthorityScheme(raw string) bool {
	return authoritySchemePrefixRe.MatchString(raw)
}

// schemeAuthorityRunRe matches a NON-http link run in free text: any scheme, then `//`,
// then the run up to snapshotURLRunRe's stop set, with the same bracketed-host branch (and
// optional userinfo before it) for an IPv6 literal. The body may be empty after `//`, so
// `file:///private/TOKEN` is matched whole. It runs AFTER the http pass, so an http(s) run
// it meets has already been reduced to scheme+host, which SnapshotURL returns unchanged.
var schemeAuthorityRunRe = regexp.MustCompile(
	`(?i)[a-z][a-z0-9+.-]*://(?:(?:[^\s<>"\x60\[\]}|\\^/?#@]*@)?\[[^\]\s]+\][^\s<>"\x60\]}|\\^]*|[^\s<>"\x60\]}|\\^]*)`,
)

// snapshotURLRunRe matches an http/https URL run inside free text: everything from the
// scheme up to the first character that cannot continue a URL. Whitespace ends a run, and
// so do the characters that in prose almost always belong to the sentence rather than the
// link — a trailing double quote, bracket or angle. Sentence-final punctuation ('.', ',',
// ')', '!', '?') IS admitted here, because it is legal inside a URL and a run trimmed too
// eagerly leaves the query fragment behind as bare text, which is the exact leak this
// exists to prevent. Over-matching costs a sanitized URL a trailing period; under-matching
// costs a token.
//
// The stop set holds only characters RFC 3986 excludes from a URI outright, which is what
// makes stopping at them safe. The APOSTROPHE was in it and does not belong: `'` is a
// sub-delimiter, legal in a query, so `…/reg?x=1'api_token=SECRET` ended the run at the
// quote and left `'api_token=SECRET` in the snapshot as bare text — under-matching
// costing exactly the token this exists to stop. Admitting it costs at most a possessive
// `'s` being swallowed into a run that is then reduced to scheme+host anyway, which is
// the cheap side of that trade. The single quote is therefore NOT a stop character; the
// double quote still is, because `"` cannot appear in a URI unescaped.
//
// The leading alternative exists because `]` is in that stop set — it ends a markdown
// link — and an IPv6 literal host is written INSIDE brackets. Without it,
// `https://[2001:db8::1]/reg?ticket=…` matched only as far as `https://[2001:db8::1`,
// the truncated prefix went to SnapshotURL, and `]/reg?ticket=…` stayed in the
// snapshot as bare text — under-matching costing exactly the token this guards. A
// bracketed host is therefore consumed whole first, and only the REST of the run is
// subject to the stop set.
//
// That branch admits any non-space run up to the closing `]`, NOT a hex/colon IP
// grammar. A tighter class looks safer and is not: it rejected the zone-scoped form
// `https://[fe80::1%25eth0]/…`, which then fell through to the general alternative and
// truncated at the bracket again — reopening the leak for the one host shape the branch
// was added for. Matching the authority whole and leaving its validity to net/url is the
// direction that fails safe here: over-matching a bracketed run that is not a host costs
// a sanitized fragment of prose, while under-matching costs a token.
//
// The bracketed branch admits an optional USERINFO before the bracket. Requiring `[` hard
// against `//` sent `https://bob:pw@[2001:db8::1]/reset/SECRET?token=…` to the general
// alternative, which stopped at the `]`, so SnapshotURL dropped only the prefix and
// `]/reset/SECRET?token=…` stayed behind as bare text. Consumed whole, the run reaches
// SnapshotURL with its userinfo intact and is dropped entirely, as any userinfo run is.
//
// There is NO `\b` before the scheme, and deliberately no replacement for it. Go's `\b`
// is defined over `\w`, and `\w` includes `_`, so `_https://…` had no boundary between the
// underscore and the `h` and the whole run went unmatched — `_https://events.example/cb?
// access_token=…` survived intact into the UNENCRYPTED config_snapshot. An underscore is
// how markdown italicises and how a copied link arrives out of half the clients an
// operator pastes from, so that is a shape real text has.
//
// Fixing only `_` would leave the same hole one character over. This function redacts
// what it finds and leaves everything else alone, so the cost of matching a scheme buried
// inside a longer word is a mangled fragment of prose in a snapshot, and the cost of
// missing one is a persisted token — the asymmetry the stop set above is already chosen
// for. The screen in `internal/platform/twitter` keeps a boundary rule because the same
// scanner there also drives length weighting and destination matching, where
// over-matching means something. Here nothing depends on it.
// The `//` is OPTIONAL, for the reason documented on isHTTPScheme: a run that announces
// the scheme and then malforms the authority — `http:/reset/SECRET`, or the opaque
// `http:reset/SECRET` — is a link, and requiring the double slash left it unmatched in
// free text and therefore stored verbatim. The third alternative consumes it, and
// SnapshotURL fails it closed. It cannot swallow ordinary prose: something has to
// follow the colon with no space between, and a sentence-final `http:` is not matched at
// all.
var snapshotURLRunRe = regexp.MustCompile(
	`(?i)https?:(?://(?:(?:[^\s<>"\x60\[\]}|\\^/?#@]*@)?\[[^\]\s]+\][^\s<>"\x60\]}|\\^]*|[^\s<>"\x60\]}|\\^]+)` +
		`|/?[^\s<>"\x60\]}|\\^/][^\s<>"\x60\]}|\\^]*)`,
)

// SnapshotText strips the query and fragment from every URL embedded in free text
// before that text is stored in config_snapshot (which is persisted UNENCRYPTED).
//
// SnapshotURL already does this for a field that IS a URL. A free-text field is the
// same exposure with an extra step: X's tweetText is operator-authored prose that routinely
// carries a registration link, and a link pasted out of a browser carries whatever query the
// operator's session put there — including, in the shapes the create path now refuses
// outright, a token. Refusing those at create does not help a campaign snapshot written
// before that check existed, nor a credential-shaped parameter the denylist does not name,
// and the snapshot is the copy that persists.
//
// Each run is rewritten through SnapshotURL, so the two paths cannot drift on what
// "stripped" means. A run SnapshotURL fails closed on (embedded userinfo) is
// replaced by nothing, which is the same fail-closed answer the single-URL path gives. Text
// outside a URL run is left exactly as written — this redacts links, it does not attempt to
// find secrets in prose.
func SnapshotText(raw string) string {
	if raw == "" {
		return ""
	}
	// Scheme-ful runs first, then scheme-less ones over the RESULT. The order is what
	// keeps the second pass off links the first already handled: the first pass strips
	// every query and fragment it rewrites, and the second only ever matches a run that
	// still HAS one, so a reduced `https://a.example` is invisible to it. No masking or
	// offset bookkeeping is needed to get that.
	// The userinfo pass runs LAST, over what the other two leave. It requires no query,
	// so running it earlier would take `bob:pw@a.example/r?token=S` down to `a.example`
	// before the query pass ever saw it — same end state, but by the pass that is not
	// responsible for queries. Last, it only ever sees runs the first two had nothing to
	// say about, which is the `user:password@host` with no query at all.
	// The PATH-ONLY pass runs after all three, and last for the same reason the userinfo
	// pass was already last: it is the least discriminating of the four, so it must only
	// ever see what the others had nothing to say about. Running it before the userinfo
	// pass would reduce `bob:pw@a.example/r` to `bob:pw@a.example` and hand that pass a
	// run it no longer matches in full; running it before the query pass would take
	// `a.example/r?token=S` down to `a.example` by the pass that is not responsible for
	// queries. Placed last it sees neither: every earlier pass rewrites its runs to a bare
	// authority or to nothing, and a bare authority has no path for this one to match.
	// The non-http pass runs second, over what the http pass left: `ftp://`, `file://` and
	// every other `scheme://` run goes through SnapshotURL too, which reduces it to
	// scheme+host or drops it when it has no host to keep. Before it existed such a run was
	// matched by nothing that understood it, and `file:///private/RESET_TOKEN` or
	// `ftp://[2001:db8::1]/reset/SECRET?token=SECRET` was persisted whole.
	out := snapshotURLRunRe.ReplaceAllStringFunc(raw, SnapshotURL)
	out = schemeAuthorityRunRe.ReplaceAllStringFunc(out, SnapshotURL)
	out = schemelessSnapshotRunRe.ReplaceAllStringFunc(out, sanitizeSchemelessSnapshotRun)
	out = schemelessUserinfoSnapshotRunRe.ReplaceAllStringFunc(out, sanitizeUserinfoSnapshotRun)
	return schemelessPathSnapshotRunRe.ReplaceAllStringFunc(out, sanitizeSchemelessPathSnapshotRun)
}

// schemelessSnapshotRunRe matches a scheme-less link carrying a query or fragment, the
// same shape internal/platform/twitter screens before publication.
//
// The two sides have to agree. The twitter client was taught that X linkifies and
// publishes `www.host/r?…` and bare `host.tld/r?…`, and refuses the ones naming a
// credential — but tweetText is ALSO persisted, and this redactor still required a
// scheme, so a scheme-less link the screen merely did not object to (its parameters are
// not on the denylist, or it reached an older campaign written before that screen
// existed) kept its full query and fragment in the UNENCRYPTED config_snapshot. Fixing
// the publication side alone moved the exposure rather than closing it.
//
// The `?`/`#` requirement, the letter-initial TLD, the punycode and IPv4 host forms and
// the reasons for each are documented on `schemelessScreenRunRe` in
// internal/platform/twitter/client.go. Keep the two in step.
//
// The `?`/`#` requirement is the one thing NOT kept in step, and schemelessPathSnapshotRunRe
// below is where this side goes further: a path-only link has no parameter for the screen to
// read but still carries its secret into the snapshot. See that pattern for why the extra
// reach is affordable here and not there.
//
// That requirement is what keeps this off ordinary prose, but not perfectly: `Vue.js?Check`
// — a dotted token, no space after the question mark — matches, and its tail is dropped.
// That is the same trade snapshotURLRunRe's stop set was already chosen under, in the same
// direction: a mangled fragment of prose in a snapshot no one diagnoses anything with,
// against a persisted token.
// One deliberate difference from the twitter pattern: the optional userinfo prefix. The
// twitter screen only READS a run's parameters, so where a run begins costs it nothing;
// this one REWRITES the run, so a pattern that starts at the host leaves `user:pw@` behind
// as bare text — the password surviving the redaction of the token beside it. Consuming
// the userinfo into the run instead makes url.Parse see it and sanitizeSchemelessSnapshotRun
// fail the whole run closed, which is the answer SnapshotURL already gives.
var schemelessSnapshotRunRe = regexp.MustCompile(
	`(?i)(?:[^\s/?#@]*@)?(?:\d{1,3}(?:\.\d{1,3}){3}|[a-z0-9][a-z0-9._~%+-]*\.[a-z][a-z0-9-]+)` +
		`(?::\d+)?(?:/[^\s<>"\x60\]}|\\^]*)?[?#][^\s<>"\x60\]}|\\^]+`,
)

// schemelessUserinfoSnapshotRunRe matches `user:password@host.tld`, the scheme-less shape
// that carries a credential with no query to carry it. It is replaced by NOTHING rather
// than reduced to a host: a run reaching this pass has already failed to be either of the
// other two shapes, and the only part of it worth keeping — which site the link pointed
// at — is not worth the risk of getting the split between userinfo and authority wrong on
// a malformed run.
//
// The colon is the discriminator, exactly as on schemelessUserinfoRunRe in
// internal/platform/twitter/client.go: without it this matches every email address an
// operator writes in their copy. Keep the two in step.
//
// The USERNAME class is RFC 3986's userinfo alphabet before the colon — unreserved,
// pct-encoded and the sub-delims `!$&'()*+,;=`. It used to stop at the unreserved set, so
// `admin!:pw@events.example/reset/TOKEN` was not matched here, and the path-only pass below
// then left it whole because it contains an `@`: password and path token both persisted.
// Kept in step with the twitter screen's class.
//
// It is not the only discriminator needed, and the second one is kept in step too: a time
// of day written hard against a host — `keynote 14:00@events.example` — is the userinfo
// production byte for byte. See sanitizeUserinfoSnapshotRun.
var schemelessUserinfoSnapshotRunRe = regexp.MustCompile(
	`(?i)[a-z0-9._~%+!$&'()*,;=-]+:[^\s<>"\x60\]}|\\^@]*@` +
		`(?:\d{1,3}(?:\.\d{1,3}){3}|[a-z0-9][a-z0-9._~%+-]*\.[a-z][a-z0-9-]+)` +
		`(?::\d+)?(?:/[^\s<>"\x60\]}|\\^]*)?`,
)

// sanitizeUserinfoSnapshotRun blanks a matched userinfo run unless both sides of the colon
// are digits, which makes it a clock, a score or a ratio rather than a credential pair.
//
// The rule is deliberately identical to userinfoRunIsClockShaped in
// internal/platform/twitter/client.go — the two patterns are documented as kept in step,
// and a discriminator that lived on only one of them would put the screen and the redactor
// back out of agreement, which is the exact defect the third pass was added to fix.
//
// The COST direction differs, and it is worth being clear that this side is the milder
// one: over-redacting here loses a line of the operator's own copy from a diagnostic
// snapshot, where over-refusing on the twitter side blocks a brief before anything is
// created. Milder is not free — the snapshot exists to be read by a human — and the
// digits-both-sides test gives up no credential shape to buy it.
func sanitizeUserinfoSnapshotRun(run string) string {
	at := strings.IndexByte(run, '@')
	if at < 0 {
		return ""
	}
	userinfo := run[:at]
	colon := strings.IndexByte(userinfo, ':')
	if colon >= 0 && isAllASCIIDigits(userinfo[:colon]) && isAllASCIIDigits(userinfo[colon+1:]) {
		return run
	}
	return ""
}

// schemelessPathSnapshotRunRe matches a scheme-less link whose secret is in the PATH and
// which carries no query or fragment at all: `example.org/reset/SECRET`. Every other pass
// needs a scheme, a '?'/'#', or a userinfo colon to fire, so this shape — the one-time
// password reset link that SnapshotURL's own doc comment names as the realistic
// case — was the single URL shape that reached config_snapshot with its path intact.
//
// The host production is schemelessSnapshotRunRe's, minus the userinfo prefix (the pass
// before this one has already blanked those) and with the '?'/'#' requirement replaced by
// a REQUIRED '/'. That slash is now the only thing holding this off ordinary prose, which
// is why the TLD-shaped final label matters more here than it does there: a letter-initial
// label of two or more characters after a dot, so `and/or` and `9.5/10` do not match and
// `Vue.js/x` does — losing a mangled `/x` from a snapshot, the same trade the other
// patterns are already chosen under.
//
// This is deliberately NOT mirrored into internal/platform/twitter's screen, and the
// divergence note on schemelessScreenRunRe records why: the two patterns are kept in step
// on what a LINK looks like, not on what to do about one, because their cost directions
// are opposite. Over-matching here loses a fragment of the operator's own copy from a
// snapshot no one diagnoses anything with; over-matching there refuses a brief X would
// have accepted, and no retry fixes a refusal. A pattern whose only discriminator is a
// slash is affordable on this side and not on that one.
//
// The optional `@` prefix is matched only so the run can be RECOGNISED and then left
// alone — Go's regexp has no lookbehind, so consuming the prefix is the only way to see
// that a host was written hard against one. By the time this pass runs, the userinfo pass
// has already blanked every `user:password@host` run, so an `@` still standing belongs to
// something that pass deliberately kept: a clock (`9:30@main.stage/agenda`) or a
// colon-less email. Reducing those would undo a decision already made one pass earlier by
// the code that owns it.
var schemelessPathSnapshotRunRe = regexp.MustCompile(
	`(?i)(?:[^\s<>"\x60\]}|\\^/?#]*@)?` +
		`(?:\d{1,3}(?:\.\d{1,3}){3}|[a-z0-9][a-z0-9._~%+-]*\.[a-z][a-z0-9-]+)` +
		`(?::\d+)?/[^\s<>"\x60\]}|\\^]*`,
)

// sanitizeSchemelessPathSnapshotRun reduces a path-only scheme-less run to its authority.
//
// It reduces rather than blanking, which is where it parts company with
// sanitizeUserinfoSnapshotRun. That pass blanks because a run reaching it has already
// failed to be either of the other shapes and getting the userinfo/authority split wrong
// on a malformed run risks writing a password back out. Here there is no userinfo to split
// — the run begins at the host by construction, and the pass before this one has blanked
// the credential-carrying shape — so the host is safe to keep, and "which site did this
// link point at" is exactly the load-bearing fact SnapshotURL keeps for the
// scheme-ful case. Blanking instead would make the same link redact differently depending
// on whether the operator typed `https://` in front of it.
func sanitizeSchemelessPathSnapshotRun(run string) string {
	if strings.IndexByte(run, '@') >= 0 {
		// Not this pass's run to rewrite — see the pattern's note on the `@` prefix. The
		// credential-carrying shape is already gone; what is left was kept on purpose.
		return run
	}
	host := run
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	// Parsed with a placeholder scheme so net/url reads the token as an AUTHORITY rather
	// than an opaque path, and trimmed back off so the snapshot says what the operator
	// wrote — the same round trip (and the same re-escaping of a decoded Host) as
	// sanitizeSchemelessSnapshotRun.
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	rebuilt := url.URL{Scheme: "https", Host: u.Host}
	return strings.TrimPrefix(rebuilt.String(), "https://")
}

func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sanitizeSchemelessSnapshotRun reduces a scheme-less run to its authority, and fails
// CLOSED to the empty string on anything that will not parse or carries userinfo —
// the same answer SnapshotURL gives for the scheme-ful case, for the same
// reason: a run that announced itself as a link and then would not reduce is malformed
// input, and the truncating fallback would keep the path.
//
// The scheme is supplied only so net/url reads the leading token as an AUTHORITY rather
// than an opaque path, and is not written back — the snapshot should say what the
// operator wrote, and they wrote no scheme.
func sanitizeSchemelessSnapshotRun(run string) string {
	u, err := url.Parse("https://" + run)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	// Host holds the DECODED authority, so it is rebuilt through url.URL.String() and
	// the placeholder scheme trimmed back off, rather than concatenated — the same
	// re-escaping SnapshotURL relies on.
	rebuilt := url.URL{Scheme: "https", Host: u.Host}
	return strings.TrimPrefix(rebuilt.String(), "https://")
}
