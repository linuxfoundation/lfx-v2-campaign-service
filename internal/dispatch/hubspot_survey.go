// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
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
	// A figure is a whole number or a decimal ("3", "8.5"). A claim includes a range's lower end
	// ("5-15 minutes", "3–5%", "3%-5%"), so a copy that changes only the lower end is checked as a
	// whole rather than as its upper end alone. Matching is leftmost, so "8.5 minutes" is read from
	// its "8", never as the "5 minutes" after the decimal point.
	surveyClaimPercent = regexp.MustCompile(`\b(?:\d{1,3}(?:\.\d+)?\s?%?\s*[-–]\s*)?\d{1,3}(?:\.\d+)?\s?%`)
	surveyClaimCount   = regexp.MustCompile(`(?i)\b(?:\d{1,3}(?:\.\d+)?\s*[-–]\s*)?\d{1,3}(?:\.\d+)?[\s-]+(?:questions?|minutes?)\b`)
	surveyClaimDate    = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b`)
	htmlTagPattern     = regexp.MustCompile(`<[^>]*>`)
	spacePattern       = regexp.MustCompile(`\s+`)
	// claimParts splits a matched claim into its optional lower end, its figure and its unit.
	claimParts = regexp.MustCompile(`^(?:(\d+(?:\.\d+)?)\s*%?\s*[-–]\s*)?(\d+(?:\.\d+)?)[\s-]*(%|question|minute)`)
)

// maxSurveyCopyWarnings caps the list, so a body that is wrong everywhere does not bury the one
// line an operator needs under twenty.
const maxSurveyCopyWarnings = 5

// surveyCopyWarnings reports figures and dates the generated body states that the brief's source
// text does not contain. ADVISORY: it never blocks staging, because the draft is a human-reviewed
// draft and the source text may say the same thing in words this check does not read.
//
// The source is read with the same patterns as the body, and the two are compared as claim keys
// (see claimKeys), never as raw text: the patterns decide where a figure starts and ends, so a
// sentence's full stop, a neighbouring number or a longer figure cannot be mistaken for part of
// one.
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

	var out []string
	seen := map[string]bool{}
	add := func(claim string) {
		if !seen[claim] && len(out) < maxSurveyCopyWarnings {
			seen[claim] = true
			out = append(out, fmt.Sprintf("The email copy states %q, which the source text does not contain. Check it against the source before sending.", claim))
		}
	}

	for _, pattern := range []*regexp.Regexp{surveyClaimPercent, surveyClaimCount} {
		stated := map[string]bool{}
		for _, claim := range pattern.FindAllString(source, -1) {
			for _, key := range claimKeys(claim, true) {
				stated[key] = true
			}
		}
		for _, claim := range pattern.FindAllString(body, -1) {
			if keys := claimKeys(claim, false); len(keys) == 0 || !stated[keys[0]] {
				add(strings.TrimSpace(claim))
			}
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
// percentage the copy states. A non-breaking space (what "&nbsp;" decodes to) becomes a plain one,
// because RE2's \s does not match it and "9&nbsp;questions" would otherwise not read as a claim.
func visibleText(s string) string {
	s = htmlTagPattern.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(html.UnescapeString(s), "\u00a0", " ")
	return strings.TrimSpace(spacePattern.ReplaceAllString(s, " "))
}

// claimKeys turns a matched percentage or count into the key it is compared by: the figure (or
// "lower~upper" for a range) and the unit in the singular, so "35 %" and "35%", "9-questions" and
// "9 questions", "a 3-minute survey" and "3 minutes", and "10–15", "10 - 15" or "3%-5%" and
// "10-15" or "3-5%" compare equal. The figure is compared whole, so "5%" is not "35%" or "3.5%",
// and "35%" is not the "3-5%" range.
//
// With asSource, a range also yields a key for each of its ends: a source that says "10-15
// minutes" backs copy saying "15 minutes", but a copy's range is backed only by the same range.
func claimKeys(claim string, asSource bool) []string {
	m := claimParts.FindStringSubmatch(strings.ToLower(strings.TrimSpace(claim)))
	if m == nil {
		return nil
	}
	lower, figure, unit := canonicalFigure(m[1]), canonicalFigure(m[2]), m[3]
	if lower == "" {
		return []string{figure + unit}
	}
	keys := []string{lower + "~" + figure + unit}
	if asSource {
		keys = append(keys, lower+unit, figure+unit)
	}
	return keys
}

// canonicalFigure spells a figure one way, so "3.50" and "3.5", or "05" and "5", compare equal.
func canonicalFigure(figure string) string {
	f, err := strconv.ParseFloat(figure, 64)
	if err != nil {
		return figure
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
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
