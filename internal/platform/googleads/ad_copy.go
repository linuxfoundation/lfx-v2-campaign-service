// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Responsive Search Ad copy generation and destination-URL building for GA-3
// (see adgroup_ad.go for the ad group/ad creation cascade that consumes
// these). Pure functions, no network calls — kept separate so the ad-group/ad
// mutate cascade in adgroup_ad.go can be reviewed independently of the copy
// derivation rules here.
// ---------------------------------------------------------------------------

// Responsive Search Ad content bounds (Google Ads v23 System Limits), expressed
// in Google's double-width character WEIGHT, not plain rune count: Google Ads
// Help and Advertising Policies count CJK/full-width characters as 2 toward
// headline/description limits (effective 15/45 for CJK-heavy text), the same
// doubling Microsoft documents explicitly. Google's API does not surface this,
// so text that fits by plain rune count can still be rejected upstream with
// LINE_TOO_WIDE — see googleAdsCharWeight/truncateWeighted.
const (
	minHeadlines         = 3
	maxHeadlines         = 15
	maxHeadlineWeight    = 30
	minDescriptions      = 2
	maxDescriptions      = 4
	maxDescriptionWeight = 90
)

// adTextAsset is one headline/description entry in a responsiveSearchAd.
//
// next stacked PR (GA-3b) and is not yet merged into this branch.
//
//nolint:unused // consumed by the ad group/ad cascade (adgroup_ad.go), which lands in the
type adTextAsset struct {
	Text string `json:"text"`
}

// textAssets wraps plain strings as the {"text": ...} shape RSA headlines/
// descriptions require.
//
//nolint:unused // see adTextAsset above.
func textAssets(ss []string) []adTextAsset {
	out := make([]adTextAsset, len(ss))
	for i, s := range ss {
		out[i] = adTextAsset{Text: s}
	}
	return out
}

// composeAdCopy resolves the caller-supplied headlines/descriptions (if any)
// into a valid Responsive Search Ad content set: each entry trimmed and
// weight-capped to its limit (see truncateWeighted — CJK/full-width runes
// count double), empties dropped, and duplicates (after trimming)
// removed — Google rejects both an over-limit asset and a duplicate one within
// the same ad. If fewer than the minimum survive, deterministic placeholders
// derived from eventName are appended (never removed) until the minimum is
// met; the result is also capped at the maximum count. An eventName so long
// none of its truncations are useful should not happen in practice (EventName
// is capped upstream via the campaign name validation), but a caller supplying
// zero usable text and an empty eventName is a hard error — there is nothing
// to advertise.
func composeAdCopy(callerHeadlines, callerDescriptions []string, eventName, project string) (headlines, descriptions []string, err error) {
	headlines = boundedUniqueCopy(callerHeadlines, maxHeadlineWeight, maxHeadlines)
	descriptions = boundedUniqueCopy(callerDescriptions, maxDescriptionWeight, maxDescriptions)

	headlines = padUnique(headlines, defaultHeadlines(eventName), maxHeadlineWeight, minHeadlines, maxHeadlines)
	descriptions = padUnique(descriptions, defaultDescriptions(eventName, project), maxDescriptionWeight, minDescriptions, maxDescriptions)

	if len(headlines) < minHeadlines {
		return nil, nil, fmt.Errorf("google-ads ad requires at least %d usable headline(s), got %d (need a non-empty eventName or caller-supplied headlines)", minHeadlines, len(headlines))
	}
	if len(descriptions) < minDescriptions {
		return nil, nil, fmt.Errorf("google-ads ad requires at least %d usable description(s), got %d (need a non-empty eventName or caller-supplied descriptions)", minDescriptions, len(descriptions))
	}
	return headlines, descriptions, nil
}

// boundedUniqueCopy trims each candidate, weight-truncates it to maxWeight
// (see truncateWeighted), drops empties, de-duplicates (case-sensitive,
// post-truncation), and caps the result at maxCount entries.
func boundedUniqueCopy(candidates []string, maxWeight, maxCount int) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		s = truncateWeighted(s, maxWeight)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= maxCount {
			break
		}
	}
	return out
}

// padUnique appends entries from fallback (already-ordered candidates) to base
// until base reaches min entries or fallback is exhausted, skipping any
// fallback entry that duplicates one already present (post-truncation). The
// result is capped at max.
func padUnique(base, fallback []string, maxWeight, min, max int) []string {
	seen := map[string]struct{}{}
	for _, s := range base {
		seen[s] = struct{}{}
	}
	for _, s := range fallback {
		if len(base) >= min || len(base) >= max {
			break
		}
		s = strings.TrimSpace(s)
		s = truncateWeighted(s, maxWeight)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		base = append(base, s)
	}
	return base
}

// truncateRunes cuts s to at most n runes (not bytes), so a multibyte
// character is never split mid-encoding.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// googleAdsCharWeight returns how much r counts toward an RSA headline/
// description limit. Google Ads treats full-width and CJK characters as
// weight 2 (Google Ads Help / Advertising Policies); everything else is
// weight 1. The ranges cover Hangul Jamo, the CJK Unified Ideographs block
// and its extensions/compatibility forms, Hangul Syllables, and the
// Halfwidth/Fullwidth Forms block.
func googleAdsCharWeight(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x11FF, // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF,   // CJK Radicals Supplement .. Yi Radicals
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul Syllables
		r >= 0xF900 && r <= 0xFAFF,   // CJK Compatibility Ideographs
		r >= 0xFE30 && r <= 0xFE4F,   // CJK Compatibility Forms
		r >= 0xFF00 && r <= 0xFF60,   // Fullwidth Forms
		r >= 0xFFE0 && r <= 0xFFE6,   // Fullwidth Signs
		r >= 0x20000 && r <= 0x3FFFD: // CJK Unified Ideographs Extension B-G
		return 2
	}
	return 1
}

// truncateWeighted cuts s to at most maxWeight of Google Ads' double-width
// character weight (googleAdsCharWeight), never splitting a multibyte rune
// mid-encoding and never leaving a result whose weight exceeds maxWeight —
// unlike a plain rune-count truncation, which lets a run of CJK/full-width
// characters through at double their effective cost and risks an upstream
// LINE_TOO_WIDE rejection.
func truncateWeighted(s string, maxWeight int) string {
	weight := 0
	for i, r := range s {
		w := googleAdsCharWeight(r)
		if weight+w > maxWeight {
			return s[:i]
		}
		weight += w
	}
	return s
}

// defaultHeadlines derives deterministic placeholder headlines from the event
// name so a caller that supplies none still gets a valid, non-generic-sounding
// ad. Order matters: padUnique consumes these in order until the minimum is
// reached.
func defaultHeadlines(eventName string) []string {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		return nil
	}
	return []string{
		eventName,
		"Register for " + eventName,
		"Join " + eventName + " Today",
		"Save Your Spot Now",
		"Learn More & Register",
	}
}

// defaultDescriptions mirrors defaultHeadlines for the description slots.
func defaultDescriptions(eventName, project string) []string {
	eventName = strings.TrimSpace(eventName)
	project = strings.TrimSpace(project)
	if eventName == "" {
		return nil
	}
	var out []string
	if project != "" {
		out = append(out, fmt.Sprintf("%s is happening soon, hosted by %s. Reserve your spot today.", eventName, project))
	}
	out = append(out,
		fmt.Sprintf("Don't miss %s. Register now to secure your place.", eventName),
		"Connect with the community. Registration is open now.",
	)
	return out
}

// buildAdFinalURL builds the ad's destination URL from the brief's
// registration URL, tagging it with UTM parameters for attribution. Existing
// query parameters on the registration URL are preserved; a utm_* key already
// present is left untouched rather than overwritten (mirrors the reddit/meta/
// twitter/microsoft clients' final-URL builders).
func buildAdFinalURL(registrationURL, eventSlug, eventName, project, nameSuffix string) (string, error) {
	registrationURL = strings.TrimSpace(registrationURL)
	if registrationURL == "" {
		return "", fmt.Errorf("registration URL is empty")
	}
	u, err := url.Parse(registrationURL)
	if err != nil {
		// Do NOT echo the raw URL or wrap err (both may carry secrets in
		// userinfo/query/fragment) — this message and its wrapped url.Error
		// can both be logged or persisted in a result step/snapshot.
		return "", fmt.Errorf("registration URL %q is not a valid URL", redactURLForError(registrationURL))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("registration URL %q must be http(s), got scheme %q", redactURLForError(registrationURL), u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("registration URL %q has no host", redactURLForError(registrationURL))
	}
	// Reject embedded userinfo (user[:password]@host): an ad destination never
	// needs URL credentials, and forwarding them downstream would leak a
	// basic-auth secret. Mirrors the twitter/reddit/meta clients' validators.
	if u.User != nil {
		return "", fmt.Errorf("registration URL %q must not contain embedded credentials (userinfo)", redactURLForError(registrationURL))
	}
	// Validate the existing query before merging in utm_* params: a malformed
	// percent-escape in RawQuery is silently dropped by u.Query(), which would
	// alter the destination the ad actually points to.
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return "", fmt.Errorf("registration URL %q has a malformed query string", redactURLForError(registrationURL))
	}

	campaign := sanitizeNamePart(eventSlug)
	if campaign == "" {
		campaign = sanitizeNamePart(eventName)
	}
	if campaign == "" {
		campaign = sanitizeNamePart(nameSuffix)
	}

	q := u.Query()
	setIfAbsent(q, "utm_source", "google")
	setIfAbsent(q, "utm_medium", "cpc")
	if campaign != "" {
		setIfAbsent(q, "utm_campaign", campaign)
	}
	if p := sanitizeNamePart(project); p != "" {
		setIfAbsent(q, "utm_content", p)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// setIfAbsent sets key=value in q only when key is not already present, so a
// registration URL that already carries its own utm_* tagging is not
// overwritten.
func setIfAbsent(q url.Values, key, value string) {
	if q.Has(key) {
		return
	}
	q.Set(key, value)
}

// redactURLForError reduces a caller-supplied registration URL to
// scheme+host+path for inclusion in a validation error message, so the error
// (which may be logged or persisted in a step/snapshot) never carries
// userinfo/query/fragment that can hold secrets. A value that can't be parsed
// as an absolute URL is reported as an opaque placeholder rather than echoed
// raw. Identical to redactURLForError in the twitter client (the reddit/meta
// clients' redactURL uses more permissive fallback heuristics — it strips a
// trailing "?"/"#" and echoes the rest rather than falling back to a fixed
// placeholder).
func redactURLForError(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		if err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
		return "(redacted)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}
