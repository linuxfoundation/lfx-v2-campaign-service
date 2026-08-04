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

// skipPrefixes are link schemes that must never be tagged. mailto:/tel: are not web
// destinations, and a bare fragment is an in-page anchor — appending query parameters to any
// of them produces a broken link rather than an attributed one.
var skipPrefixes = []string{"mailto:", "tel:", "#"}

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
//   - the link is a mailto:/tel:/fragment target;
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
	if hasSkipPrefix(rawURL) {
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
		// Same escaping-only test as the path. Fragments use FragmentUnescape, whose escaping
		// rules differ from a path's.
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
	// Re-joining with "&" is deliberate: "&" is the separator Go and every analytics backend
	// parse, so a rewritten query is strictly more likely to be read correctly than the
	// semicolon form it replaces. Only queries that actually CONTAIN a utm_ pair are rewritten;
	// a query with no utm_ pairs is returned byte-identical, semicolons intact (see the early
	// return below), so this cannot reshape a link it has no reason to touch.
	if !hasUTMPair(rawQuery) {
		return rawQuery
	}
	kept := make([]string, 0, 8)
	for _, part := range splitQuery(rawQuery) {
		k, _, _ := strings.Cut(part, "=")
		if strings.HasPrefix(k, "utm_") {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "&")
}

// splitQuery splits a raw query on BOTH "&" and ";".
//
// url.Query() cannot be used anywhere this package inspects a query: Go 1.17+ dropped ";" as a
// separator, so it silently DISCARDS semicolon-delimited pairs rather than reporting them. A
// utm_ pair hidden in one is invisible to a Values-based check while remaining very much present
// in the URL that gets sent.
func splitQuery(rawQuery string) []string {
	return strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' })
}

// rawQueryValues returns every value for key in a raw query, across both separators. Unlike
// Values.Get it returns ALL of them, so a guard cannot be slipped past with a leading empty pair.
func rawQueryValues(rawQuery, key string) []string {
	var out []string
	for _, part := range splitQuery(rawQuery) {
		k, v, _ := strings.Cut(part, "=")
		if k != key {
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
		if k, _, _ := strings.Cut(part, "="); strings.HasPrefix(k, "utm_") {
			return true
		}
	}
	return false
}

// hasSkipPrefix reports whether a link is one of the non-web targets that must not be tagged.
// The check is case-insensitive: "MAILTO:" is as valid as "mailto:" in HTML.
func hasSkipPrefix(rawURL string) bool {
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	for _, p := range skipPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
