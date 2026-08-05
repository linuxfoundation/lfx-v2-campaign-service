// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package utm tags outbound email links so email traffic is attributable.
//
// Without this the paid channels carry UTM parameters (see each platform client's
// <platform>UTMParams) while email links arrive bare, so email sessions land in the warehouse
// as direct/unattributed traffic and the marketing dashboards cannot see the channel at all.
package utm

import (
	"net/url"
	"regexp"
	"strings"
)

// Params are the UTM parameters applied to every link in one email.
type Params struct {
	// Source and Medium identify the channel. LF Events uses source=email, medium=LF-Events
	// (see DefaultSource/DefaultMedium) — medium is NOT "email", which is a deliberate
	// convention the warehouse reports depend on.
	Source string
	Medium string
	// Campaign is the campaign slug. Resolved from the HubSpot campaign when the template
	// email belongs to one, otherwise derived from the email name.
	Campaign string
	// Term is optional and usually empty for email.
	Term string
}

// The LF Events email conventions. Medium is "LF-Events", not "email": the warehouse's
// channel reporting keys on this pair, so changing either silently re-buckets historical
// comparisons rather than failing.
const (
	DefaultSource = "email"
	DefaultMedium = "LF-Events"
)

// FallbackCampaign is used when neither a HubSpot campaign nor a usable email name yields a
// slug. It is deliberately a real value rather than "": an empty utm_campaign is what makes a
// session unattributable, which is the whole problem this package exists to fix.
const FallbackCampaign = "email-campaign"

// nonSlugChars matches every run of characters that is not a lowercase letter or digit.
var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// taggableSchemes are the ONLY explicit schemes a link may carry and still be tagged.
//
// This is an ALLOWLIST, not a denylist, and deliberately so. A denylist of the known-bad schemes
// (mailto:, tel:) silently let every other non-web action through: `javascript:void(0)` — the
// standard placeholder href in marketing-tool HTML — became `javascript:void(0)?utm_source=…`,
// which changes the expression the browser evaluates. `sms:`, `data:` and `ftp:` mangled the same
// way. Only http/https carry a query string that means what UTM tagging assumes it means, so
// anything else is left exactly as the author wrote it.
//
// A link with NO scheme (relative "/register", protocol-relative "//lf.dev/x") is still eligible:
// those resolve to the email's web destination and are the ordinary case in templated HTML. A
// bare "#anchor" is handled separately — it is an in-page jump, not a destination.
var taggableSchemes = map[string]bool{"http": true, "https": true}

// Slug converts free text to a URL-safe slug: "Register Now" -> "register-now".
// Returns "" when the text contains nothing sluggable, so callers can detect that and fall
// back rather than emitting an empty parameter.
func Slug(text string) string {
	return strings.Trim(nonSlugChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(text)), "-"), "-")
}

// SlugWithSuffix builds a slug and ensures it ends with suffix ("Register Now" + "cta" ->
// "register-now-cta"). When the text yields nothing sluggable the suffix alone is used, so the
// result is never empty as long as the suffix isn't.
func SlugWithSuffix(text, suffix string) string {
	slug := Slug(text)
	switch {
	case slug == "":
		return suffix
	case suffix == "" || strings.HasSuffix(slug, "-"+suffix) || slug == suffix:
		return slug
	default:
		return slug + "-" + suffix
	}
}

// Apply merges the UTM parameters into rawURL's query string and returns the tagged URL.
//
// It returns the URL UNCHANGED when:
//   - rawURL is blank, or p carries no campaign (an empty utm_campaign is worse than none:
//     it looks tagged in reports while attributing nothing);
//   - the link is not a web destination — it carries an explicit scheme other than http/https
//     (mailto:, tel:, javascript:, sms:, data:, …), or it is a bare "#fragment". Relative and
//     protocol-relative links ARE eligible. See taggableSchemes;
//   - the URL ALREADY carries a non-empty utm_campaign. Re-tagging a pre-tagged link would
//     silently overwrite a deliberate campaign an author set by hand, and that loss is
//     invisible — the link still works, it just reports to the wrong campaign.
//
// content, when non-empty, becomes utm_content, distinguishing which link in the email was
// clicked. An unparseable URL is returned unchanged rather than dropped: a broken link in the
// email is a far worse outcome than an untagged one.
func Apply(rawURL string, p Params, content string) string {
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(p.Campaign) == "" {
		return rawURL
	}
	// A token-only href (e.g., "{{contact.email}}") has no destination until HubSpot
	// substitutes the token at send time. Appending UTM query params to it creates
	// "{{contact.email}}?utm_campaign=...", and when HubSpot expands the token, the
	// parameters end up attached to whatever URL the token resolves to, potentially
	// breaking the structure if the destination URL already has a query or fragment.
	// Token-only hrefs must pass through unchanged.
	trimmed := strings.TrimSpace(rawURL)
	if templateToken.MatchString(trimmed) && templateToken.ReplaceAllString(trimmed, "") == "" {
		// The URL is only whitespace and template tokens; return it unchanged.
		return rawURL
	}
	if !isTaggable(rawURL) {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	// Scan the RAW query, not u.Query(). Values.Get returns only the FIRST value, so
	// `?utm_campaign=&utm_campaign=hand-picked` would pass the guard and then have the author's
	// deliberate campaign overwritten — hidden behind a leading empty value. And url.Query()
	// DISCARDS semicolon-separated pairs entirely (Go 1.17+ rejects `;` as a separator), so
	// `?utm_campaign=hand-picked;a=1` parsed to nothing: the guard saw no campaign, and tagging
	// then replaced the author's campaign AND dropped `a=1` with it. Reading the raw query is
	// the only way to see both forms — the same reason this function appends rather than
	// re-encoding.
	for _, v := range rawQueryValues(u.RawQuery, "utm_campaign") {
		if strings.TrimSpace(v) != "" {
			return rawURL
		}
	}

	// APPEND to the existing raw query rather than rebuilding it with Encode().
	//
	// Encode() re-serializes the whole query, which:
	//   - REORDERS keys alphabetically, so a token restored "by value" afterwards could land on
	//     the wrong occurrence (?z={{id}}&a=%7B%7Bid%7D%7D swapped which one was live);
	//   - DROPS anything it cannot round-trip, notably semicolon-separated params: `a=1;b=2`
	//     parsed to nothing and vanished from the URL entirely;
	//   - percent-encodes HubSpot's {{...}} tokens, which then needed undoing.
	//
	// Appending sidesteps all three: every original byte of the query survives untouched, and
	// only the UTM pairs are added.
	var add []string
	appendParam := func(k, v string) {
		add = append(add, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}
	appendParam("utm_source", orDefault(p.Source, DefaultSource))
	appendParam("utm_medium", orDefault(p.Medium, DefaultMedium))
	appendParam("utm_campaign", p.Campaign)
	if content != "" {
		appendParam("utm_content", content)
	}
	if p.Term != "" {
		appendParam("utm_term", p.Term)
	}

	// Drop any existing utm_* pairs before appending. Appending alone leaves the ORIGINAL
	// values first, and readers taking the first occurrence would see those — a stale
	// `utm_source=facebook` would out-rank the appended `email`.
	base := stripUTM(u.RawQuery)

	added := strings.Join(add, "&")
	if base == "" {
		u.RawQuery = added
	} else {
		u.RawQuery = base + "&" + added
	}
	// RawQuery is written verbatim by URL.String(), so the original QUERY — tokens, ordering
	// and separators included — survives untouched. The PATH still needs restoring: String()
	// re-escapes it, so a token there comes back as %7B%7B…%7D%7D.
	return restoreTemplateTokens(u.String(), rawURL)
}

// templateToken matches a HubSpot personalization token: {{contact.firstname}}, {{ event.slug }}.
var templateToken = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// schemeOf returns the leading "scheme://" of a url, or "" if there is none.
func schemeOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		return u[:i+3]
	}
	return ""
}

// sameExceptSchemeCase reports whether two urls are identical once the SCHEME is compared
// case-insensitively.
//
// url.Parse lower-cases the scheme, so a template written "HTTPS://host/{{contact.id}}" produces
// a tagged url whose scheme no longer matches the original byte-for-byte. A plain == then fails,
// the path restore is skipped, and the token ships as %7B%7Bcontact.id%7D%7D — a link HubSpot
// never expands. Schemes are case-insensitive per RFC 3986 §3.1; the rest of the url is not, so
// only this segment is folded.
func sameExceptSchemeCase(a, b string) bool {
	as, bs := schemeOf(a), schemeOf(b)
	return strings.EqualFold(as, bs) && a[len(as):] == b[len(bs):]
}

// restoreTemplateTokens undoes url.URL's percent-encoding of personalization tokens in the PATH
// and the FRAGMENT.
//
// HubSpot substitutes {{...}} at SEND time, and URL.String() re-escapes the path, so a token
// there comes back as %7B%7B…%7D%7D and HubSpot never recognises it — every personalized link
// in the email breaks. Tagging a link must not change where it goes.
//
// The QUERY is not touched here: it is appended verbatim (see Apply), so its tokens never get
// escaped in the first place. That distinction matters — restoring by VALUE across the whole
// URL was wrong when the same token appeared both live and pre-encoded, because the restore
// could not tell the two occurrences apart.
//
// The FRAGMENT needs the same treatment for the same reason: URL.String() escapes it too, so
// "#{{contact.id}}" shipped as "#%7B%7Bcontact.id%7D%7D". Path and fragment are restored
// INDEPENDENTLY — a url can personalize either one alone, and gating the fragment on the path
// having tokens (or vice versa) would leave the other broken.
//
// Only tokens present in the ORIGINAL url are restored, so this cannot introduce a token the
// author did not write.
func restoreTemplateTokens(tagged, original string) string {
	// Restore the ORIGINAL path wholesale rather than replacing token occurrences.
	//
	// Occurrence-by-occurrence replacement cannot work: URL.String() encodes an
	// already-encoded literal and a live token IDENTICALLY, so
	// `/%7B%7Bcontact.id%7D%7D/{{contact.id}}` produced two matching needles and a first-match
	// replace revived the wrong one — swapping which occurrence was live.
	//
	// Splicing the original path back is exact by construction: the path is not something this
	// function modifies, only something URL.String() re-escaped on the way out.
	// Strip the FRAGMENT before the query: a url with a fragment and no query
	// ("/path/{{tok}}#agenda") would otherwise keep "#agenda" inside the "path", the unescape
	// comparison would never match, and the restore would silently skip — leaving the token
	// percent-encoded and the personalized link broken at send time.
	origNoFrag, origFragment, origHasFragment := strings.Cut(original, "#")
	origPath, _, _ := strings.Cut(origNoFrag, "?")

	// Split the TAGGED url the same way.
	taggedNoFrag, taggedFragment, taggedHasFragment := strings.Cut(tagged, "#")
	taggedPath, taggedQuery, hasQuery := strings.Cut(taggedNoFrag, "?")

	// Restore each part independently: a url may personalize the path, the fragment, or both.
	path := taggedPath
	if templateToken.MatchString(origPath) {
		// Only splice when the two differ purely by ESCAPING. Comparing their unescaped forms is
		// the check — but unescape BOTH: the original may already contain percent-escapes the
		// author wrote deliberately, and comparing a decoded tagged path against a raw original
		// would then never match, silently skipping the restore.
		taggedPlain, terr := url.PathUnescape(taggedPath)
		origPlain, oerr := url.PathUnescape(origPath)
		if terr == nil && oerr == nil && sameExceptSchemeCase(taggedPlain, origPlain) {
			// Splice the TAGGED scheme, not the original: url.Parse normalized it, and
			// re-introducing "HTTPS://" would undo that normalization for no benefit. Only the
			// path is recovered — everything URL.String() legitimately canonicalized stays so.
			path = schemeOf(taggedPath) + origPath[len(schemeOf(origPath)):]
		}
	}

	fragment, hasFragment := taggedFragment, taggedHasFragment
	if origHasFragment && templateToken.MatchString(origFragment) {
		// Same escaping-only test as the path, using the same decoder. net/url exports no
		// FragmentUnescape — url.PathUnescape is the general "%XX except +" decoder and is what
		// net/url itself uses for fragments (setFragment calls unescape(..., encodeFragment),
		// which, like a path, leaves "+" alone). QueryUnescape would be wrong here: it turns "+"
		// into a space, so "#a+b" would decode to "a b" on one side only and the restore would
		// silently skip.
		taggedPlain, terr := url.PathUnescape(taggedFragment)
		origPlain, oerr := url.PathUnescape(origFragment)
		if terr == nil && oerr == nil && taggedPlain == origPlain {
			fragment, hasFragment = origFragment, true
		}
	}

	out := path
	if hasQuery {
		out += "?" + taggedQuery
	}
	if hasFragment {
		out += "#" + fragment
	}
	return out
}

// stripUTM removes EVERY utm_* pair, preserving all other pairs verbatim (position and exotic
// separators included).
//
// All of them, not just blank ones: appending duplicates leaves the ORIGINAL values first, and
// url.Values.Get — what most readers use — returns the first. Tagging
// `?utm_source=facebook&utm_medium=cpc` would report facebook/cpc while the appended email
// values sat behind them, so the link looked tagged and attributed to the wrong channel.
//
// Safe because Apply has already returned unchanged for any link carrying a real utm_campaign
// (the never-retag guard): anything reaching here is being tagged, so a leftover partial tag
// from a previous pass is not an author's deliberate value.
func stripUTM(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	// Split on BOTH separators. Splitting only on "&" leaves `a=1;utm_campaign=x` as a single
	// opaque part whose key is "a", so the utm_ pair inside it survives the strip — and the
	// freshly-appended utm_campaign then lands NEXT TO the stale one, leaving two conflicting
	// values in the URL for analytics to choose between.
	//
	// Every KEPT part is re-emitted with the separator that ORIGINALLY PRECEDED it, so a value
	// that legitimately contains ";" survives byte-identical. Re-joining on "&" instead shredded
	// it: `?sig=a;b;c&utm_term=old` became `sig=a&b&c&utm_…`, silently turning one signature into
	// three empty-valued parameters. The link still resolved, so the destination just saw a
	// truncated signature — worse than the untagged link this package exists to avoid.
	// Signatures, base64 payloads, `redirect=` targets and ad-tracker macros all routinely carry
	// unencoded semicolons.
	//
	// PRECEDING, not trailing. Keying off the byte that FOLLOWED each kept part looks equivalent
	// and is not: when a utm_ pair sits BETWEEN two kept parts, the survivor inherits a separator
	// that belonged to the removed pair. `a=1;utm_source=fb&b=2` collapsed to `a=1;b=2`, and
	// url.ParseQuery has rejected ";" since Go 1.17 — so it returns an EMPTY map and `b` is lost
	// outright, not merely merged into `a`. A part's own leading byte always belongs to that part,
	// so dropping any number of neighbours can never reassign it.
	if !hasUTMPair(rawQuery) {
		return rawQuery
	}
	var b strings.Builder
	b.Grow(len(rawQuery))
	for _, part := range splitQuery(rawQuery) {
		if strings.HasPrefix(queryKey(part.token), "utm_") {
			continue
		}
		// Skip the separator only for the FIRST part actually emitted: a leading "&"/";" would
		// otherwise appear when the original first pair was a utm_ one that got stripped.
		if b.Len() > 0 {
			b.WriteByte(part.sep)
		}
		b.WriteString(part.token)
	}
	return b.String()
}

// queryPart is one raw query token plus the separator byte that PRECEDED it in the original query
// ('&' or ';'; 0 for the first part, which has nothing before it).
type queryPart struct {
	token string
	sep   byte
}

// splitQuery splits a raw query on BOTH "&" and ";", keeping each part's LEADING separator so a
// caller can re-emit a subset of the parts without reshaping the ones it keeps.
//
// url.Query() cannot be used anywhere this package inspects a query: Go 1.17+ dropped ";" as a
// separator, so it silently DISCARDS semicolon-delimited pairs rather than reporting them. A
// utm_ pair hidden in one is invisible to a Values-based check while remaining very much present
// in the URL that gets sent.
//
// The separator is recorded as the byte immediately BEFORE the token rather than after it, so it
// stays bound to the part it actually delimits no matter which neighbours a caller drops. Empty
// parts are skipped (as strings.FieldsFunc did), so "a=1&&b=2" yields two parts; the second's
// leading separator is the "&" adjacent to it.
func splitQuery(rawQuery string) []queryPart {
	var out []queryPart
	start := 0
	for i := 0; i <= len(rawQuery); i++ {
		if i < len(rawQuery) && rawQuery[i] != '&' && rawQuery[i] != ';' {
			continue
		}
		if i > start {
			var sep byte
			if start > 0 {
				sep = rawQuery[start-1]
			}
			out = append(out, queryPart{token: rawQuery[start:i], sep: sep})
		}
		start = i + 1
	}
	return out
}

// queryKey returns a part's key, PERCENT-DECODED.
//
// Keys must be decoded before any comparison. `?utm%5Fcampaign=hand-picked` decodes to
// `utm_campaign` for every normal query reader (and for the analytics backend), but a raw
// comparison sees the literal "utm%5Fcampaign": the never-retag guard missed it and Apply
// appended a SECOND campaign, while the strip left the author's original in place ahead of it —
// two conflicting utm_campaign values in one link, with the author's the one most readers pick.
//
// An undecodable key falls back to its raw form rather than being dropped: a malformed escape is
// not a reason to stop seeing the parameter.
func queryKey(part string) string {
	k, _, _ := strings.Cut(part, "=")
	if decoded, err := url.QueryUnescape(k); err == nil {
		return decoded
	}
	return k
}

// rawQueryValues returns every value for key in a raw query, across both separators. Unlike
// Values.Get it returns ALL of them, so a guard cannot be slipped past with a leading empty pair.
func rawQueryValues(rawQuery, key string) []string {
	var out []string
	for _, part := range splitQuery(rawQuery) {
		_, v, _ := strings.Cut(part.token, "=")
		if queryKey(part.token) != key {
			continue
		}
		// Decode before comparing: a value that arrives percent-encoded ("%20") is still a
		// deliberate campaign, and treating it as empty would defeat the guard.
		if decoded, err := url.QueryUnescape(v); err == nil {
			out = append(out, decoded)
			continue
		}
		out = append(out, v)
	}
	return out
}

// hasUTMPair reports whether a raw query contains any utm_ pair, across both separators.
func hasUTMPair(rawQuery string) bool {
	for _, part := range splitQuery(rawQuery) {
		if strings.HasPrefix(queryKey(part.token), "utm_") {
			return true
		}
	}
	return false
}

// isTaggable reports whether a link is a web destination that may carry UTM parameters.
//
// It runs BEFORE url.Parse, on the raw string, because a link this rejects must come back
// byte-identical — round-tripping "MAILTO:A@B.dev" through url.Parse/String would rewrite it
// even though nothing was tagged.
//
// Scheme comparison is case-insensitive (RFC 3986 §3.1 — "MAILTO:" is as valid as "mailto:" in
// HTML), and a scheme is only recognised when the colon comes before any '/', '?' or '#': in
// "/path:x" and "?a=b:c" the colon is data, not a scheme delimiter, and treating it as one would
// wrongly reject an ordinary relative link.
func isTaggable(rawURL string) bool {
	s := strings.TrimSpace(rawURL)
	if strings.HasPrefix(s, "#") {
		// A bare fragment is an in-page jump, not a destination; a query on it goes nowhere.
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ':':
			// A scheme must be non-empty; ":foo" is not one.
			return i > 0 && taggableSchemes[strings.ToLower(s[:i])]
		case '/', '?', '#':
			return true // scheme-relative or relative: eligible.
		}
	}
	return true // no scheme delimiter at all: a bare relative path.
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
