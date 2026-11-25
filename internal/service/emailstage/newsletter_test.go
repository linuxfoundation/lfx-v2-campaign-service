// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

import (
	"strings"
	"testing"
)

// TestNewsletterNamesAndTemplatesAgree is TestSurveyNamesAndTemplatesAgree for the newsletter map.
func TestNewsletterNamesAndTemplatesAgree(t *testing.T) {
	if len(NewsletterNames()) != len(NewsletterTemplates) {
		t.Fatalf("NewsletterNames() has %d entries, NewsletterTemplates has %d", len(NewsletterNames()), len(NewsletterTemplates))
	}
	for _, name := range NewsletterNames() {
		tpl, ok := NewsletterTemplates[name]
		if !ok {
			t.Fatalf("NewsletterNames() offers %q with no template", name)
		}
		if tpl.StageName != name {
			t.Errorf("template %q carries StageName %q", name, tpl.StageName)
		}
		if strings.TrimSpace(tpl.ContentPrompt) == "" {
			t.Errorf("template %q has an empty ContentPrompt", name)
		}
		if strings.TrimSpace(tpl.PrimaryCTA) == "" {
			t.Errorf("template %q has no PrimaryCTA", name)
		}
		if tpl.LinksToRegistration {
			t.Errorf("template %q links to registration; a newsletter has no registration page", name)
		}
	}
}

// TestNewsletterStagesDoNotOverlapOtherSurfaces keeps the three maps disjoint, so a stage routes to
// exactly one surface's templates.
func TestNewsletterStagesDoNotOverlapOtherSurfaces(t *testing.T) {
	for _, n := range NewsletterNames() {
		if _, clash := Templates[n]; clash {
			t.Errorf("%q is both a newsletter stage and an event stage", n)
		}
		if _, clash := SurveyTemplates[n]; clash {
			t.Errorf("%q is both a newsletter stage and a survey stage", n)
		}
	}
}

// TestResolveNewsletterFallsBackWithoutCrossingSurfaces mirrors the survey routing test.
func TestResolveNewsletterFallsBackWithoutCrossingSurfaces(t *testing.T) {
	if got := ResolveNewsletter("  " + NewsletterIssue + " ").StageName; got != NewsletterIssue {
		t.Errorf("ResolveNewsletter trims surrounding space, got %q", got)
	}
	for _, other := range []string{RegistrationPush, SurveyPromo, "Nonsense"} {
		if got := ResolveNewsletter(other).StageName; got != DefaultNewsletterStage {
			t.Errorf("ResolveNewsletter(%q) = %q, want the newsletter default", other, got)
		}
	}
	if !IsNewsletterStage(NewsletterIssue) || IsNewsletterStage(RegistrationPush) || IsNewsletterStage(SurveyPromo) {
		t.Error("IsNewsletterStage routes a stage to the wrong surface")
	}
}

// TestNewsletterPromptKeepsFactsToTheNotes pins the rules that make reference-driven drafting safe:
// the notes are the only source of fact, and the past issues are structure and voice only.
func TestNewsletterPromptKeepsFactsToTheNotes(t *testing.T) {
	prompt := NewsletterTemplates[NewsletterIssue].ContentPrompt
	for _, rule := range []string{"NEVER INVENT", "ONLY source of", "STRUCTURE and VOICE only", "appears verbatim in the Source document"} {
		if !strings.Contains(prompt, rule) {
			t.Errorf("the newsletter prompt lost the rule %q", rule)
		}
	}
}
