// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package emailstage holds the per-stage generation specs for event email copy.
//
// An event's email programme is not one email: a CFP launch, a schedule announcement and a
// post-event thank-you differ in purpose, tone, urgency, subject shape and call to action. Before
// this package there was ONE hardcoded prompt — "invites registration" — so every send generated
// registration-push copy whatever the operator actually wanted, and the rest was rewritten by hand.
//
// Ported from the LF-Marketing-Ops reference implementation (prasad/skills, 3bca85c2), which
// carries this taxonomy as AI_STAGE_TEMPLATES. It is DATA, deliberately: the marginal cost of a
// sixth stage over a second is a struct literal, which is why all six land together rather than a
// subset that would leave a selector with unusable options.
package emailstage

import "strings"

// Template is one stage's complete generation spec.
//
// Every field feeds the prompt. `SubjectPattern` and `SubjectExamples` are shown to the model as
// shape rather than text to copy, and `UrgencyLevel` (1-10) is what separates a Final Countdown
// from a CFP Launch when the wording alone would not.
type Template struct {
	StageName       string
	Purpose         string
	Timing          string
	Tone            string
	UrgencyLevel    int
	SubjectPattern  string
	SubjectExamples []string
	PreviewPattern  string
	ContentPrompt   string
	CTAStrategy     []string
	FooterNote      string
}

// Stage identifiers. Exported so callers name a stage rather than passing a loose string that a
// typo turns into a silent fallback.
const (
	CFPLaunch            = "CFP Launch"
	ScheduleAnnouncement = "Schedule Announcement"
	RegistrationPush     = "Registration Push"
	DiscountOffer        = "Discount Offer"
	FinalCountdown       = "Final Countdown"
	PostEvent            = "Post-Event"
)

// DefaultStage is what an absent or unrecognised stage resolves to.
//
// Registration Push, deliberately: it is what the single hardcoded prompt this package replaces
// produced, so a caller that sends no stage keeps exactly the behaviour it had. Falling back to
// anything else would silently change every existing caller's output.
const DefaultStage = RegistrationPush

// Resolve returns the stage's template, falling back to DefaultStage.
//
// Never returns a zero Template: an unknown stage means "the caller did not say", not "generate
// nothing", and a zero value would hand the model an empty purpose and tone — worse output than
// the single prompt this replaced, and silently so.
func Resolve(stage string) Template {
	if tpl, ok := Templates[strings.TrimSpace(stage)]; ok {
		return tpl
	}
	return Templates[DefaultStage]
}

// Names lists the stages in programme order, for callers that offer a choice.
func Names() []string {
	return []string{CFPLaunch, ScheduleAnnouncement, RegistrationPush, DiscountOffer, FinalCountdown, PostEvent}
}
