// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

import "strings"

// The newsletter surface's stage, kept apart from Templates and SurveyTemplates for the reason
// survey.go gives: it is a different vocabulary. An event brief's stage says WHICH SEND in an
// event's series it is; a newsletter brief's stage says the brief is ONE ISSUE of a brand's
// recurring newsletter. Pooling it with either map would admit `(email, "Newsletter")` or
// `(newsletter, "Final Countdown")`, identity slots no generator can serve.
//
// One stage, not one per month: an issue is told apart by its brief's event_slug (the brand plus
// the issue, e.g. `openssf-newsletter-2026-11`), not by its stage, so a monthly series is many
// briefs under one stage rather than twelve stages.
//
// Ported from the legacy emailcreationskill, which treats a newsletter as an email whose clone
// source is the brand's previous PUBLISHED email named "newsletter"
// (backend/integrations/hubspot.py lookup_brand_history). That skill had no newsletter-specific
// content prompt; the one below is written for this surface.
const NewsletterIssue = "Newsletter"

// DefaultNewsletterStage is what an absent or unrecognised stage resolves to on the newsletter
// surface. There is only one.
const DefaultNewsletterStage = NewsletterIssue

// newsletterContentPrompt is the newsletter issue's specification.
//
// The operator's notes arrive as the "Source document" fact line (the survey surface's
// source_text field, reused), and past issues of the same brand's newsletter arrive as the style
// reference block. The prompt makes the split explicit: the notes are the ONLY source of fact,
// the past issues set only the structure and the voice.
const newsletterContentPrompt = `Generate ONE ISSUE of a brand's recurring email NEWSLETTER.

PRIMARY OBJECTIVE: a reader who skims the issue learns what is new and clicks through to the items
they care about. Every section is one news item, announcement, event or resource.

WHERE FACTS COME FROM:
- The "Source document" above is the operator's notes for THIS issue. It is the ONLY source of
  fact: every item, date, name, figure and link in the email must appear in it.
- The style reference from past issues, when present, sets the STRUCTURE and VOICE only: the order
  and kind of sections, the heading style, the greeting, how long each item runs, and how items
  are introduced. Never carry an item, a date, a name or a link from a past issue into this one;
  past issues are stale by definition.
- When there is no style reference, use the structure below.

STRUCTURE (when no past issue says otherwise):
1. OPENING: the first rich_text section opens with an <h1> title naming the newsletter and the
   issue, taken from the Event Name line above (e.g. "OpenSSF Newsletter: November 2026"; drop
   anything in parentheses), then a one- or two-sentence lead-in that names the issue's main
   theme or headline item. No "Welcome to our newsletter" filler.
2. ITEMS: one rich_text section per item in the notes, in the order the notes give them unless the
   notes mark one as the headline. Each opens with an <h2> naming the item, then 1-3 short
   sentences on what it is and why the reader should care.
3. GROUPED SHORT ITEMS: when the notes list three or more brief items of one kind (upcoming
   events, new members, blog posts, releases), render them together as a <ul>/<li> list in one
   section rather than one section each.
4. CLOSING: at most one short line pointing the reader to where to follow along, only if the
   notes give such a place.

LINKS:
- A URL that appears verbatim in the Source document may be linked inline, ONCE, inside the
  section about its item, as <a href="...">link text naming what it is</a>, copied exactly.
- Never link an item whose URL the notes do not give. Never invent, shorten or "fix" a URL.
- At most ONE button, "Read More": use it only when the notes name a single primary destination
  for the whole issue (a headline event's registration, a report), and point it at that URL.
  Otherwise emit no button section.

LENGTH: keep each item short; a newsletter is skimmed. Prefer more items over longer ones, and
never pad an item the notes describe in one line.

NEVER INVENT: no item, date, statistic, quote, speaker, sponsor or link that the Source document
does not state. A notes line too thin to write a sentence from becomes a list bullet, not an
embellished paragraph.`

// NewsletterTemplates is the newsletter surface's stage spec, keyed exactly as the stage is stored.
var NewsletterTemplates = map[string]Template{
	NewsletterIssue: {
		StageName:      NewsletterIssue,
		Purpose:        "Tell subscribers what is new in this issue and send them to the items they care about",
		Timing:         "On the brand's regular newsletter cadence",
		Tone:           "Informative, warm, plain vocabulary; community news rather than promotion",
		UrgencyLevel:   2,
		SubjectPattern: "[BRAND] Newsletter: [HEADLINE ITEM]",
		SubjectExamples: []string{
			"OpenSSF Newsletter: Scorecard v5 is here",
			"This month in [BRAND]: [HEADLINE ITEM]",
			"[HEADLINE ITEM], plus [SECOND ITEM] and more news",
		},
		PreviewPattern: "One-line inbox preview naming the next two items after the headline",
		ContentPrompt:  newsletterContentPrompt,
		CTAStrategy: []string{
			"Inline links per item, each copied from the notes",
			"At most one button, only for a single primary destination the notes name",
			"No invented call to action",
		},
		FooterNote: "No sign-off: the cloned template carries the brand's own newsletter footer.",
		PrimaryCTA: "Read More",
		// A newsletter has no registration page. The brief's url column, when set, is the brand's
		// site rather than a destination the issue's button means, so it is withheld from the
		// prompt (see Template.LinksToRegistration); the only links are the ones the notes carry.
		LinksToRegistration: false,
	},
}

// ResolveNewsletter returns the newsletter stage's template, falling back to
// DefaultNewsletterStage. Separate from Resolve for the reason ResolveSurvey is: resolving the
// newsletter stage against the event templates would silently hand back Registration Push.
func ResolveNewsletter(stage string) Template {
	if tpl, ok := NewsletterTemplates[strings.TrimSpace(stage)]; ok {
		return tpl
	}
	return NewsletterTemplates[DefaultNewsletterStage]
}

// NewsletterNames lists the newsletter surface's stages. The database pair CHECK, the published
// API enum and the UI all read this list, so a stage added here without a migration writes a row
// the column refuses.
func NewsletterNames() []string {
	return []string{NewsletterIssue}
}

// IsNewsletterStage reports whether stage belongs to the newsletter surface. Used where a caller
// has a stage but not the brief's delivery type -- generate-email-copy takes the stage as free
// text -- to choose which template map resolves it.
func IsNewsletterStage(stage string) bool {
	_, ok := NewsletterTemplates[strings.TrimSpace(stage)]
	return ok
}
