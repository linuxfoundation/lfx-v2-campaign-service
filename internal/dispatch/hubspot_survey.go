// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// emailLayoutFor chooses the chrome a staged draft is rebuilt with. A survey brief gets the LF
// Research survey layout; every other brief keeps the event layout it always had.
//
// Read from the brief's delivery type rather than its stage so that a survey brief whose stage the
// column CHECK has not seen yet (a stage added ahead of its migration) still lands in the survey
// chrome rather than silently turning into an event email.
func emailLayoutFor(brief *model.CampaignBrief) hubspot.EmailLayout {
	if brief != nil && brief.DeliveryType == model.DeliverySurvey {
		return hubspot.LayoutSurvey
	}
	return hubspot.LayoutEvent
}

// The claims the research team's own rule forbids inventing: "no deadline, incentive, statistic,
// finding, sponsor or claim that the source document does not state". The prompt asks the model to
// obey it; this is the only place the service checks. Deliberately narrow -- a percentage, a
// calendar date, and a question or minute count -- because those are the figures a recipient acts
// on, and because a wider net would warn on every ordinary number in a body and teach operators to
// ignore the warning.
var (
	// A claim includes a range's lower end ("5-15 minutes", "3–5%", "3%-5%"), so a copy that changes
	// only the lower end is checked as a whole rather than as its upper end alone.
	surveyClaimPercent = regexp.MustCompile(`\b(?:\d{1,3}(?:\.\d+)?\s?%?\s*[-–]\s*)?\d{1,3}(?:\.\d+)?\s?%`)
	surveyClaimCount   = regexp.MustCompile(`(?i)\b(?:\d{1,3}\s*[-–]\s*)?\d{1,3}[\s-]+(?:questions?|minutes?)\b`)
	surveyClaimDate    = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b`)
	htmlTagPattern     = regexp.MustCompile(`<[^>]*>`)
	// digitRangeHyphen is a hyphen or en dash between two numbers: a range ("10-15 minutes",
	// "3–5%"). A "%" on the lower end ("3%-5%") is dropped with it, so that spelling compares equal
	// to "3-5%".
	digitRangeHyphen = regexp.MustCompile(`(\d)\s*%?\s*[-–]\s*(\d)`)
	spacePattern     = regexp.MustCompile(`\s+`)
)

// maxSurveyCopyWarnings caps the list, so a body that is wrong everywhere does not bury the one
// line an operator needs under twenty.
const maxSurveyCopyWarnings = 5

// surveyCopyWarnings reports figures and dates the generated body states that the brief's source
// text does not contain. ADVISORY: it never blocks staging, because the draft is a human-reviewed
// draft and the source text may say the same thing in words this check does not read.
//
// The returned strings are built only from what the three patterns above can match (digits, a
// month name, a unit word), so they carry no free text from the model into a persisted,
// API-visible field.
func surveyCopyWarnings(brief *model.CampaignBrief, bodyHTML string) []string {
	if brief == nil || brief.DeliveryType != model.DeliverySurvey || strings.TrimSpace(bodyHTML) == "" {
		return nil
	}
	var details struct {
		SourceText string `json:"sourceText"`
	}
	if len(brief.EventDetails) > 0 {
		// A brief whose details do not decode has no source text to check against; the warnings
		// below then say so, which is the honest answer, rather than this returning nothing.
		_ = json.Unmarshal(brief.EventDetails, &details)
	}

	body := visibleText(bodyHTML)
	source := visibleText(details.SourceText)
	compactSource := compactClaims(source)

	var out []string
	seen := map[string]bool{}
	add := func(claim string) {
		if !seen[claim] && len(out) < maxSurveyCopyWarnings {
			seen[claim] = true
			out = append(out, fmt.Sprintf("The email copy states %q, which the source text does not contain. Check it against the source before sending.", claim))
		}
	}

	for _, claim := range surveyClaimPercent.FindAllString(body, -1) {
		if !claimStated(compactSource, claim) {
			add(strings.TrimSpace(claim))
		}
	}
	for _, claim := range surveyClaimCount.FindAllString(body, -1) {
		if !claimStated(compactSource, claim) {
			add(strings.TrimSpace(claim))
		}
	}
	sourceDates := dateKeys(source)
	for _, m := range surveyClaimDate.FindAllStringSubmatch(body, -1) {
		if !sourceDates[dateKey(m[1], m[2])] {
			add(strings.TrimSpace(m[0]))
		}
	}
	return out
}

// visibleText is the text a recipient reads: tags dropped, entities decoded, whitespace collapsed.
// Dropping the tags first is what keeps `line-height:160%` in a style attribute from reading as a
// percentage the copy states.
func visibleText(s string) string {
	s = htmlTagPattern.ReplaceAllString(s, " ")
	return strings.TrimSpace(spacePattern.ReplaceAllString(html.UnescapeString(s), " "))
}

// compactClaims lower-cases and removes spaces (including a non-breaking one, which visibleText
// leaves in place) and the hyphens that join a number to a word, so "35 %" and "35%", or "9
// questions" and "9-questions", compare equal. A hyphen or en dash between two numbers is a range
// and is kept as a separator instead (see below), so "10–15" and "10-15" compare equal too. The
// unit is compared in the singular, so "a 3-minute survey" and "3 minutes" compare equal.
func compactClaims(s string) string {
	s = strings.ToLower(s)
	// A hyphen between two numbers is a range, not a joiner. Deleting it fuses "10-15" into "1015",
	// which hides the "15" the copy states behind a leading "10" (a false warning on the most common
	// survey-length phrasing), or lets an invented "35%" pass as the source's "3-5%". Turn it into a
	// separator that is not a digit so each end stays whole. Looped because a chain ("1-2-3") shares
	// its middle number between two matches, which one pass cannot rewrite.
	for digitRangeHyphen.MatchString(s) {
		s = digitRangeHyphen.ReplaceAllString(s, "$1~$2")
	}
	s = strings.NewReplacer(" ", "", "-", "", "\u00a0", "").Replace(s)
	// The unit is compared in the singular, so "a 3-minute survey" backs "3 minutes" and "1
	// question" backs "1 questions"-style plurals in either direction. The figure is what is checked.
	return strings.NewReplacer("minutes", "minute", "questions", "question").Replace(s)
}

// claimStated reports whether the compacted source states the claim as a whole figure. A bare
// substring test would let an invented "5%" pass because the source says "35%", or "3 minutes"
// because it says "13 minutes": the match must not continue a longer number on its left. The right
// edge needs no such rule, because every claim ends in a unit ("%", "questions", "minutes").
func claimStated(compactSource, claim string) bool {
	want := compactClaims(claim)
	for from := 0; from <= len(compactSource); {
		i := strings.Index(compactSource[from:], want)
		if i < 0 {
			return false
		}
		at := from + i
		if at == 0 || !isNumberByte(compactSource[at-1]) {
			return true
		}
		from = at + 1
	}
	return false
}

// isNumberByte is a digit or the decimal point: what, directly left of a figure, means the figure
// is the tail of a longer one ("35%" in "135%", "5%" in "3.5%").
func isNumberByte(b byte) bool {
	return (b >= '0' && b <= '9') || b == '.'
}

// dateKey is month-abbreviation plus day, so "February 28" in the copy matches "Feb 28, 2027" in
// the source: the year is not compared, only the day a reader would act on.
func dateKey(month, day string) string {
	m := strings.ToLower(month)
	if len(m) > 3 {
		m = m[:3]
	}
	return m + " " + strings.TrimLeft(day, "0")
}

func dateKeys(text string) map[string]bool {
	keys := map[string]bool{}
	for _, m := range surveyClaimDate.FindAllStringSubmatch(text, -1) {
		keys[dateKey(m[1], m[2])] = true
	}
	return keys
}
