// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package emailstage

import (
	"fmt"
	"regexp"
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
	// Facts NOTHING in the pipeline supplies: only eventName, location and dates are ever filled.
	// A sentence ASSERTING one must carry a placeholder so the OMIT rule can drop it.
	// Every noun the briefs promise that the pipeline cannot supply. Extended after a mutation
	// showed the first list caught only two of four known-bad lines: "Event guide and schedule
	// inside" and a "Watch Recordings" CTA both slipped through because the words were absent
	// from the list, not because the guard was wrong.
	claims := []string{
		"early bird", "pricing", "promo code", "discount", "recordings", "3-minute",
		"event guide", "schedule inside", "video library", "watch recordings", "resources",
	}

	// A prohibition is the opposite of a claim. The briefs are full of "No pricing", "NO sales
	// pitch", "Pricing (belongs in Registration Push email)" -- instructions NOT to mention the
	// thing. Flagging those made the guard fire on every stage, which is how a real finding gets
	// ignored. Only affirmative sentences are checked.
	negated := regexp.MustCompile(`(?i)\b(no|not|never|without|remove|avoid|omit|exclude|belongs in)\b`)

	for name, tpl := range Templates {
		fields := map[string]string{
			"SubjectPattern": tpl.SubjectPattern,
			"PreviewPattern": tpl.PreviewPattern,
			"FooterNote":     tpl.FooterNote,
			"ContentPrompt":  tpl.ContentPrompt,
		}
		for i, cta := range tpl.CTAStrategy {
			fields[fmt.Sprintf("CTAStrategy[%d]", i)] = cta
		}
		for field, text := range fields {
			for _, claim := range claims {
				// WORD-boundary: substring matching made "rate" hit "Generate" and "recruitment".
				re := regexp.MustCompile(`(?i)(^|[^\p{L}])` + regexp.QuoteMeta(claim) + `([^\p{L}]|$)`)
				for _, sentence := range claimSentences(text, claim) {
					if !re.MatchString(sentence) || negated.MatchString(sentence) {
						continue
					}
					// The opening "Generate a <Stage> email for ..." line NAMES the stage; it is a
					// label, not a claim made to the reader.
					if strings.HasPrefix(strings.TrimSpace(sentence), "Generate a") {
						continue
					}
					if !strings.Contains(sentence, "[") {
						t.Errorf("stage %q %s asserts %q with no placeholder, so the OMIT rule cannot remove it: %q",
							name, field, claim, strings.TrimSpace(sentence))
					}
				}
			}
		}
	}
}

func claimSentences(text, needle string) []string {
	var out []string
	for _, s := range strings.Split(text, ".") {
		if strings.Contains(strings.ToLower(s), needle) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}
