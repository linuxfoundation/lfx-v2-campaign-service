// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

import (
	"strings"
	"testing"
)

// All six stages must be present and complete. A stage that ports with an empty ContentPrompt
// still compiles and still generates -- it just silently produces generic copy, which is the
// failure this whole package exists to remove.
func TestTemplatesAreCompleteForEveryStage(t *testing.T) {
	for _, name := range Names() {
		tpl, ok := Templates[name]
		if !ok {
			t.Fatalf("stage %q is named but has no template", name)
		}
		if tpl.StageName == "" || tpl.Purpose == "" || tpl.Tone == "" || tpl.ContentPrompt == "" {
			t.Errorf("stage %q has empty required fields", name)
		}
		if len(tpl.CTAStrategy) == 0 {
			t.Errorf("stage %q has no CTA strategy", name)
		}
		if tpl.UrgencyLevel < 1 || tpl.UrgencyLevel > 10 {
			t.Errorf("stage %q urgency = %d, want 1-10", name, tpl.UrgencyLevel)
		}
	}
	if len(Templates) != len(Names()) {
		t.Errorf("Templates has %d entries, Names lists %d -- one is out of date", len(Templates), len(Names()))
	}
}

// The stages must be genuinely DIFFERENT. Porting six entries that share one prompt would pass
// every completeness check above while leaving the original problem in place.
func TestStagesDifferFromEachOther(t *testing.T) {
	seen := map[string]string{}
	for _, name := range Names() {
		p := Templates[name].ContentPrompt
		if prev, dup := seen[p]; dup {
			t.Errorf("stages %q and %q share an identical ContentPrompt", prev, name)
		}
		seen[p] = name
	}
}

func TestResolveFallsBackToDefault(t *testing.T) {
	cases := []struct{ in, want string }{
		{PostEvent, PostEvent},
		{"  " + CFPLaunch + "  ", CFPLaunch},
		{"", DefaultStage},
		{"Nonexistent Stage", DefaultStage},
	}
	for _, tc := range cases {
		if got := Resolve(tc.in).StageName; !strings.Contains(got, strings.Split(tc.want, " ")[0]) {
			t.Errorf("Resolve(%q).StageName = %q, want the %q template", tc.in, got, tc.want)
		}
	}
}

// Resolve must never hand back a zero Template. An unknown stage means "the caller did not say",
// not "generate nothing" -- and a zero value would give the model an empty purpose and tone,
// which is worse output than the single prompt this package replaced, and silent.
func TestResolveNeverReturnsAnEmptyTemplate(t *testing.T) {
	for _, in := range []string{"", "garbage", "  ", "Registration push"} {
		if got := Resolve(in); got.ContentPrompt == "" || got.Purpose == "" {
			t.Errorf("Resolve(%q) returned an empty template", in)
		}
	}
}
