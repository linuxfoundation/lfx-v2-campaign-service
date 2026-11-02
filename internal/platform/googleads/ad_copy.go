// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"errors"
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
// LINE_TOO_WIDE — callerCopy refuses caller text over these weights (textWeight), and
// truncateWeighted cuts only the service's own default lines to them.
const (
	minHeadlines         = 3
	maxHeadlines         = 15
	maxHeadlineWeight    = 30
	minDescriptions      = 2
	maxDescriptions      = 4
	maxDescriptionWeight = 90
	maxErrorComponentLen = 100 // cap caller-controlled URL components in error messages
)

// adTextAsset is one headline/description entry in a responsiveSearchAd.
// consumed by the ad group/ad cascade (adgroup_ad.go), which lands in the
// next stacked PR (GA-3b) and is not yet merged into this branch.
//
//nolint:unused // GA-3b dependency explained above.
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

// composeAdCopy resolves the caller-supplied headlines/descriptions (if any) into a valid
// Responsive Search Ad content set, and says in notes everything it changed (#441).
//
// Caller copy is never cut: a line over Google's display width (see googleAdsCharWeight —
// CJK/full-width runes count double) is REFUSED here, in the pure preflight before the budget
// mutate, as the Display, Demand Gen and Performance Max creatives refuse theirs. Google would
// refuse it anyway, after the campaign exists.
//
// Three changes are still made, and each is reported in notes for the result's steps:
//   - a duplicate line (Google refuses one within an ad) is dropped;
//   - lines past the count cap (15 headlines, 4 descriptions) are dropped. lfx-self-serve's
//     Add buttons have no cap, so refusing would fail a create that works today. The walk
//     stops at the cap, so an over-long list costs one note, not one per extra line;
//   - a set below Google's minimum (3 headlines, 2 descriptions) is padded with
//     deterministic lines derived from eventName. Refusing it instead would fail every
//     Implement-tab create with one headline, which lfx-self-serve does not prevent.
//
// Blank lines are skipped without a note: an empty form row is not copy. A caller with
// no usable text and an empty eventName is a hard error — there is nothing to advertise.
func composeAdCopy(callerHeadlines, callerDescriptions []string, eventName, project string) (adPlan, error) {
	var notes []string
	headlines, err := callerCopy("headline", callerHeadlines, maxHeadlineWeight, maxHeadlines, &notes)
	if err != nil {
		return adPlan{}, err
	}
	descriptions, err := callerCopy("description", callerDescriptions, maxDescriptionWeight, maxDescriptions, &notes)
	if err != nil {
		return adPlan{}, err
	}

	headlines = padUnique("headline", headlines, defaultHeadlines(eventName), maxHeadlineWeight, minHeadlines, maxHeadlines, &notes)
	descriptions = padUnique("description", descriptions, defaultDescriptions(eventName, project), maxDescriptionWeight, minDescriptions, maxDescriptions, &notes)

	if len(headlines) < minHeadlines {
		return adPlan{}, fmt.Errorf("google-ads ad requires at least %d usable headline(s), got %d (need a non-empty eventName or caller-supplied headlines)", minHeadlines, len(headlines))
	}
	if len(descriptions) < minDescriptions {
		return adPlan{}, fmt.Errorf("google-ads ad requires at least %d usable description(s), got %d (need a non-empty eventName or at least %d caller-supplied description(s))", minDescriptions, len(descriptions), minDescriptions)
	}
	return adPlan{headlines: headlines, descriptions: descriptions, copyNotes: notes}, nil
}

// callerCopy trims the caller's lines, skips blanks, refuses a line over maxWeight, drops an
// exact duplicate, and stops at maxCount. Each kind of drop is ONE note naming the first line
// dropped, so the steps stay bounded however long the caller's list is.
func callerCopy(label string, candidates []string, maxWeight, maxCount int, notes *[]string) ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	dups, firstDup := 0, ""
	for _, raw := range candidates {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if len(out) >= maxCount {
			// Past the cap nothing is sent, so nothing past it is measured or hashed.
			*notes = append(*notes, fmt.Sprintf("dropped every %s after the %d Google takes, from %q on", label, maxCount, capForError(s)))
			break
		}
		if w := textWeight(s); w > maxWeight {
			return nil, fmt.Errorf("google-ads search ad %s %q has a display width of %d, exceeding the %d limit (CJK and full-width characters count double); shorten it", label, capForError(s), w, maxWeight)
		}
		if _, dup := seen[s]; dup {
			if dups == 0 {
				firstDup = s
			}
			dups++
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if dups > 0 {
		*notes = append(*notes, fmt.Sprintf("dropped %d duplicate %s(s), first %q", dups, label, firstDup))
	}
	return out, nil
}

// padUnique appends entries from fallback (already-ordered candidates) to base until base
// reaches min entries or fallback is exhausted, skipping any fallback entry that duplicates
// one already present (post-truncation), and notes each line it adds. Only these
// service-written defaults are ever truncated. The result is capped at max.
func padUnique(label string, base, fallback []string, maxWeight, min, max int, notes *[]string) []string {
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
		*notes = append(*notes, fmt.Sprintf("added default %s %q (Google needs at least %d)", label, s, min))
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
// ad. eventName is sanitized to remove control characters and normalize
// whitespace before use. Order matters: padUnique consumes these in order until
// the minimum is reached.
func defaultHeadlines(eventName string) []string {
	eventName = sanitizeNamePart(eventName)
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
// Both eventName and project are sanitized to remove control characters and
// normalize whitespace before use.
func defaultDescriptions(eventName, project string) []string {
	eventName = sanitizeNamePart(eventName)
	project = sanitizeNamePart(project)
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

// capForError truncates s to maxErrorComponentLen characters for safe inclusion in
// persisted error messages, adding "…" if truncated.
//
// The cut lands on a RUNE boundary, not a byte one. Callers include fields this client
// validates by DISPLAY WIDTH rather than byte length — business names, headlines,
// descriptions — so multibyte text is expected there, and a byte-slice at a fixed offset
// would write a half rune into a message that persists unencrypted as a Steps entry.
func capForError(s string) string {
	if len(s) <= maxErrorComponentLen {
		return s
	}
	cut := maxErrorComponentLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// buildAdFinalURL builds the ad's destination URL from the brief's
// registration URL, tagging it with UTM parameters for attribution. utm_source
// and utm_medium are set unconditionally to reflect the Google CPC channel,
// overwriting any existing values; other utm_* parameters are preserved if
// already present (mirrors the microsoft client's final-URL builder to avoid
// misattribution).
func buildAdFinalURL(registrationURL, eventSlug, eventName, project, nameSuffix string) (string, error) {
	return buildTaggedFinalURL("registration URL", registrationURL, eventSlug, eventName, project, nameSuffix)
}

// buildTaggedFinalURL is buildAdFinalURL with the noun its error messages use
// made a parameter. Sitelinks are ad destinations too — a click on one is an ad
// click and must attribute to google/cpc exactly like the headline's — so they
// need this builder verbatim, and only the wording of a validation failure
// differs. Keeping one implementation is what stops sitelink URLs from drifting
// into a second, laxer set of checks.
func buildTaggedFinalURL(noun, registrationURL, eventSlug, eventName, project, nameSuffix string) (string, error) {
	u, err := validateServableURL(noun, registrationURL)
	if err != nil {
		return "", err
	}

	campaign := sanitizeNamePart(eventSlug)
	if campaign == "" {
		campaign = sanitizeNamePart(eventName)
	}
	if campaign == "" {
		campaign = sanitizeNamePart(nameSuffix)
	}

	q := u.Query()
	// Set utm_source and utm_medium unconditionally: a click from a Google CPC ad
	// must be attributed to Google/CPC, never to an earlier channel's utm_source
	// from the caller-supplied URL (which would misattribute the ad spend).
	q.Set("utm_source", "google")
	q.Set("utm_medium", "cpc")
	// Preserve existing utm_campaign and utm_content if already present.
	if campaign != "" {
		setIfAbsent(q, "utm_campaign", campaign)
	}
	if p := sanitizeNamePart(project); p != "" {
		setIfAbsent(q, "utm_content", p)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateServableURL is the half of buildTaggedFinalURL that decides whether a
// caller-supplied URL is one this client is willing to send at all, split out so
// a URL that must NOT be UTM-tagged still passes exactly the same checks.
//
// The lead form's privacy-policy URL is the case that needs it: it is a link
// Google renders inside the form, not an ad destination, so tagging it with this
// campaign's utm_* parameters would attribute a privacy-policy read as an ad
// click and could break a URL whose query the policy host parses itself. What it
// must still be is servable and secret-free, and that is this function. Keeping
// one implementation is what stops a second, laxer set of URL checks appearing.
//
// Returns the PARSED url so the tagging caller does not parse twice.
func validateServableURL(noun, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is empty", noun)
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Do NOT echo the raw URL or wrap err (both may carry secrets in
		// userinfo/query/fragment) — this message and its wrapped url.Error
		// can both be logged or persisted in a result step/snapshot.
		return nil, fmt.Errorf("%s %q is not a valid URL", noun, redactURLForError(raw))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%s %q must be http(s), got scheme %q", noun, redactURLForError(raw), capForError(u.Scheme))
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%s %q has no host", noun, redactURLForError(raw))
	}
	// Reject embedded userinfo (user[:password]@host): an ad destination never
	// needs URL credentials, and forwarding them downstream would leak a
	// basic-auth secret. Mirrors the twitter/reddit/meta clients' validators.
	if u.User != nil {
		return nil, fmt.Errorf("%s %q must not contain embedded credentials (userinfo)", noun, redactURLForError(raw))
	}
	// Validate the query before anything merges into it: a malformed
	// percent-escape in RawQuery is silently dropped by u.Query(), which would
	// alter the destination the ad actually points to.
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return nil, fmt.Errorf("%s %q has a malformed query string", noun, redactURLForError(raw))
	}
	return u, nil
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
// userinfo/query/fragment that can hold secrets. The returned URL is capped at
// maxErrorComponentLen to prevent unbounded caller-controlled URL components
// from bloating persisted errors. A value that can't be parsed as an absolute
// URL is reported as an opaque placeholder rather than echoed raw.
func redactURLForError(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		if err == nil && u.Scheme != "" && u.Host != "" {
			redacted := u.Scheme + "://" + u.Host
			return capForError(redacted)
		}
		return "(redacted)"
	}
	redacted := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	return capForError(redacted)
}

// redactedCause renders a transport error WITHOUT the request URL, for wrapping the
// cause of a fetch against a caller-supplied address.
//
// net/http's Client.Do, and http.NewRequestWithContext, return a *url.Error whose
// Error() prints the full request URL — query string included, since only a password
// in userinfo is stripped. Wrapping one with %w therefore puts the caller's URL into
// the message a second time and into whatever persists it, which for this service is
// an unencrypted Steps entry. Redacting the URL in the format arguments while handing
// %w the raw cause redacts nothing at all.
//
// The chain stays reachable: Unwrap returns the original, so errors.Is/As — and the
// ambiguity classification that keys on them — behave exactly as before. Only the
// rendered text changes.
type redactedCause struct{ err error }

func (r redactedCause) Error() string {
	if r.err == nil {
		return "(redacted)"
	}
	var ue *url.Error
	if errors.As(r.err, &ue) {
		// The operation and the inner cause are what diagnose the failure; the URL is
		// the part that can hold a secret, and the caller already has it.
		return capForError(ue.Op + ": " + redactedCause{ue.Err}.Error())
	}
	return capForError(r.err.Error())
}

func (r redactedCause) Unwrap() error { return r.err }
