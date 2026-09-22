// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import "strings"

// ---------------------------------------------------------------------------
// Name-search predicates (LFXV2-2770)
//
// Every one of these exists because HubSpot's list search matches LOOSELY and
// returns at most 20 hits with no recency ordering. A search for
// "KubeCon Suppression" happily returns a different event's list, and accepting it
// would exclude exactly the people the send is for. So each tier of list is probed
// by a pattern and then accepted only against an explicit predicate — the probe
// finds candidates, the predicate decides.
//
// The probes and predicates are separated from the searching so both can be tested
// without a HubSpot client, and so the reason a candidate was REJECTED is
// inspectable rather than buried in a filter chain.
// ---------------------------------------------------------------------------

// SharesKeyword reports whether a name shares at least one content word with the
// event — the weakest honest link between a search hit and this event.
//
// An EMPTY keyword set matches nothing. This differs from IsPlausibleCandidate's
// callers on purpose: with no event identity there is no evidence, and treating
// "no evidence" as "matches" would accept every suppression list in the portal for
// an event we could not name.
func SharesKeyword(name string, keywords map[string]struct{}) bool {
	if len(keywords) == 0 {
		return false
	}
	return IsPlausibleCandidate(name, keywords)
}

// MatchesStandardSuppression accepts a hit for one of the fixed portfolio-wide
// hygiene lists.
//
// The search term must appear in the name verbatim. These names are fixed by
// convention ("LF Events GDPR Suppression"), so a looser test buys nothing and
// would let "LF Events Suppression List" satisfy the GDPR row — which an operator
// would then read as GDPR suppression being applied when it is not.
func MatchesStandardSuppression(name, searchTerm string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower(strings.TrimSpace(searchTerm)))
}

// BrandOptOutProbe is the search for a brand's own global opt-out list, e.g.
// "CNCF Global Opt Out". Empty when there is no brand to probe with.
func BrandOptOutProbe(brandShort string) string {
	if brand := strings.TrimSpace(brandShort); brand != "" {
		return brand + " Global Opt"
	}
	return ""
}

// BrandOptOutLabel is the operator-facing label for that list.
func BrandOptOutLabel(brandShort string) string {
	return strings.TrimSpace(brandShort) + " Global Opt-Outs"
}

// MatchesBrandOptOut requires both the brand and "opt" in the name. The brand
// alone would match the brand's own audience lists; "opt" alone would match
// another brand's.
func MatchesBrandOptOut(name, brandShort string) bool {
	brand := strings.ToLower(strings.TrimSpace(brandShort))
	if brand == "" {
		return false
	}
	lower := strings.ToLower(name)
	return strings.Contains(lower, brand) && strings.Contains(lower, "opt")
}

// EventSuppressionProbes are the searches for an event's own suppression list,
// carried over from a prior edition.
//
// The highest-value suppression pick when it exists: it already bundles that
// event's registrant and unsubscribe exclusions.
func EventSuppressionProbes(eventName string) []string {
	event := strings.TrimSpace(eventName)
	if event == "" {
		return nil
	}
	return []string{event + " Suppression", event + " Exclusion"}
}

// EventSuppressionLabel is the operator-facing label for that list.
func EventSuppressionLabel(eventName string) string {
	return strings.TrimSpace(eventName) + " Suppression"
}

// MatchesEventSuppression requires BOTH a keyword overlap with the event AND a
// suppression word in the name.
//
// Either test alone matches the event's own AUDIENCE lists — and excluding those
// would suppress exactly the people the send is for, producing an empty or
// near-empty audience that still looks like a successful build.
func MatchesEventSuppression(name string, keywords map[string]struct{}) bool {
	return SharesKeyword(name, keywords) && containsAny(name, suppressionNameHints)
}

// ExistingMasterProbes are the searches for master lists already built for this
// event. Two probes because the naming convention embeds the brand inconsistently.
func ExistingMasterProbes(brandShort, eventName string) []string {
	event := strings.TrimSpace(eventName)
	if event == "" {
		return nil
	}
	probes := []string{event + " Master"}
	if brand := strings.TrimSpace(brandShort); brand != "" {
		probes = append(probes, brand+" "+event+" Master")
	}
	return probes
}

// MatchesExistingMaster requires "master" in the name plus a keyword overlap, so
// an unrelated event's master list cannot appear as this event's.
func MatchesExistingMaster(name string, keywords map[string]struct{}) bool {
	return strings.Contains(strings.ToLower(name), "master") && SharesKeyword(name, keywords)
}

// ---------------------------------------------------------------------------
// Last-sent search terms
// ---------------------------------------------------------------------------

// StripYear removes every four-digit year from a search term.
//
// "KubeCon Europe 2026" finds only THIS year's emails — and this year's is exactly
// the one that has not been sent yet. Stripping the year is what surfaces last
// edition's send, which is the entire point of the last-sent section.
func StripYear(term string) string {
	return strings.Join(strings.Fields(yearRE.ReplaceAllString(term, " ")), " ")
}

// genericEventWords are event-name tokens too COMMON to admit a match on their own.
//
// They are not stopwords -- they carry real meaning and they still RANK (see
// LastSentMatch.Overlap). What they cannot do is establish that a candidate belongs to
// this event: "Summit Recap" shares "summit" with "Open Source Summit" and with a dozen
// unrelated portfolio sends, and an operator reads the last-sent panel as precedent for
// the audience they are about to build.
//
// Kept deliberately small, in the same shape as stopwords and suppressionNameHints. A
// token belongs here only when it recurs across DIFFERENT events in the portfolio.
var genericEventWords = map[string]struct{}{
	"summit": {}, "conference": {}, "con": {}, "forum": {}, "expo": {}, "festival": {},
	"world": {}, "international": {}, "annual": {}, "virtual": {}, "online": {},
	"day": {}, "days": {}, "north": {}, "america": {}, "europe": {}, "asia": {},
	"emea": {}, "apac": {}, "event": {}, "events": {}, "series": {},
}

// LastSentTerms is the token evidence a last-sent candidate is judged against.
//
// THREE tiers rather than one search string, because the search string was the bug. A
// marketing email named "KubeCon NA 2026 - Registration Open" does not contain the phrase
// "KubeCon + CloudNativeCon North America" anywhere, so matching the event name as one
// contiguous term found nothing at all -- for an event with a full history of sends. What
// identifies the email is that it shares the event's DISTINCTIVE tokens.
type LastSentTerms struct {
	// Event tokens distinctive enough that ONE of them is evidence ("kubecon").
	Event map[string]struct{}
	// Generic tokens from the same name: they rank, but never admit alone.
	Generic map[string]struct{}
	// Brand tokens. A brand-only hit is a FALLBACK for a renamed or first-time event,
	// never a match on the event itself -- the caller demotes it accordingly.
	Brand map[string]struct{}
}

// NewLastSentTerms splits an event name into its distinctive and generic tokens, and
// keeps the brand apart as the fallback tier.
//
// The year is stripped from both: "KubeCon Europe 2026" would otherwise find only THIS
// year's emails, and this year's is exactly the one that has not been sent yet.
func NewLastSentTerms(eventName, brandShort string) LastSentTerms {
	out := LastSentTerms{
		Event:   make(map[string]struct{}),
		Generic: make(map[string]struct{}),
		Brand:   EventKeywords(StripYear(brandShort)),
	}
	for token := range EventKeywords(StripYear(eventName)) {
		if _, generic := genericEventWords[token]; generic {
			out.Generic[token] = struct{}{}
			continue
		}
		out.Event[token] = struct{}{}
	}
	// A brand token that is ALSO an event token is not a separate tier of evidence --
	// it is the event match. Leaving it in Brand would let the brand fallback claim
	// credit for a hit the event name already explains.
	for token := range out.Event {
		delete(out.Brand, token)
	}
	for token := range out.Generic {
		delete(out.Brand, token)
	}
	return out
}

// LastSentMatch is the verdict on one candidate email.
type LastSentMatch struct {
	// Overlap is how many event tokens (distinctive AND generic) the candidate carries.
	// It is a RANKING signal only; Matched decides admission.
	Overlap int
	// Matched reports that the candidate plausibly belongs to this event or brand.
	Matched bool
	// BrandOnly reports that nothing in the EVENT name matched and only the brand did.
	BrandOnly bool
}

// MatchLastSent classifies a candidate marketing email against an event's terms.
//
// The name AND the subject are read, because either can carry the event: an email named
// "Registration Open" with the subject "KubeCon NA closes Friday" is the same send as one
// named the other way round. Scoring the name alone gave such a row an overlap of zero and
// truncated it away behind emails that merely had wordier names.
//
// Admission needs ONE distinctive token, or TWO tokens of any kind. A flat "two tokens"
// rule was rejected: it rejects "KubeCon NA 2026" for a KubeCon event, which overlaps on
// exactly one token and is the precise case this exists to find. Distinctiveness, not
// count, is what separates "kubecon" from "summit".
func MatchLastSent(name, subject string, t LastSentTerms) LastSentMatch {
	words := EventKeywords(name + " " + subject)

	distinctive := 0
	for token := range t.Event {
		if _, ok := words[token]; ok {
			distinctive++
		}
	}
	generic := 0
	for token := range t.Generic {
		if _, ok := words[token]; ok {
			generic++
		}
	}

	m := LastSentMatch{Overlap: distinctive + generic}
	switch {
	case distinctive >= 1, m.Overlap >= 2:
		m.Matched = true
		return m
	}

	// Nothing in the event name matched. The brand is the last resort, and it is reported
	// AS a last resort so the caller can drop it when the event itself matched elsewhere.
	for token := range t.Brand {
		if _, ok := words[token]; ok {
			m.Matched, m.BrandOnly = true, true
			return m
		}
	}
	return m
}

// KeywordOverlap is how many of the event's content words a name carries — the
// only ranking signal available for a marketing email, whose record says nothing
// else about which event it belonged to.
func KeywordOverlap(name string, keywords map[string]struct{}) int {
	words := EventKeywords(name)
	overlap := 0
	for keyword := range keywords {
		if _, ok := words[keyword]; ok {
			overlap++
		}
	}
	return overlap
}
