// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Discovery hygiene
//
// Rules that keep lists out of places they must not be offered, found by the
// end-to-end test on a real portal: opt-out and GDPR lists sat in the INCLUDE
// grid (and "Select all" would have ticked them), alongside master lists, test
// lists and staff lists.
// ---------------------------------------------------------------------------

// discoveryHiddenHints mark lists that are never an inclusion candidate. Each has
// its own home: suppressions in the Exclude step, masters in the Reuse step.
var discoveryHiddenHints = []struct {
	reason string
	hints  []string
}{
	{"suppression", suppressionNameHints},
	{"master", []string{"master"}},
	{"test", []string{"[psh-test]", "[test", "test]", "e2e test", "(test)", "- test -"}},
	{"staff", []string{"staff"}},
}

// uploadSuffixRE matches a list named after an uploaded file ("Japan_final1.xlsx").
var uploadSuffixRE = regexp.MustCompile(`(?i)\.(csv|xlsx?|tsv)\s*$`)

// DiscoveryHidden reports whether a discovered list must not be offered for
// inclusion, and why. uncertain says the classifier found no signal: a file-named
// upload is hidden only then, since a classified upload carries real evidence.
func DiscoveryHidden(name string, uncertain bool) (string, bool) {
	for _, group := range discoveryHiddenHints {
		if containsAny(name, group.hints) {
			return group.reason, true
		}
	}
	if uncertain && uploadSuffixRE.MatchString(strings.TrimSpace(name)) {
		return "upload", true
	}
	return "", false
}

// brandSegmentRE captures the brand segment of the naming convention
// "<YYQN> - <BRAND> - <rest>".
var brandSegmentRE = regexp.MustCompile(`(?i)^\s*\d{2}Q[1-4]\s*[-–]\s*([^-–]{2,24}?)\s*[-–]\s*\S`)

// genericBrandSegments are segments that sit in the brand slot but are not a brand.
var genericBrandSegments = map[string]bool{"events": true, "lf events": true, "event": true, "marketing": true}

// DominantBrand is the brand most of these list names are filed under, or "".
//
// The fallback when an event page names no brand: an event's own lists follow
// "<YYQN> - <BRAND> - <Event>", so the brand they agree on is the event's brand.
// At least two names must agree, and a tie between brands yields "" — a guessed
// brand gates an audience on the wrong master list.
//
// eventName keeps the event itself out: some lists file the EVENT in that slot
// ("26Q3 - AGNTCon + MCPCon Japan - APAC Registrants"), and the live test read
// the event's own name back as its brand. A segment sharing a word with the event
// name is the event, not a brand.
func DominantBrand(names []string, eventName string) string {
	eventWords := EventKeywords(eventName)
	counts := map[string]int{}
	display := map[string]string{}
	for _, n := range names {
		m := brandSegmentRE.FindStringSubmatch(n)
		if len(m) != 2 {
			continue
		}
		seg := strings.TrimSpace(m[1])
		key := strings.ToLower(seg)
		if genericBrandSegments[key] || strings.ContainsAny(seg, "0123456789") || SharesKeyword(seg, eventWords) {
			continue
		}
		counts[key]++
		display[key] = seg
	}
	type kv struct {
		k string
		n int
	}
	var ranked []kv
	for k, n := range counts {
		ranked = append(ranked, kv{k, n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].k < ranked[j].k
	})
	if len(ranked) == 0 || ranked[0].n < 2 || (len(ranked) > 1 && ranked[1].n == ranked[0].n) {
		return ""
	}
	return display[ranked[0].k]
}

// audienceWords mark a list that is an audience in its own right rather than one
// event's registrations or attendees.
var audienceWords = []string{"mailable", "marketable", "contacts", "catchment"}

// GuardReuse overrides the model where the list name contradicts it. A list
// named for a different event ("Open Compliance Summit Tokyo 2025") is not this
// audience however well its city matches — the live test ranked exactly that
// "likely the same audience". A recommendation stands only when the name shares
// a DISTINCTIVE event word with the label or reads as a general audience list.
// The audience's own cities and countries, and generic event words ("summit",
// "europe"), are not distinctive: "Open Compliance Summit Tokyo 2025" shares
// "Tokyo" with "AGNTCon + MCPCon Tokyo" and is still another event's list.
// A label with no distinctive word left must appear whole in the list's name.
//
// A restricted audience (speakers, ambassadors, property filters) is narrower
// than any list whose name does not carry the restriction, so such a list is at
// most "update", never "reuse": using it as-is would reach people the restriction
// rules out. A speakers list must be the selected one, or one whose name proves
// the requested scope and editions (SpeakerListFits). A property filter never
// shows in a name, so with one no list is "reuse". Course topics are not a restriction: every education audience carries
// them as its brand fit, and treating them as one would make reuse unreachable.
func GuardReuse(similar []CustomSimilarList, c CustomCriteria) []CustomSimilarList {
	keywords := distinctiveEventKeywords(c)
	locationAudience := c.ContactLocation || c.EventRegistered || c.EducationEnrolled
	out := make([]CustomSimilarList, 0, len(similar))
	for _, s := range similar {
		if s.Recommendation == CustomReuseAsIs || s.Recommendation == CustomReuseUpdate {
			sharesEvent := len(keywords) > 0 && SharesKeyword(s.Name, keywords)
			if len(keywords) == 0 {
				// A label made only of generic and location words ("Open Source Summit
				// Japan") has no single distinctive word, so it must match as a whole.
				sharesEvent = namesWholeLabel(s.Name, c.Label)
			}
			generalAudience := locationAudience && containsAny(s.Name, audienceWords)
			if !sharesEvent && !generalAudience {
				s.Recommendation = CustomReuseNone
				s.Reason = "Named for a different event or audience."
			}
		}
		if s.Recommendation == CustomReuseAsIs && !carriesRestriction(s, c) {
			s.Recommendation = CustomReuseUpdate
			s.Reason = "Broader than this audience: its name does not carry the audience's restriction."
		}
		out = append(out, s)
	}
	order := map[CustomReuseRecommendation]int{CustomReuseAsIs: 0, CustomReuseUpdate: 1, "": 2, CustomReuseNone: 3}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Recommendation] < order[out[j].Recommendation] })
	return out
}

// distinctiveEventKeywords are the label's event words that can tell one event
// from another: not a generic event word, and not one of the audience's own
// locations, which every list for the place shares.
func distinctiveEventKeywords(c CustomCriteria) map[string]struct{} {
	locations := EventKeywords(strings.Join(append(cleanValues(c.Cities), cleanValues(c.Countries)...), " "))
	out := EventKeywords(c.Label)
	for token := range out {
		_, generic := genericEventWords[token]
		_, location := locations[token]
		if generic || location {
			delete(out, token)
		}
	}
	return out
}

// namesWholeLabel reports whether a list name contains the label as a phrase,
// ignoring case, spacing and the label's year.
func namesWholeLabel(name, label string) bool {
	phrase := strings.ToLower(strings.Join(strings.Fields(StripYear(label)), " "))
	return phrase != "" && strings.Contains(strings.ToLower(strings.Join(strings.Fields(name), " ")), phrase)
}

// carriesRestriction reports whether a similar list shows every restriction the
// criteria narrow by. A speakers list must be the one already selected, or one
// SpeakerListFits accepts for the requested scope and editions (it fails closed,
// as the auto-pick does). A property filter never shows in a name.
func carriesRestriction(s CustomSimilarList, c CustomCriteria) bool {
	lower := strings.ToLower(s.Name)
	if sp := c.Speakers; sp != nil {
		selected := strings.TrimSpace(sp.ListID)
		if selected != "" && s.ListID != selected {
			return false
		}
		if selected == "" && !SpeakerListFits(s.Name, sp.Scope, sp.EventNames) {
			return false
		}
	}
	if c.Ambassadors && !strings.Contains(lower, "ambassador") {
		return false
	}
	return len(c.PropertyFilters) == 0
}

// customMaxAnswerRounds bounds the question loop: after this many answers the
// draft is final and anything left is the operator's to adjust in the form. The
// live test showed the model raising a fresh question every round.
const customMaxAnswerRounds = 3

// SettleQuestions drops questions already answered and, after enough rounds,
// all of them.
func SettleQuestions(questions []CustomQuestion, answers []CustomAnswer) []CustomQuestion {
	if len(answers) >= customMaxAnswerRounds {
		return []CustomQuestion{}
	}
	answered := map[string]bool{}
	for _, a := range answers {
		answered[strings.ToLower(strings.TrimSpace(a.QuestionID))] = true
	}
	out := make([]CustomQuestion, 0, len(questions))
	for _, q := range questions {
		if answered[strings.ToLower(strings.TrimSpace(q.ID))] {
			continue
		}
		out = append(out, q)
	}
	return out
}

// WithEventYear appends the year of the event's first date to a name that has
// none. Registration and speaker filters match event_name exactly, and edition
// names carry their year ("AGNTCon + MCPCon Japan 2026").
func WithEventYear(name string, dates []string) string {
	name = strings.TrimSpace(name)
	if name == "" || yearRE.MatchString(name) {
		return name
	}
	for _, d := range dates {
		if y := yearRE.FindString(d); y != "" {
			return name + " " + y
		}
	}
	return name
}
