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

// Responsive Search Ad content bounds (Google Ads v23 System Limits). Unlike
// Microsoft, Google does NOT halve these for double-width (CJK/emoji) text —
// the limits are plain rune counts.
const (
	minHeadlines        = 3
	maxHeadlines        = 15
	maxHeadlineRunes    = 30
	minDescriptions     = 2
	maxDescriptions     = 4
	maxDescriptionRunes = 90
)

// adTextAsset is one headline/description entry in a responsiveSearchAd.
type adTextAsset struct {
	Text string `json:"text"`
}

// textAssets wraps plain strings as the {"text": ...} shape RSA headlines/
// descriptions require.
func textAssets(ss []string) []adTextAsset {
	out := make([]adTextAsset, len(ss))
	for i, s := range ss {
		out[i] = adTextAsset{Text: s}
	}
	return out
}

// composeAdCopy resolves the caller-supplied headlines/descriptions (if any)
// into a valid Responsive Search Ad content set: each entry trimmed and
// rune-capped to its limit, empties dropped, and duplicates (after trimming)
// removed — Google rejects both an over-limit asset and a duplicate one within
// the same ad. If fewer than the minimum survive, deterministic placeholders
// derived from eventName are appended (never removed) until the minimum is
// met; the result is also capped at the maximum count. An eventName so long
// none of its truncations are useful should not happen in practice (EventName
// is capped upstream via the campaign name validation), but a caller supplying
// zero usable text and an empty eventName is a hard error — there is nothing
// to advertise.
func composeAdCopy(callerHeadlines, callerDescriptions []string, eventName, project string) (headlines, descriptions []string, err error) {
	headlines = boundedUniqueCopy(callerHeadlines, maxHeadlineRunes, maxHeadlines)
	descriptions = boundedUniqueCopy(callerDescriptions, maxDescriptionRunes, maxDescriptions)

	headlines = padUnique(headlines, defaultHeadlines(eventName), maxHeadlineRunes, minHeadlines, maxHeadlines)
	descriptions = padUnique(descriptions, defaultDescriptions(eventName, project), maxDescriptionRunes, minDescriptions, maxDescriptions)

	if len(headlines) < minHeadlines {
		return nil, nil, fmt.Errorf("google-ads ad requires at least %d usable headline(s), got %d (need a non-empty eventName or caller-supplied headlines)", minHeadlines, len(headlines))
	}
	if len(descriptions) < minDescriptions {
		return nil, nil, fmt.Errorf("google-ads ad requires at least %d usable description(s), got %d (need a non-empty eventName/project or caller-supplied descriptions)", minDescriptions, len(descriptions))
	}
	return headlines, descriptions, nil
}

// boundedUniqueCopy trims each candidate, rune-truncates it to maxRunes, drops
// empties, de-duplicates (case-sensitive, post-truncation), and caps the
// result at maxCount entries.
func boundedUniqueCopy(candidates []string, maxRunes, maxCount int) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		s = truncateRunes(s, maxRunes)
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
func padUnique(base, fallback []string, maxRunes, min, max int) []string {
	seen := map[string]struct{}{}
	for _, s := range base {
		seen[s] = struct{}{}
	}
	for _, s := range fallback {
		if len(base) >= min || len(base) >= max {
			break
		}
		s = truncateRunes(strings.TrimSpace(s), maxRunes)
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
		return "", fmt.Errorf("registration URL %q is not a valid URL: %w", registrationURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("registration URL %q must be http(s), got scheme %q", registrationURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("registration URL %q has no host", registrationURL)
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
