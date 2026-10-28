// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

import (
	"strings"
	"testing"
)

// TestSurveyNamesAndTemplatesAgree guards the pair every surface-aware caller trusts: the list a
// picker offers and the map a generator resolves against must name the same stages. A stage in one
// but not the other is either an unofferable template or an option that silently resolves to the
// default.
func TestSurveyNamesAndTemplatesAgree(t *testing.T) {
	if len(SurveyNames()) != len(SurveyTemplates) {
		t.Fatalf("SurveyNames() has %d entries, SurveyTemplates has %d", len(SurveyNames()), len(SurveyTemplates))
	}
	for _, name := range SurveyNames() {
		tpl, ok := SurveyTemplates[name]
		if !ok {
			t.Errorf("SurveyNames() offers %q with no template", name)
			continue
		}
		if tpl.StageName != name {
			t.Errorf("template %q carries StageName %q", name, tpl.StageName)
		}
		if strings.TrimSpace(tpl.ContentPrompt) == "" {
			t.Errorf("template %q has an empty ContentPrompt, so the model gets no brief", name)
		}
		if strings.TrimSpace(tpl.PrimaryCTA) == "" {
			t.Errorf("template %q has no PrimaryCTA; the service refuses an empty cta with a 503", name)
		}
	}
}

// TestSurveyAndEventStagesDoNotOverlap is the invariant the two separate maps exist for. If a name
// appeared in both, IsSurveyStage would route an event stage to the survey templates, and the pair
// CHECK's two arms would admit the same stage on both surfaces.
func TestSurveyAndEventStagesDoNotOverlap(t *testing.T) {
	for _, s := range SurveyNames() {
		if _, clash := Templates[s]; clash {
			t.Errorf("%q is both a survey stage and an event stage", s)
		}
	}
	for _, e := range Names() {
		if _, clash := SurveyTemplates[e]; clash {
			t.Errorf("%q is both an event stage and a survey stage", e)
		}
	}
}

// TestResolveSurveyFallsBackWithoutCrossingSurfaces pins the two halves of the routing rule: an
// unknown stage resolves to the survey default rather than to a zero Template, and an EVENT stage
// handed to ResolveSurvey does NOT resolve to its event template -- a caller that did that chose
// the wrong surface, and quietly serving event copy is the failure this split prevents.
func TestResolveSurveyFallsBackWithoutCrossingSurfaces(t *testing.T) {
	if got := ResolveSurvey(SurveyPromo).StageName; got != SurveyPromo {
		t.Errorf("ResolveSurvey(%q).StageName = %q", SurveyPromo, got)
	}
	if got := ResolveSurvey(ReportPromo).StageName; got != ReportPromo {
		t.Errorf("ResolveSurvey(%q).StageName = %q", ReportPromo, got)
	}
	if got := ResolveSurvey("  " + ReportPromo + "  ").StageName; got != ReportPromo {
		t.Errorf("ResolveSurvey trims surrounding space, got %q", got)
	}
	if got := ResolveSurvey("Nonsense").StageName; got != DefaultSurveyStage {
		t.Errorf("ResolveSurvey(unknown).StageName = %q, want the survey default %q", got, DefaultSurveyStage)
	}
	if got := ResolveSurvey(RegistrationPush).StageName; got != DefaultSurveyStage {
		t.Errorf("ResolveSurvey(%q) returned %q: an event stage must not resolve on the survey "+
			"surface, it must take the survey default", RegistrationPush, got)
	}
}

// TestIsSurveyStage pins the predicate generate-email-copy uses to pick a map when it is given a
// stage but never a delivery type.
func TestIsSurveyStage(t *testing.T) {
	for _, s := range SurveyNames() {
		if !IsSurveyStage(s) {
			t.Errorf("IsSurveyStage(%q) = false", s)
		}
	}
	for _, e := range append(Names(), "", "Nonsense") {
		if IsSurveyStage(e) {
			t.Errorf("IsSurveyStage(%q) = true", e)
		}
	}
}

// TestSurveyPromptsCarryTheirNonNegotiableRules pins the two rules the research team's own
// specification makes non-negotiable, and which a prompt edit could silently drop: the 100-word
// body cap and the anti-invention clause. Both templates build from one shared constant, so this
// fails if that constant stops reaching either.
func TestSurveyPromptsCarryTheirNonNegotiableRules(t *testing.T) {
	for _, name := range SurveyNames() {
		prompt := SurveyTemplates[name].ContentPrompt
		if !strings.Contains(prompt, "at most 100 words") {
			t.Errorf("%q: the 100-word body cap is missing from the prompt", name)
		}
		if !strings.Contains(prompt, "NEVER INVENT") {
			t.Errorf("%q: the anti-invention rule is missing from the prompt", name)
		}
	}
}
