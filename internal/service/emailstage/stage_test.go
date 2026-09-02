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
		// A third family: PROGRAMME and SOCIAL PROOF. Nothing supplies a schedule, a track list,
		// a session title, an attendee quote or an event statistic, and each is a fact a reader
		// would act on -- a testimonial most of all, since an invented quote attributes words to
		// a person who never said them.
		"session title", "learning track", "tracks", "schedule", "testimonial", "attendee quote",
		"event highlights", "statistics",
	}

	// A prohibition is the opposite of a claim. The briefs are full of "No pricing", "NO sales
	// pitch", "Pricing (belongs in Registration Push email)" -- instructions NOT to mention the
	// thing. Flagging those made the guard fire on every stage, which is how a real finding gets
	// ignored. Only affirmative sentences are checked.
	negated := regexp.MustCompile(`(?i)\b(no|not|never|without|remove|avoid|omit|exclude|belongs in)\b`)

	// The negation must govern the CLAIM, not merely appear somewhere on the line. A parenthetical
	// aside carries its own negatives -- "1-2 attendee testimonials (genuine quotes, not
	// marketing)" is an ORDER to produce quotes whose aside happens to say "not". Matching the
	// whole line let that pass, so the guard reported clean on a directive telling the model to
	// compose attendee quotes and attribute them to people.
	//
	// Stripping parentheticals before the negation test is what separates the two: what remains
	// is the directive itself, and a real prohibition ("No pricing", "Pricing (belongs in
	// Registration Push)") still carries its negative outside the brackets or is caught by the
	// "belongs in" alternative.
	aside := regexp.MustCompile(`\([^)]*\)`)

	// A REDIRECT is a prohibition written as a signpost: the section is banned here and named
	// elsewhere. These live inside the parenthetical, so they survive the strip above.
	redirect := regexp.MustCompile(`(?i)\b(belongs in|no photos|not for this stage)\b`)

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
				// Trailing `s?` so a singular claim also matches its plural. Without it the
				// boundary required a non-letter immediately after the claim, so "testimonial"
				// silently failed to match "testimonials" -- and the guard reported clean on a
				// directive telling the model to compose attendee quotes. Every singular entry in
				// the list carried the same hole.
				re := regexp.MustCompile(`(?i)(^|[^\p{L}])` + regexp.QuoteMeta(claim) + `s?([^\p{L}]|$)`)
				for _, sentence := range claimSentences(text, claim) {
					// A prohibition counts however it is spelled: "No pricing" states it outright,
					// "Pricing (belongs in Registration Push email)" states it inside the aside.
					// Both are checked. What the stripped form rules out is the OPPOSITE case --
					// an affirmative order whose aside merely contains a negative word, like
					// "testimonials (genuine quotes, not marketing)".
					stripped := aside.ReplaceAllString(sentence, " ")
					prohibited := negated.MatchString(stripped) || redirect.MatchString(sentence)
					if !re.MatchString(sentence) || prohibited {
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
					// [EVENT_NAME]/[LOCATION]/[DATES] are the three fields the pipeline always
					// fills (see emailCopyPromptVars), so a bracket that is ONLY one of these
					// proves nothing: the OMIT rule never strips them, and a claim can ride in on
					// a placeholder that can never gate it. The Schedule Announcement headline
					// ("[EVENT_NAME] Schedule is Live") carried exactly this: a claim word
					// ("schedule") plus an always-supplied bracket, with no [SCHEDULE_URL]
					// anywhere on the line.
					if !hasOmittablePlaceholder(sentence) {
						t.Errorf("stage %q %s asserts %q with no placeholder the OMIT rule can act on, so the claim is unconditional: %q",
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
	// Information-hierarchy lines ("headline → intro → CTA") are pure ordering.
	if strings.Contains(t, "→") {
		return true
	}
	// Validation checklist rows are structural -- EXCEPT when the row REQUIRES a specific CTA.
	// "□ Clear pricing comparison" is a formatting rule; `□ One primary CTA: "View Schedule"` is
	// a requirement for a button pointing at a link nothing supplies, and exempting it wholesale
	// let that ship. The box is not what makes a line structural; what it asks for is.
	if strings.HasPrefix(t, "□") {
		return !ctaRequirementRE.MatchString(t)
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

// hasOmittablePlaceholder reports whether the sentence names a placeholder the OMIT rule can
// actually act on -- one standing for a fact nothing supplies.
//
// Two things must NOT count as a gate, and stripping the always-supplied names alone catches only
// the first:
//
//   - [EVENT_NAME]/[LOCATION]/[DATES], which the pipeline fills on every request. This is the
//     Schedule Announcement headline case.
//   - CTA BUTTON TEXT like `[ Register Now ]` or `[ View Full Schedule ]`, which names no fact at
//     all. A residual `[` from one of these satisfied a bare has-a-bracket check, so a claim could
//     ride in on its own button label. Not reachable in today's templates -- the three CTA lines
//     each carry a real [SCHEDULE_URL]/[RECORDINGS_URL] beside them -- but it is the same shape as
//     the bug above, one layer down.
//
// So a gate must be a PLACEHOLDER TOKEN (no spaces inside the brackets, e.g. [SCHEDULE_URL] or
// [Session title 1]) that is not one of the always-supplied three.
func hasOmittablePlaceholder(sentence string) bool {
	for _, m := range placeholderRE.FindAllStringSubmatch(sentence, -1) {
		token := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", "_"))
		if !alwaysSupplied[token] {
			return true
		}
	}
	return false
}

// alwaysSupplied are the placeholders emailCopyPromptVars fills on every request.
var alwaysSupplied = map[string]bool{"EVENT_NAME": true, "LOCATION": true, "DATES": true}

// placeholderRE matches a placeholder TOKEN: `[SCHEDULE_URL]`, `[Session title 1]`. The leading
// character must not be a space, which is what excludes CTA button text (`[ Register Now ]`).
var placeholderRE = regexp.MustCompile(`\[([A-Za-z][A-Za-z0-9_ ]*)\]`)

// Final Countdown must never ask the reader to register.
//
// The stage is for people who have ALREADY registered -- its own SECTIONS TO REMOVE excludes
// "Registration info". Gating the "View Full Schedule" CTA on [SCHEDULE_URL] introduced a
// FALLBACK of "Register Now", and since nothing supplies that URL the fallback fires every time:
// the gating fix quietly turned every Final Countdown email back into a registration push.
//
// Pinned narrowly rather than as a general "stage contradicts its own removals" rule. That
// general form was written first and produced a false finding on Discount Offer, whose removal
// entry ("Registration details -- only the discount matters") bans a SECTION while its whole
// purpose is a discounted-registration CTA. The distinction lives in prose the templates write
// for humans, and a guard that cannot read it reports the wrong stage.
func TestFinalCountdownNeverAsksForRegistration(t *testing.T) {
	t.Parallel()

	tpl, ok := Templates[FinalCountdown]
	if !ok {
		t.Fatalf("Final Countdown template missing")
	}
	// CTAStrategy entries are checked as CTA directives WHOLESALE -- the field IS the call to
	// action, so its lines never contain the literal word "CTA". Requiring that word is what let
	// `"Primary: View Full Schedule -- ... else Register Now"` through: it is the same
	// contradiction as the ContentPrompt one, in the field that most directly drives the button.
	type source struct {
		text      string
		alwaysCTA bool
	}
	sources := []source{{tpl.ContentPrompt, false}, {tpl.SubjectPattern, false}, {tpl.PreviewPattern, false}, {tpl.FooterNote, false}}
	for _, cta := range tpl.CTAStrategy {
		sources = append(sources, source{cta, true})
	}

	for _, src := range sources {
		for _, line := range strings.Split(src.text, "\n") {
			l := strings.TrimSpace(line)
			// In the prompt body only CTA directives are in scope: the removal list itself names
			// registration, correctly. A CTAStrategy line is always in scope.
			if !src.alwaysCTA && !strings.Contains(strings.ToUpper(l), "CTA") {
				continue
			}
			// A PROHIBITION names registration in order to forbid it -- "Do NOT fall back to
			// Register Now", "a registration CTA here contradicts the stage". Those lines are the
			// fix, not the defect, and flagging them makes the guard fire on its own remedy.
			if prohibitionRE.MatchString(l) {
				continue
			}
			if regexp.MustCompile(`(?i)\bregist`).MatchString(l) {
				t.Errorf("Final Countdown has a CTA asking the reader to register, which the stage excludes: %q", l)
			}
		}
	}
}

// prohibitionRE marks a directive that names registration in order to BAN it.
var prohibitionRE = regexp.MustCompile(`(?i)(never|do not|don't|no primary cta|omit|contradicts|excludes)`)

// ctaRequirementRE marks a checklist row that mandates a particular call to action, as opposed to
// one describing layout or tone.
var ctaRequirementRE = regexp.MustCompile(`(?i)(primary|secondary) cta:`)

// Every stage must be able to produce a non-empty CTA with only the three supplied fields.
//
// The service rejects an empty `cta` with a 503 (`GenerateEmailCopy`, the required-fields check),
// and the shared JSON schema asks for one unconditionally. So a stage brief that instructs the
// model to OMIT the primary CTA puts it in an unwinnable position: obey the stage and the
// response is rejected, or obey the schema and violate the stage.
//
// Final Countdown hit exactly that. Gating its "View Full Schedule" CTA on [SCHEDULE_URL] --
// which nothing supplies -- left "otherwise omit the primary CTA" as the always-taken branch, so
// the stage could not satisfy the endpoint contract at all. The fix that removed one
// contradiction created a worse one, which is why this is pinned rather than reasoned about.
func TestEveryStageCanProduceACTA(t *testing.T) {
	t.Parallel()

	// "the" is OPTIONAL. The enforcement lines write "else NO primary CTA" without it, so
	// requiring the article let a second instance of this exact defect survive in the same
	// template the first one was fixed in -- the guard reported clean on the line below the one
	// it had just proven.
	omitCTA := regexp.MustCompile(`(?i)\b(omit|no|remove|drop)\s+(the\s+)?(primary\s+)?cta\b`)
	for name, tpl := range Templates {
		for _, text := range append([]string{tpl.ContentPrompt}, tpl.CTAStrategy...) {
			for _, line := range strings.Split(text, "\n") {
				l := strings.TrimSpace(line)
				// A prohibition on omitting is the fix, not the defect.
				if strings.Contains(strings.ToUpper(l), "NEVER OMIT") {
					continue
				}
				if omitCTA.MatchString(l) {
					t.Errorf("stage %q instructs the model to omit the CTA, which the schema requires and the service rejects when empty: %q", name, l)
				}
			}
		}
		// Every stage must name at least one primary CTA reachable with no unsupplied fact --
		// i.e. a CTA phrase that is not gated behind a placeholder the pipeline never fills.
		if len(tpl.CTAStrategy) == 0 {
			t.Errorf("stage %q declares no CTAStrategy, so nothing tells the model what button to write", name)
		}
	}
}

// A stage's CTA fallback must be the SAME phrase everywhere it is named.
//
// Each stage states its CTA in up to four places: the numbered hierarchy, the CTA ENFORCEMENT
// list, the validation checklist, and CTAStrategy. Fixing one and missing another is what
// happened twice on Final Countdown -- the hierarchy said "See You There" while the enforcement
// line still said "NO primary CTA", so the model got two contradictory instructions for the
// always-taken branch. Reviewers caught both; nothing in the repo did.
//
// Keyed on the fallback phrase rather than parsing the grammar: if a stage names a gated CTA at
// all, every surface that names a fallback must name the same one.
func TestStageCTAFallbacksAgree(t *testing.T) {
	t.Parallel()

	// The phrases a fallback can be. A stage uses at most one.
	fallbacks := []string{"See You There", "Share Feedback", "Register Now"}

	for name, tpl := range Templates {
		text := tpl.ContentPrompt + "\n" + strings.Join(tpl.CTAStrategy, "\n")
		var seen []string
		for _, f := range fallbacks {
			// Only count it as this stage's fallback when it appears in an else/otherwise clause.
			if regexp.MustCompile(`(?i)(else|otherwise)[^\n]{0,40}` + regexp.QuoteMeta(f)).MatchString(text) {
				seen = append(seen, f)
			}
		}
		if len(seen) > 1 {
			t.Errorf("stage %q names %d different CTA fallbacks (%s); the surfaces disagree and the model gets contradictory instructions",
				name, len(seen), strings.Join(seen, ", "))
		}
	}
}

// Every CTA phrase in a stage's prose must be one the stage DECLARED.
//
// This is the structural fix for a defect that shipped three times in one day. Each stage states
// its call to action in up to four hand-written places -- the numbered hierarchy, the CTA
// ENFORCEMENT list, the validation checklist, and CTAStrategy -- and fixing one while missing
// another gave the model two contradictory instructions for the branch that always runs. Final
// Countdown said "See You There" on one line and "NO primary CTA" on the next; before that it
// said "Register Now" to readers who already held a ticket. Every instance was caught by a
// reviewer, none by the repo.
//
// `PrimaryCTA` and `PrimaryCTAFallback` are now the single source of truth. This walks the prose
// and fails when a CTA directive names a button that is neither, so a drifted surface breaks the
// build instead of reaching the model.
func TestStageCTAPromptMatchesDeclaration(t *testing.T) {
	t.Parallel()

	// A CTA directive: the numbered hierarchy line, an ENFORCEMENT bullet, or a checklist row.
	// "CTA" is OPTIONAL after primary/secondary. The enforcement lists write both
	// `- 1 PRIMARY CTA: "..."` and `- 1 OPTIONAL SECONDARY: "..."`, and requiring the word let a
	// drifted secondary through -- Post-Event declared "Share Your Feedback" but one line said
	// "Share Feedback", which is also its primary fallback, so the model was told to write the
	// same button twice. This is the second blind spot of this exact shape (the first needed
	// "the" to be optional), so the pattern is now permissive about the wording around the noun.
	directive := regexp.MustCompile(`(?i)^(\d+\.\s*)?(-\s*\d+\s*|□\s*(one|maximum \d+)\s*)?(optional\s+)?(primary|secondary)(\s+cta)?\b`)
	// The button text itself, in [ Brackets ] or "Quotes".
	button := regexp.MustCompile(`\[\s*([A-Z][^\]\[]{2,40}?)\s*\]|"([^"]{3,40})"`)

	for name, tpl := range Templates {
		if strings.TrimSpace(tpl.PrimaryCTA) == "" {
			t.Errorf("stage %q declares no PrimaryCTA", name)
			continue
		}
		// A gated CTA needs a fallback, because the placeholder it names is never supplied and an
		// empty `cta` is refused by the service with a 503.
		if strings.Contains(tpl.PrimaryCTA, "[") && strings.TrimSpace(tpl.PrimaryCTAFallback) == "" {
			t.Errorf("stage %q gates its PrimaryCTA on a placeholder but declares no fallback; the gated branch never runs and an empty cta is a 503", name)
		}

		allowed := map[string]bool{norm(tpl.PrimaryCTA): true}
		if f := strings.TrimSpace(tpl.PrimaryCTAFallback); f != "" {
			allowed[norm(f)] = true
		}
		// EXACT match, no prefix tolerance. Accepting "Register" for a declared "Register to
		// Attend" would tolerate exactly the drift this exists to stop -- the model would still
		// receive two different button texts for one button.
		if sec := strings.TrimSpace(tpl.SecondaryCTA); sec != "" {
			allowed[norm(sec)] = true
		}
		for _, c := range tpl.CTAStrategy {
			for _, m := range button.FindAllStringSubmatch(c, -1) {
				allowed[norm(pick(m))] = true
			}
		}

		for _, line := range strings.Split(tpl.ContentPrompt, "\n") {
			l := strings.TrimSpace(line)
			if !directive.MatchString(l) {
				continue
			}
			// ROLE-AWARE. A SECONDARY line may only name the declared secondary, and a PRIMARY
			// line only the primary or its fallback. Checking against one combined set let
			// Post-Event label its secondary "Share Feedback" -- which is legitimate as its
			// PRIMARY fallback, so the combined set accepted it -- telling the model to write the
			// same button twice when recordings are unavailable.
			role := allowed
			if regexp.MustCompile(`(?i)\bsecondary\b`).MatchString(l) {
				role = map[string]bool{}
				if sec := strings.TrimSpace(tpl.SecondaryCTA); sec != "" {
					role[norm(sec)] = true
				}
			}
			for _, m := range button.FindAllStringSubmatch(l, -1) {
				got := norm(pick(m))
				// Placeholder tokens are shape, not button text. UPPER_SNAKE inside the brackets
				// is what distinguishes `[SCHEDULE_URL]` from `[ View Full Schedule ]`; the
				// earlier prefix check only caught nested brackets and let a lone placeholder on
				// a CTA line read as a drifted label.
				if got == "" || placeholderTokenRE.MatchString(got) {
					continue
				}
				if !role[got] {
					t.Errorf("stage %q names CTA %q in its prompt, which is neither its PrimaryCTA (%q), its fallback (%q), nor a declared secondary: %q",
						name, got, tpl.PrimaryCTA, tpl.PrimaryCTAFallback, l)
				}
			}
		}
	}
}

// pick returns whichever capture group matched.
func pick(m []string) string {
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// norm lowercases and collapses whitespace so "View Full Schedule" and "view  full schedule"
// compare equal; the prose is hand-written and its spacing varies.
func norm(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// placeholderTokenRE matches an UPPER_SNAKE placeholder name, so it is not mistaken for a button.
var placeholderTokenRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// A CTA fallback must not share a sentence with an unsupplied placeholder.
//
// The system prompt's OMIT rule removes any sentence whose placeholder has no supplied value, and
// it outranks the stage brief. So `PRIMARY CTA: [ View Full Schedule ] with [SCHEDULE_URL], else
// [ See You There ]` is self-defeating: the token is never supplied, so the model may drop the
// whole line -- taking the fallback with it and leaving no CTA at all, which the service refuses
// with a 503.
//
// This is the third distinct route to the same empty-CTA failure (the first was a "Register Now"
// fallback that contradicted the stage, the second an explicit "omit the CTA" instruction), so it
// is pinned rather than reasoned about. The fallback must live on its own line, naming no
// placeholder, where OMIT cannot reach it.
func TestCTAFallbacksSurviveTheOmitRule(t *testing.T) {
	t.Parallel()

	placeholder := regexp.MustCompile(`\[[A-Z][A-Z0-9_]*\]`)
	fallbackWord := regexp.MustCompile(`(?i)\b(else|otherwise|without it)\b`)

	for name, tpl := range Templates {
		fb := strings.TrimSpace(tpl.PrimaryCTAFallback)
		if fb == "" {
			continue
		}
		for _, text := range append([]string{tpl.ContentPrompt}, tpl.CTAStrategy...) {
			for _, line := range strings.Split(text, "\n") {
				l := strings.TrimSpace(line)
				if !strings.Contains(l, fb) {
					continue
				}
				// The line names the fallback. If it also names an unsupplied placeholder AND
				// reads as a conditional, OMIT can delete the fallback along with the condition.
				if placeholder.MatchString(l) && fallbackWord.MatchString(l) {
					t.Errorf("stage %q states its CTA fallback %q on the same line as %s; the OMIT rule can drop that whole sentence and leave no CTA: %q",
						name, fb, placeholder.FindString(l), l)
				}
			}
		}
	}
}
