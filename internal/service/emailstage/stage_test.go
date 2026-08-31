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
		// EXACT, not a first-word substring. The looser form discriminates today only because no
		// two stages share a first word -- adding "Registration Launch" beside "Registration Push"
		// would silently make it vacuous.
		if got := Templates[tc.want].StageName; Resolve(tc.in).StageName != got {
			t.Errorf("Resolve(%q).StageName = %q, want %q", tc.in, Resolve(tc.in).StageName, got)
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

// A claim about an unsupplied fact must carry a PLACEHOLDER, or nothing can remove it.
//
// The composed prompt tells the model to OMIT any sentence whose [BRACKETED] placeholder has no
// supplied value. That rule reaches placeholders only. "Early bird pricing available now" carried
// none, so the model was told as FACT that early pricing exists -- for an event whose price is
// never supplied, on the DEFAULT stage every unrecognised value resolves to.
//
// Only pricing/discount/code words are checked. A generic line ("Event guide and schedule
// inside") asserts nothing the brief has to supply, and requiring a placeholder in every preview
// would be noise rather than a guard.
func TestPatternsDoNotAssertUnsuppliedCommercialFacts(t *testing.T) {
	// Words that name a fact NOTHING in the pipeline supplies: only eventName, location and dates
	// are ever filled. A pattern using one of these must wrap it in a placeholder so the OMIT rule
	// can drop the sentence.
	commercial := []string{"price", "pricing", "rate", "discount", "early bird", "promo", "coupon", "code", "save"}

	for name, tpl := range Templates {
		for field, text := range map[string]string{
			"SubjectPattern": tpl.SubjectPattern,
			"PreviewPattern": tpl.PreviewPattern,
			"FooterNote":     tpl.FooterNote,
		} {
			lower := strings.ToLower(text)
			for _, w := range commercial {
				if !strings.Contains(lower, w) {
					continue
				}
				// The SENTENCE carrying the claim must hold the placeholder -- not merely the
				// string. "Registration closes [DATE]. Early bird pricing available now." has a
				// bracket, but it belongs to a different sentence, so the OMIT rule still cannot
				// touch the pricing claim. Checking the whole string let exactly that through.
				sentence := claimSentence(text, w)
				if !strings.Contains(sentence, "[") {
					t.Errorf("stage %q %s asserts %q in a sentence with no placeholder, so the OMIT rule cannot remove it: %q",
						name, field, w, sentence)
				}
			}
		}
	}
}

// claimSentence returns the sentence of text containing needle (case-insensitive), or text when
// it cannot be split. Sentence-scoped because the OMIT rule drops a SENTENCE, so a placeholder
// elsewhere in the string does not make this claim removable.
func claimSentence(text, needle string) string {
	for _, s := range strings.Split(text, ".") {
		if strings.Contains(strings.ToLower(s), needle) {
			return s
		}
	}
	return text
}
