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

// LastSentSearchTerms are the marketing-email name searches to try, in order,
// de-duplicated.
//
// The event name first, then the brand. The brand fallback is for a first-time or
// renamed event, where the brand's prior sends are still the best available
// precedent — the first term that yields any PUBLISHED email wins, so the fallback
// never overrides a real match.
func LastSentSearchTerms(eventName, brandShort string) []string {
	out := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, raw := range []string{eventName, brandShort} {
		term := strings.TrimSpace(StripYear(raw))
		if term == "" {
			continue
		}
		if _, dup := seen[term]; dup {
			continue
		}
		seen[term] = struct{}{}
		out = append(out, term)
	}
	return out
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
