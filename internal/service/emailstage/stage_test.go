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
	// Two families, both unsupplied. The first is COMMERCIAL -- prices, codes, deadlines. The
	// second is EDITORIAL -- speakers, talk types, transit, things to bring. Only the first was
	// listed originally, so a template ordering the model to "List 3-4 top speakers" passed a
	// guard whose whole purpose is to stop it inventing exactly that. Nothing supplies either
	// family: `emailCopyPromptVars` carries eventName, location and dates, and no more.
	claims := []string{
		"early bird", "pricing", "promo code", "discount", "recordings", "3-minute",
		"event guide", "schedule inside", "video library", "watch recordings", "resources",
		"speakers", "speaker name", "talk type", "what to bring", "getting there",
		"transit", "parking", "selection criteria",
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
					// STRUCTURAL directives are instructions to the MODEL about how to organise a
					// section -- a checklist row, a section heading, a formatting rule, an
					// information-hierarchy line. They name a topic; they do not assert a fact to
					// the reader, and a placeholder in them would be meaningless. What this guard
					// is for is the opposite: a line that puts an unsupplied fact in front of the
					// reader, like a required "Watch Recordings" CTA with no [RECORDINGS_URL].
					//
					// Without this the guard reported 15 formatting rows alongside the 4 real
					// findings, which is how a real finding gets ignored -- the same reason the
					// `negated` filter above exists.
					if isStructuralDirective(sentence) {
						continue
					}
					// A line EXPLAINING the policy is not a line enacting it. The templates carry
					// prose telling the model why a section is dropped ("Nothing supplies talk
					// types ... so this section is normally DROPPED"), and matching the topic word
					// inside that explanation flags the fix as the defect.
					if explanatoryRE.MatchString(sentence) {
						continue
					}
					// "supplied"/"only if" already names the gate; the placeholder that enforces it
					// sits on the section HEADING a line or two above, which a per-line check
					// cannot see. Checking the line alone reported "List 3-4 of the supplied
					// speakers" as unguarded while the heading directly above it read
					// "5. FEATURED SPEAKERS -- ONLY if [SPEAKER_NAMES] is supplied".
					if gatedRE.MatchString(sentence) {
						continue
					}
					// An OBJECTIVE names what the email is for ("PRIMARY OBJECTIVE: Recruit
					// speakers"). It promises the reader nothing.
					if strings.Contains(sentence, "PRIMARY OBJECTIVE") || strings.Contains(sentence, "OBJECTIVE:") {
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

// claimSentences returns every unit of `text` that mentions `needle`.
//
// Splits on NEWLINES as well as periods. These templates are newline-delimited directives, not
// prose: "4. PRIMARY CTA: [ Watch Recordings ]" and a nearby "[LINK]" resource line share no
// period between them, so a period-only split merged whole sections into one "sentence" and any
// stray bracket anywhere in that blob satisfied the has-a-placeholder check for every claim
// inside it. The guard passed on a Post-Event contradiction that was genuinely present.
func claimSentences(text, needle string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(text, func(r rune) bool { return r == '.' || r == '\n' }) {
		if strings.Contains(strings.ToLower(s), needle) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// isStructuralDirective reports whether a line tells the model how to STRUCTURE the email rather
// than stating a fact to its reader.
//
// Deliberately narrow: it keys on the shapes these templates use for scaffolding -- the validation
// checklist, a numbered or all-caps section heading, an information-hierarchy arrow line, and the
// "Use ... format" / "X comes after Y" ordering rules. A sentence that merely mentions a topic in
// prose is NOT structural and still has to carry a placeholder.
func isStructuralDirective(sentence string) bool {
	t := strings.TrimSpace(sentence)
	if t == "" {
		return true
	}
	// Validation checklist rows ("□ Clear pricing comparison") and information-hierarchy
	// lines ("headline → intro → CTA").
	if strings.HasPrefix(t, "□") || strings.Contains(t, "→") {
		return true
	}
	// Ordering and formatting rules.
	lower := strings.ToLower(t)
	for _, marker := range []string{"use bullet", "use structured", "format for", "comes after", "comes early", "comes before", "scannable format"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// A SECTION HEADING: "5. PRICING SECTION (40-60 words...)". The parenthesised word budget is
	// what makes it a heading rather than a sentence.
	if headingRE.MatchString(t) {
		return true
	}
	// A bare topic row in a "SECTIONS TO REMOVE" style list: short, dash-led, no assertion.
	return strings.HasPrefix(t, "-") && len(strings.Fields(t)) <= 6
}

var headingRE = regexp.MustCompile(`^([0-9]+\.\s*)?[A-Z][A-Z0-9 '/&-]{3,}\s*\(`)

// explanatoryRE marks a line that DESCRIBES the omit policy rather than asserting a fact. These
// are the sentences added to make a section conditional; flagging them would report the fix as
// the bug.
var explanatoryRE = regexp.MustCompile(`(?i)(nothing supplies|normally DROPPED|never invent|do not invent|inventing one|only from \[|omit (the line|entirely)|guessing them|is a SELECTION POLICY)`)

// gatedRE marks a line that defers to a supplied value. The placeholder enforcing it usually sits
// on the section heading above, so the line itself carries no bracket.
var gatedRE = regexp.MustCompile(`(?i)(the supplied|supplied [a-z]|only if|only with|when .* is supplied|from \[)`)
