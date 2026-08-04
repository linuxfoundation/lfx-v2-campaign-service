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
	q := u.Query()
	// Check EVERY utm_campaign value, not just the first. Values.Get returns only the first,
	// so `?utm_campaign=&utm_campaign=hand-picked` would pass the guard and then Set would
	// DELETE the author's deliberate campaign — the exact silent overwrite this guard exists
	// to prevent, hidden behind a leading empty value.
	for _, v := range q["utm_campaign"] {
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

	// Drop any BLANK utm_* pairs already present before appending. A `?utm_campaign=` is not a
	// real tag (the guard above lets it through for exactly that reason), but leaving it in
	// place would make the FIRST value empty and readers that take the first value would see
	// nothing — the appended real value would never win.
	base := stripBlankUTM(u.RawQuery)

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

// restoreTemplateTokens undoes url.URL's percent-encoding of personalization tokens IN THE PATH.
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
// Only tokens present in the ORIGINAL PATH are restored, so this cannot introduce a token the
// author did not write.
func restoreTemplateTokens(tagged, original string) string {
	// Scope to the path: everything before the first "?" in the ORIGINAL.
	origPath := original
	if i := strings.IndexByte(origPath, '?'); i >= 0 {
		origPath = origPath[:i]
	}
	tokens := templateToken.FindAllString(origPath, -1)
	for _, tok := range tokens {
		escaped := (&url.URL{Path: tok}).EscapedPath()
		if escaped != tok && strings.Contains(tagged, escaped) {
			tagged = strings.Replace(tagged, escaped, tok, 1)
			continue
		}
		// Query-position escaping differs from path escaping; try that form too.
		if q := url.QueryEscape(tok); q != tok && strings.Contains(tagged, q) {
			tagged = strings.Replace(tagged, q, tok, 1)
		}
	}
	return tagged
}

// stripBlankUTM removes utm_* pairs that carry no value, preserving every other pair verbatim
// (including its position and any exotic separator around it).
func stripBlankUTM(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	kept := parts[:0]
	for _, part := range parts {
		k, v, _ := strings.Cut(part, "=")
		if strings.HasPrefix(k, "utm_") && strings.TrimSpace(v) == "" {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "&")
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
