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
	ReportPromo = "Report Promo"
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
		ContentPrompt: `Generate a SURVEY PARTICIPATION PROMO email.

PRIMARY OBJECTIVE: the reader clicks through and takes the survey. EVERY section supports this.

Mandatory specification from the LF Research team, reproduced verbatim:
"Write at most 100 words with the goal of having the recipient click the link in the email to
take the survey. Ask the reader if they identify with the qualified demographic to take the
survey, explain who is fielding the survey (LF Research and any sponsors), write a sentence
describing the goals of the survey, and include a few topics the survey questions cover. Write a
sentence explaining how the recipient's participation will impact the open source community and
any relevant sectors you can glean from the survey topic."

REQUIRED INFORMATION HIERARCHY:
1. OPENING: ask the reader whether they fit the survey's target demographic. Lead with the
   question -- it is what qualifies the reader to keep reading.

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
` + surveyBodyRule,
		CTAStrategy: []string{
			"One primary button, 'Take the Survey', on the supplied URL",
			"Repeat the ask at most twice across the whole email",
			"No secondary call to action competing with the survey",
		},
		FooterNote:          "Closes with the research team's privacy, visibility and questions disclosure.",
		PrimaryCTA:          "Take the Survey",
		LinksToRegistration: false,
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
		LinksToRegistration: false,
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
	return []string{SurveyPromo, ReportPromo}
}

// IsSurveyStage reports whether stage belongs to the survey surface.
//
// Used where a caller has a stage but not the brief's delivery type -- generate-email-copy takes
// the stage as free text -- to choose which template map resolves it.
func IsSurveyStage(stage string) bool {
	_, ok := SurveyTemplates[strings.TrimSpace(stage)]
	return ok
}
