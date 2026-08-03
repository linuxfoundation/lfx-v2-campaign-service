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
	if strings.TrimSpace(q.Get("utm_campaign")) != "" {
		return rawURL
	}

	q.Set("utm_source", orDefault(p.Source, DefaultSource))
	q.Set("utm_medium", orDefault(p.Medium, DefaultMedium))
	q.Set("utm_campaign", p.Campaign)
	if content != "" {
		q.Set("utm_content", content)
	}
	if p.Term != "" {
		q.Set("utm_term", p.Term)
	}
	u.RawQuery = q.Encode()
	return u.String()
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
