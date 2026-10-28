// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

import "strings"

// The survey surface's stages, kept in this file rather than added to Templates because they are
// a DIFFERENT VOCABULARY, not two more points on an event's lifecycle.
//
// An event brief's stage says WHICH SEND in a series it is; a survey brief's stage says WHICH
// ASSET is being promoted. Pooling them would admit `(email, "Survey Promo")` and
// `(survey, "Final Countdown")` -- identity slots no generator can serve -- and would break
// `Names()`, which the published API enum, the database pair CHECK and their parity tests all
// read as "the event stages".
//
// Ported from the LF Research survey/report promotion tool
// (LF-Marketing-Ops/marketing/emailcreationskill/backend/survey_promo/prompts.py). The verbatim
// Gem specification each type carries is reproduced inside its ContentPrompt, because that text
// is the spec the research team signed off on rather than a paraphrase this package invented.
const (
	SurveyPromo = "Survey Promo"
	// SurveyReminder and SurveyClosing are the survey's SERIES, the counterpart of the event
	// surface's six sends: the same instrument promoted again while it is still in field, and once
	// more as it closes. Separate stages rather than one re-sent brief because a brief is keyed on
	// its stage -- two sends under one stage would occupy one identity slot and the second would
	// displace the first, taking its audience and its draft link with it.
	//
	// Report promotion has no counterpart here on purpose: a published report does not close, so a
	// closing send would have to name a deadline the source cannot state.
	SurveyReminder = "Survey Reminder"
	SurveyClosing  = "Survey Closing"
	ReportPromo    = "Report Promo"
)

// DefaultSurveyStage is what an absent or unrecognised stage resolves to on the survey surface.
//
// Survey Promo rather than Report Promo: a survey instrument is the asset whose promotion is
// time-bounded (it closes), so a caller who did not say is likelier to mean the one with a
// deadline. The choice matters only for a caller that sends a stage this package does not know,
// which the API enum and the column CHECK both already refuse.
const DefaultSurveyStage = SurveyPromo

// surveyBodyRule is the length discipline both survey types share.
//
// 100 words is the research team's own limit, and it is far shorter than any event stage's. It is
// stated as a hard cap in each ContentPrompt rather than inherited from the shared prompt, which
// carries no word budget at all.
const surveyBodyRule = `
BODY LENGTH (HARD LIMIT): at most 100 words of body copy TOTAL across every rich_text section
combined, excluding the subject, the preheader and the button text. This is the research team's
own limit, not a style preference. A draft over it is wrong even if every sentence is good.

NEVER INVENT: no deadline, incentive, statistic, finding, sponsor or claim that the source
document below does not state. If the source gives no deadline or no incentive, omit that
sentence entirely rather than writing a plausible one. This outranks every structural
instruction below: a required section whose supporting fact is absent from the source is
dropped, not guessed.`

// The survey series' three sends differ ONLY in how they open. Everything below the opening --
// who is fielding it, the goals, the topics, the impact sentence, the incentive, the CTA and the
// disclosure -- is one specification, so it is written once here and shared, rather than pasted
// into three templates that would drift apart at the first edit.
//
// Split into a head and a tail with the opening between them because the opening is the only
// stage-specific part: `surveyParticipationPrompt` assembles the three.
const surveyParticipationHead = `Generate a SURVEY PARTICIPATION PROMO email.

PRIMARY OBJECTIVE: the reader clicks through and takes the survey. EVERY section supports this.

Mandatory specification from the LF Research team, reproduced verbatim:
"Write at most 100 words with the goal of having the recipient click the link in the email to
take the survey. Ask the reader if they identify with the qualified demographic to take the
survey, explain who is fielding the survey (LF Research and any sponsors), write a sentence
describing the goals of the survey, and include a few topics the survey questions cover. Write a
sentence explaining how the recipient's participation will impact the open source community and
any relevant sectors you can glean from the survey topic."

REQUIRED INFORMATION HIERARCHY:
`

const surveyParticipationTail = `
2. WHO IS FIELDING IT: name LF Research and any sponsors the source document names. Name no
   sponsor the source does not.

3. GOALS: one sentence on what the survey is trying to learn.

4. TOPICS COVERED: the subject areas the questions span. Where the source lists three or more
   distinct topics, render them as a <ul>/<li> list; with fewer, keep them in prose. Judge this
   from the source, not from a preference for lists.

5. COMMUNITY IMPACT: one sentence on how responding helps the open source community and the
   sectors the survey's topic touches. Required by the specification above; ground it in the
   survey's actual subject.

6. INCENTIVE OR DEADLINE: include it ONLY if the source document states one, close to verbatim.
   If the source states neither, write neither.

7. PRIMARY CTA: a single button, "Take the Survey", pointing at the supplied URL.

8. PRIVACY, VISIBILITY AND QUESTIONS: close with the research team's standard disclosure as a
   final rich_text section -- that no personally identifiable information is requested and
   responses are attributed only by role, company size and industry; that the data will be
   analysed into a published report and the dataset made publicly available; and that questions
   go to research@linuxfoundation.org. Write it compactly; it does not count toward the 100-word
   body limit.
`

// The three openings, ported from the prototype's sequence instructions
// (survey_promo/prompts.py, SEQUENCE_INSTRUCTIONS): "invite: neutral opening", "reminder: same
// offer framed as still open", "deadline: urgency ... If the source text does not state a
// deadline, do not invent one".
const (
	surveyPromoOpening = `1. OPENING: ask the reader whether they fit the survey's target demographic. Lead with the
   question -- it is what qualifies the reader to keep reading.
`

	surveyReminderOpening = `1. OPENING: this is the SECOND send to the same readers, so open by framing the survey as still
   open rather than introducing it as news -- "the survey is still open", "there is still time to
   share your perspective". Keep the demographic question only if it can be folded into that
   framing; do not re-introduce the survey as though the reader has not heard of it.

   Do NOT say or imply that the reader has not yet responded. The send list is not filtered on
   who has answered, so that claim is wrong for everyone who already has.
`

	surveyClosingOpening = `1. OPENING: this is the LAST send, so open on the closing. Name the closing date ONLY if the
   source document states one, and then state it exactly as the source does. If the source gives
   no date, use urgency without one -- "closing soon", "the last chance to take part" -- and
   never a date you inferred, rounded or calculated.

   A date that appears in the source for something else (a coupon's expiry, a report's publication
   window) is NOT the survey's closing date. Use only a date the source ties to the survey closing.
`
)

// surveyParticipationPrompt assembles one survey send's spec from the shared body and its own
// opening, so the three stages cannot drift apart below the opening line.
func surveyParticipationPrompt(opening string) string {
	return surveyParticipationHead + opening + surveyParticipationTail + surveyBodyRule
}

// surveyCTAStrategy is the call-to-action discipline every survey send shares. One variable
// rather than three identical literals, for the reason the prompt spec is one: the rule is about
// the SURFACE, so a send that disagreed with it would be a defect rather than a variation.
var surveyCTAStrategy = []string{
	"One primary button, 'Take the Survey', on the supplied URL",
	"Repeat the ask at most twice across the whole email",
	"No secondary call to action competing with the survey",
}

// SurveyTemplates is the survey surface's stage specs, keyed exactly as the stage is stored.
var SurveyTemplates = map[string]Template{
	SurveyPromo: {
		StageName:      SurveyPromo,
		Purpose:        "Win survey responses from readers who fit the target demographic",
		Timing:         "While the survey is in field",
		Tone:           "Direct, second person, plain vocabulary, respectful of the reader's time",
		UrgencyLevel:   4,
		SubjectPattern: "[DEMOGRAPHIC] -- we need your perspective",
		SubjectExamples: []string{
			"Do you work in open source security?",
			"Share your perspective: the 2026 state of [TOPIC]",
			"The [SURVEY_NAME] survey is still open",
		},
		PreviewPattern: "One-line inbox preview naming who the survey is for",
		ContentPrompt:  surveyParticipationPrompt(surveyPromoOpening),
		CTAStrategy:    surveyCTAStrategy,
		FooterNote:     "Closes with the research team's privacy, visibility and questions disclosure.",
		PrimaryCTA:     "Take the Survey",
		// TRUE on every survey stage: a survey brief's url column IS the promoted survey or
		// report, so the CTA's "supplied URL" is that column. False withheld it from the prompt
		// while the prompt still asked for a button pointing at it -- a button with no link.
		LinksToRegistration: true,
	},

	SurveyReminder: {
		StageName:      SurveyReminder,
		Purpose:        "Win responses from readers who have not acted on the first send",
		Timing:         "Mid-field, between the opening send and the close",
		Tone:           "Direct, second person, plain vocabulary, respectful of the reader's time",
		UrgencyLevel:   5,
		SubjectPattern: "The [SURVEY_NAME] survey is still open",
		SubjectExamples: []string{
			"Still open: the 2026 [TOPIC] survey",
			"Your voice matters: we are still seeking input on [TOPIC]",
			"A few minutes to shape [TOPIC] research",
		},
		PreviewPattern:      "One-line inbox preview framing the survey as still open",
		ContentPrompt:       surveyParticipationPrompt(surveyReminderOpening),
		CTAStrategy:         surveyCTAStrategy,
		FooterNote:          "Closes with the research team's privacy, visibility and questions disclosure.",
		PrimaryCTA:          "Take the Survey",
		LinksToRegistration: true,
	},

	SurveyClosing: {
		StageName:      SurveyClosing,
		Purpose:        "Win the last responses before the survey closes",
		Timing:         "The final days the survey is in field",
		Tone:           "Direct and urgent, second person, plain vocabulary, never alarmist",
		UrgencyLevel:   8,
		SubjectPattern: "Last chance: [SURVEY_NAME] closes [DATE_IF_SOURCE_GIVES_ONE]",
		SubjectExamples: []string{
			"Last chance to help shape the 2026 [TOPIC] story",
			"The [SURVEY_NAME] survey closes this week",
			"Closing soon: share your [TOPIC] perspective",
		},
		PreviewPattern:      "One-line inbox preview carrying the closing, with a date only if the source states one",
		ContentPrompt:       surveyParticipationPrompt(surveyClosingOpening),
		CTAStrategy:         surveyCTAStrategy,
		FooterNote:          "Closes with the research team's privacy, visibility and questions disclosure.",
		PrimaryCTA:          "Take the Survey",
		LinksToRegistration: true,
	},

	ReportPromo: {
		StageName:      ReportPromo,
		Purpose:        "Win readers for a newly published research report",
		Timing:         "On and shortly after publication",
		Tone:           "Informative, second person, finding-led, plain vocabulary",
		UrgencyLevel:   3,
		SubjectPattern: "[FINDING] -- new from LF Research",
		SubjectExamples: []string{
			"New research: [TOPIC] in 2026",
			"[STAT] of maintainers report [FINDING]",
			"The [REPORT_NAME] report is out",
		},
		PreviewPattern: "One-line inbox preview naming the report's main topic",
		ContentPrompt: `Generate a REPORT PROMO email.

PRIMARY OBJECTIVE: the reader clicks through and reads the report. EVERY section supports this.

Mandatory specification from the LF Research team, reproduced verbatim:
"Write at most 100 words with the goal of having the recipient click the link in the email to
read the report. Introduce the reader to the main topic of the report, explain who is responsible
for the report (LF Research and any sponsors), write a 2-3 sentences describing 3-4 of the key
findings from the report (which we call infographics). Write a sentence explaining why the reader
should care about the report, based on the recommendations and conclusions of the report.
Endeavour the reader to read the report to learn more. Please also include a sentence that we can
program with a click-to-share button."

REQUIRED INFORMATION HIERARCHY:
1. OPENING: introduce the report's main topic.

2. WHO IS RESPONSIBLE: name LF Research and any sponsors the source document names. Name no
   sponsor the source does not.

3. KEY FINDINGS: 3-4 findings drawn from the source, in 2-3 sentences or as a <ul>/<li> list
   with a bolded lead-in phrase per item. Choose the list when the source presents a clear
   findings or statistics set, and prose when it reads as narrative. Every figure must appear in
   the source document; never round, extrapolate or invent one.

4. WHY IT MATTERS: one sentence on why the reader should care, grounded in the report's own
   recommendations and conclusions.

5. PRIMARY CTA: a single button, "Read the Report", pointing at the supplied URL.

6. SHARE LINE: one short standalone sentence inviting the reader to pass the report on, in its
   own final rich_text section (for example "Know someone who would find this useful? Share the
   report."). Required by the specification above. Do not fold it into an earlier paragraph.
` + surveyBodyRule,
		CTAStrategy: []string{
			"One primary button, 'Read the Report', on the supplied URL",
			"Close with a share line that stands on its own",
			"No competing secondary call to action",
		},
		FooterNote:          "Closes with a standalone click-to-share sentence.",
		PrimaryCTA:          "Read the Report",
		LinksToRegistration: true,
	},
}

// ResolveSurvey returns the survey stage's template, falling back to DefaultSurveyStage.
//
// Separate from Resolve for the reason the two maps are separate: resolving a survey stage against
// the event templates would silently hand back Registration Push -- event copy for a survey brief,
// under a 200.
func ResolveSurvey(stage string) Template {
	if tpl, ok := SurveyTemplates[strings.TrimSpace(stage)]; ok {
		return tpl
	}
	return SurveyTemplates[DefaultSurveyStage]
}

// SurveyNames lists the survey surface's stages, for callers that offer a choice.
//
// The counterpart of Names(). The database pair CHECK, the published API enum and the UI's stage
// picker all read this list, so adding a stage here without a migration writes a row the column
// refuses.
func SurveyNames() []string {
	return []string{SurveyPromo, SurveyReminder, SurveyClosing, ReportPromo}
}

// IsSurveyStage reports whether stage belongs to the survey surface.
//
// Used where a caller has a stage but not the brief's delivery type -- generate-email-copy takes
// the stage as free text -- to choose which template map resolves it.
func IsSurveyStage(stage string) bool {
	_, ok := SurveyTemplates[strings.TrimSpace(stage)]
	return ok
}
