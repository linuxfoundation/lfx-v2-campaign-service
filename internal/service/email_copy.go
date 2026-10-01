// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode/utf8"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
)

// emailCopyEventDetails is the slice of a brief's EventDetails this generation needs.
// It mirrors the pattern in audience_build.go and is decoded opportunistically.
type emailCopyEventDetails struct {
	EventName string `json:"eventName"`
	Location  string `json:"location"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	// Dates is the COMBINED form the scraper produces -- "19-20 November 2026" -- which is what
	// briefs written by the UI actually carry. `startDate`/`endDate` are paid-platform config
	// fields and are empty on an email brief, so reading only those yielded "Date TBD" for every
	// email while the real dates sat in the brief beside them. Verified on a live brief.
	Dates string `json:"dates"`
	// RegistrationURL is a FALLBACK for the brief's top-level url column, read for the same
	// reason decodeBriefFields (internal/dispatch) reads it: some briefs carry the event's
	// destination only inside this blob. See resolveRegistrationURL for the precedence.
	RegistrationURL string `json:"registrationUrl"`
	// Speakers is the named speakers the UI's event scrape found. `speakers` is a real key of
	// CampaignEventDetails, so this now decodes populated on new briefs; it arrived empty on every
	// brief built before the scrape's extraction prompt was asked for it, which is why the prompt
	// omits the line rather than printing an empty one.
	Speakers []string `json:"speakers"`
	// Topics mirrors the key the prototype's content generator reads from the same shape
	// (backend/core/agent.py: event_details.get("topics")). NOTHING writes it on this path --
	// CampaignEventDetails has no `topics` field; the UI calls the same data `themes` below -- so it
	// is read only because event_details is an opaque `Any` blob and a brief from some other
	// producer that does set the key should not need a schema change to benefit from it. topicList
	// merges the two.
	Topics []string `json:"topics"`
	// Everything below is what the UI ACTUALLY writes into this blob -- toUpstreamEventDetails
	// (campaign-service.service.ts) spreads the whole CampaignEventDetails and adds three more keys
	// -- and every one of these was being decoded away and discarded. The generator read none of
	// them, so an email about a three-day conference with a published agenda, a stated audience and
	// six themes was written from its name, its city and its dates. See eventFactsBlock.
	//
	// Description is the event's own summary of itself, 1-3 sentences of scraped prose. It is the
	// only unbounded value here and the reason the block has its own size bound.
	Description string `json:"description"`
	// Themes is the key the UI writes for the event's subject matter, and the one that carries it on
	// every brief this service sees. Merged with `Topics` above by topicList rather than one winning.
	Themes []string `json:"themes"`
	// Audience is who the event is for ("platform engineers, SREs") and FormatNotes how it runs
	// ("two days of talks plus a hands-on day"). Short free text from the scrape.
	Audience    string `json:"audience"`
	FormatNotes string `json:"formatNotes"`
	// The four secondary links. Each is a URL the event page ITSELF publishes: the frontend only
	// fills these from a candidate that literally appears in an href on the fetched page
	// (verifyPageLink in apps/lfx-one/src/server/helpers/event-links.helper.ts), because a model
	// asked for "the agenda URL" will otherwise compose a plausible one from the site's shape.
	//
	// That check happens in the writer, not here, so this service validates them again on the way
	// into a prompt -- see factURL. A link that fails either check is absent, never a guess.
	AgendaURL      string `json:"agendaUrl"`
	CFPURL         string `json:"cfpUrl"`
	VenueURL       string `json:"venueUrl"`
	SponsorshipURL string `json:"sponsorshipUrl"`
	// Sponsors is the logo set the scrape found. Only the names are used in copy; see sponsorNames.
	Sponsors []emailCopySponsor `json:"sponsors"`
}

// emailCopySponsor is one entry of the brief's sponsor list, declared to match the frontend's
// CampaignEventSponsor so the blob decodes without a shape mismatch. LogoURL is decoded and
// deliberately unused by copy generation: the dispatcher renders the logos as image tiers
// (addSponsorTier in internal/platform/hubspot/content.go), and the email-copy schema has no image
// section at all, so a logo URL in the prompt could only invite an <img> that gets rejected.
type emailCopySponsor struct {
	Name    string `json:"name"`
	LogoURL string `json:"logoUrl"`
}

// maxPromptSize bounds the CALLER-supplied fields, checked before composing. That is eventName,
// location and dates always, plus registrationURL on a stage that actually formats it -- see the
// guard in GenerateEmailCopy for why the URL is counted conditionally rather than always.
const maxPromptSize = 2400 // runes

// maxComposedPromptSize bounds the WHOLE composed prompt, checked after.
//
// It must stay at or above (worst stage floor + maxPromptSize) or a caller is told their input is
// too large by the second check after the first accepted it -- with a 503 that blames the service.
// TestComposedBoundClearsEveryStageFloor computes the floors and enforces exactly that.
//
// 9000, not 8400. At 8400 the margin was 24 runes: Post-Event floors at 6078 and the input
// allowance is 2400, so the worst valid composition is 8478. A single sentence added to any
// template would have pushed valid caller input into a 503 -- and this session added 473 runes of
// template text, consuming almost the entire previous margin without noticing until a reviewer
// pointed at a stale figure three edits later.
//
// The margin is measured in SECTIONS, not in "some slack": roughly 522 runes is the size of the
// largest single section in a template brief, so a margin of about that says "one more section
// can be written before the bound has to move again", and a margin below it says the next edit
// to any template silently turns valid caller input into a 503.
//
// 9300, not 9000. The link rule added to the shared system prompt grew every stage's floor by
// 267 runes, taking Post-Event from 6078 to 6345 and the worst valid composition from 8478 to
// 8745 -- which left 255 runes under 9000, less than half a section. This is the fourth time
// prompt text has eaten this margin, so the bound moves WITH the text rather than after a
// reviewer notices: 9300 restores a section of headroom.
//
// The worst valid composition is now 8744, and the path there is worth keeping because it moved
// three times. The registrationURL line adds 19 runes ("\nRegistration URL: ") when a URL is
// present and is omitted entirely when one is not, which put the with-URL figure at 8764 while the
// no-URL floor was 8745 -- so the bound had to clear the LARGER. It then moved again when stages
// gained a link policy: Post-Event, the worst floor at 6344, WITHHOLDS the URL, so the line it was
// measured with is no longer part of its composition and the worst valid composition fell to 8788,
// leaving 512 runes of headroom.
//
// Post-Event leads on content-prompt length, not on that line, so dropping 19 runes did not change
// WHICH stage is worst -- only the number. TestComposedBoundClearsEveryStageFloor and
// TestConceptDocSizingArithmetic both COMPUTE these; the latter caught this exact figure going
// stale the moment the link policy landed, which is why neither transcribes a constant.
// maxComposedPromptSize was raised from 9300 to 11100 when the reference-email style block
// (EmailReferenceSource) was added: that block is server-fetched and hard-truncated to
// maxReferenceBlockRunes, so it is a FLOOR contributor like the fixed stage templates above, not
// caller input guarded by maxPromptSize. worstStageFloorNamed (email_copy_test.go) now composes
// with a maximally-sized reference block, so TestComposedBoundClearsEveryStageFloor and
// TestConceptDocSizingArithmetic both re-derive this bound the same way every earlier revision
// documented above was re-derived; see those tests and their comments for the current numbers.
//
// Raised again, from 11700 to 14000, when the urgency-fomo variant block was added
// (composeEmailCopyPrompt): that block is fixed prompt text appended only when
// emailCopyPromptVars.variant matches urgencyFomoVariant, so it is a FLOOR contributor exactly
// like a stage template, not caller input. worstStageFloorNamed now composes each stage BOTH with
// and without the variant and takes the max, which moved the worst case from Post-Event alone
// (8770) to Post-Event with the variant appended (11055) -- 2285 runes higher. 14000 clears
// (11055 + maxPromptSize) with headroom in the same ~500-rune-per-section range every earlier
// revision aimed for; see TestConceptDocSizingArithmetic for the exact current figures.
//
// Raised again, from 14000 to 14600, when the segment-conditional content-block guidance was
// added (composeEmailCopyPrompt): each emailSegment* block is fixed prompt text appended only
// when emailCopyPromptVars.segment exactly matches one of the recognised values, so it is a FLOOR
// contributor exactly like the stage template and variant block, not caller input. The same change
// also added a "scannable rich_text" rule to the shared stage-aware system prompt, which applies
// to EVERY stage-aware request regardless of segment (~346 runes) -- the segment block itself
// contributes the rest (~288 runes for the alumni block, the largest of the four).
// worstStageFloorNamed now composes each stage across BOTH variant-on/off AND every recognised
// segment (plus none) and takes the max, which moved the worst case from Post-Event with the
// variant alone to Post-Event with the variant AND the alumni segment block appended -- 11689
// runes, 634 higher than the variant-only floor (346 from the shared rule, 288 from the segment
// block). 14600 clears (11689 + maxPromptSize = 14089) with headroom in the same ~500-rune range
// every earlier revision aimed for; see TestConceptDocSizingArithmetic for the exact current
// figures.
//
// Raised again, from 14600 to 20400, by the richer-content work: this is the largest single move
// this bound has made, and all three contributors are measured, not estimated.
//
//   - The event-facts block (+3600, exactly maxEventFactsBlockRunes). Like the reference block it
//     is producer-bounded and reaches EVERY stage-aware request, so worstStageFloorNamed composes
//     with a maximally-sized one. It is the dominant term: half the increase is this block alone.
//   - The community-story variant block (+305 over urgency-fomo). Adding a second variant did not
//     add its size to the floor -- only one variant is ever appended -- it only moved WHICH variant
//     is worst, by the difference between the two blocks.
//   - The shared stage-aware rules (+1758 net), of which bodyStyleRule is 624. The rest is the
//     secondary-links rule, the use-every-supplied-detail rule, and the one-idea-per-section and
//     per-section-heading rules. Net, because the same change also removed the now-unused
//     speakers/topics prompt vars; I have not split it finer than measured.
//
// Worst case moved from Post-Event +urgency-fomo +alumni (11689) to Post-Event +community-story
// +alumni (17352), so the worst valid composition is 19752. 20400 clears it by 648 runes -- just
// over the ~522-rune largest-single-section margin the note above defines, so one more section can
// be written before this bound has to move again.
const maxComposedPromptSize = 20400 // runes

// maxReferenceBlockRunes bounds the reference-email style block EmailReferenceSource builds from
// up to three past sent HubSpot emails (see email_reference.go). It reaches the stage-aware user
// prompt only (composeEmailCopyPrompt), never the frozen legacySystemPrompt path, for the same
// LFXV2-1940 byte-identity reason registrationURL and eventFacts are stage-path-only.
//
// The block is TRUNCATED to this bound by EmailReferenceSource itself before it ever reaches
// composeEmailCopyPrompt, so it behaves as a fixed-size floor contributor (like a stage template)
// rather than caller input — it is counted in maxComposedPromptSize, not maxPromptSize.
const maxReferenceBlockRunes = 1800 // runes

// The event-facts block's size bounds. Every value in it comes out of the brief's `event_details`
// column, which is declared `Any` in design/brief.go and stored as TEXT, so not one of these fields
// carries a length constraint anywhere along its path from an arbitrary event page to here.
//
// Bounded HERE, by the producer, exactly as maxReferenceBlockRunes above is -- which makes the block
// a fixed-size FLOOR contributor to maxComposedPromptSize rather than caller input guarded by
// maxPromptSize. eventFactsBlock explains why counting a scraped description as caller input would
// reject legitimate briefs with a 400 that names the wrong field.
const (
	// maxEventFactsBlockRunes bounds the whole rendered block. It is a BACKSTOP: the per-field
	// bounds below already sum under it, which TestEventFactsBlockHonoursItsBound measures rather
	// than taking on faith from this comment.
	maxEventFactsBlockRunes = 3600 // runes
	// maxFactDescriptionRunes bounds the event's own description of itself -- the one genuinely long
	// fact, and most of the reason this block exists. 900 runes is roughly 150 words: more than any
	// event page's summary paragraph, far less than its about page.
	maxFactDescriptionRunes = 900 // runes
	// maxFactTextRunes bounds every other text fact, and each list fact after joining.
	maxFactTextRunes = 300 // runes
	// maxFactURLRunes bounds one secondary link; factURL explains why an over-long one is dropped
	// rather than truncated.
	maxFactURLRunes = 250 // runes
	// maxFactListEntries bounds how many entries a single list fact contributes. It is what keeps
	// the length truncation from landing mid-entry often enough to matter: a brief carrying sixty
	// themes contributes the first twelve whole, not thirty-one and a half.
	maxFactListEntries = 12
)

// emailCopyPromptVars holds the values needed to compose the generation prompt.
type emailCopyPromptVars struct {
	eventName string
	location  string
	dates     string
	// registrationURL is the event destination the generated call-to-action links to, already
	// validated by resolveRegistrationURL. EMPTY means the brief has none, and the prompt then
	// omits the Registration URL line entirely -- the link rule keys off that absence.
	//
	// This reaches the STAGE-AWARE prompt only. The frozen legacy prompt cannot carry it
	// without breaking LFXV2-1940 byte-identity, so a caller that sends no stage still gets
	// copy written without a destination.
	registrationURL string
	// eventFacts is every other fact the brief supplies about the event -- its description,
	// audience, format, themes, speakers, sponsors and secondary links -- ALREADY RENDERED as
	// labelled lines and already truncated to maxEventFactsBlockRunes by
	// emailCopyEventDetails.eventFactsBlock. composeEmailCopyPrompt neither formats nor re-bounds
	// it; it appends the string or, when it is empty, nothing at all.
	//
	// Arriving pre-rendered is the point: ONE function decides what a fact looks like in a prompt
	// and what happens when it is missing, instead of that decision being spread across a struct
	// of typed fields and a format string. An EMPTY value means the brief supplied nothing beyond
	// name, location and dates.
	//
	// Reaches the STAGE-AWARE user prompt only, same restriction as registrationURL above and for
	// the same reason: the frozen legacy prompt cannot grow without breaking LFXV2-1940
	// byte-identity.
	eventFacts string
	// referenceBlock is the style/tone corpus EmailReferenceSource builds from up to three past
	// sent HubSpot emails, already truncated to maxReferenceBlockRunes. EMPTY means none was found
	// (no HubSpot connection, no published emails, or the lookup failed) — best-effort, never a
	// hard dependency of email-copy generation. Reaches the STAGE-AWARE user prompt only, same
	// restriction as registrationURL/eventFacts above and for the same LFXV2-1940 reason.
	referenceBlock string
	// stage selects the generation spec. TWO distinct paths, deliberately not one:
	//
	//   - EMPTY (or blank) means the caller did not say. `composeEmailCopyPrompt` returns the
	//     frozen legacy prompt and never calls Resolve, because LFXV2-1940 requires a caller that
	//     sends no stage to keep receiving byte-identical prompts.
	//   - NON-EMPTY but unrecognised resolves to emailstage.DefaultStage rather than erroring,
	//     so a caller is never blocked by a stage it cannot spell -- see Resolve.
	//
	// Collapsing the first into the second is the edit to avoid: it silently changes the prompt
	// every existing caller receives.
	stage string
	// variant requests a differently-STYLED draft of the same stage's copy, orthogonal to stage
	// (which sets WHAT the email is for). Same two-path shape as stage, for the same reason:
	//
	//   - EMPTY (or blank) means no variant was requested; composeEmailCopyPrompt appends nothing
	//     and the stage's own prompt is unchanged.
	//   - Recognised (urgencyFomoVariant or communityStoryVariant) appends that variant's content
	//     block below. Anything else, including unrecognised text, is silently ignored -- same
	//     leniency as an unrecognised stage, so a caller that misspells it still gets ordinary copy
	//     rather than an error.
	//
	// Reaches the STAGE-AWARE prompt only: an absent stage already takes the frozen legacy path
	// above and this field is never consulted there, so a variant request alongside no stage is
	// silently a no-op rather than a second way to grow the LFXV2-1940-frozen prompt.
	variant string
	// segment requests which CONTENT BLOCKS appear for a specific audience, orthogonal to both
	// stage (WHAT the email is for) and variant (HOW the whole draft is styled). Same two-path
	// shape as stage and variant, for the same reason:
	//
	//   - EMPTY (or blank) means no segment was requested; composeEmailCopyPrompt appends nothing
	//     and the stage's (and variant's, if any) own prompt is unchanged.
	//   - Recognised (one of the emailSegment* constants below) appends that segment's
	//     block-inclusion guidance. Anything else, including unrecognised text, is silently
	//     ignored -- same leniency as an unrecognised stage or variant, so a caller that misspells
	//     it still gets ordinary copy rather than an error.
	//
	// Composes ADDITIVELY alongside variant: variant restructures the whole draft's framing,
	// segment narrows which blocks within that draft are relevant to a named audience. Both may
	// be set together, either alone, or neither.
	//
	// Reaches the STAGE-AWARE prompt only, same restriction as variant and for the same reason:
	// an absent stage already takes the frozen legacy path above and this field is never
	// consulted there.
	segment string
}

// The recognised values of emailCopyPromptVars.variant. Each asks for the SAME stage's copy
// restructured toward a different framing -- not a different stage and not a different set of
// facts.
//
// The two exist to be A/B tested against each other, which is the whole reason the second one is
// written to the same depth as the first. A dispatch that sends variant A with eleven ordered
// sections and variant B with nothing is not an experiment in framing: whichever wins, the result
// only says that a structured email beats an unstructured one. So communityStoryVariant carries its
// own eleven-section structure, and its block says so explicitly where a model might otherwise
// treat "softer angle" as "shorter email".
//
// They are deliberately opposed on urgency. urgencyFomoVariant leans on deadline pressure and
// scarcity; communityStoryVariant is FORBIDDEN from using either, even when the stage's own tone
// would permit it. Without that prohibition both variants converge on the same persuasion -- the
// urgency framing is the easiest thing for a model to reach for -- and the test measures nothing.
const (
	urgencyFomoVariant    = "urgency-fomo"
	communityStoryVariant = "community-story"
)

// The recognised values of emailCopyPromptVars.segment. Each narrows which content blocks
// composeEmailCopyPrompt's segment guidance asks the model to keep or drop for a named audience;
// none of them changes the underlying facts, stage or variant framing.
const (
	emailSegmentDeveloper             = "developer"
	emailSegmentBusinessDecisionMaker = "business-decision-maker"
	emailSegmentAlumni                = "alumni"
	emailSegmentProspect              = "prospect"
)

// decodeEmailCopyEventDetails pulls the fields email generation needs from the brief's opaque
// EventDetails blob. Unlike audience_build.go (which skips mismatched shapes), this function
// returns an error for missing or invalid event details, causing GenerateEmailCopy to return
// a 400 response.
func decodeEmailCopyEventDetails(blob json.RawMessage) (emailCopyEventDetails, error) {
	var details emailCopyEventDetails
	if len(blob) == 0 {
		return details, errors.New("event details are empty")
	}
	if err := json.Unmarshal(blob, &details); err != nil {
		return details, fmt.Errorf("invalid event details: %w", err)
	}
	eventName := strings.TrimSpace(details.EventName)
	if eventName == "" {
		return details, errors.New("event details have no eventName; copy generation requires it")
	}
	return details, nil
}

// eventFactsBlock renders every event fact beyond name/location/dates as labelled lines, OMITTING
// each one the brief did not supply, and bounds the result at maxEventFactsBlockRunes.
//
// Omission is the point, and it follows the convention the Registration URL line already set: a
// line reading "Audience:" with nothing after it reads as supplied-but-blank and invites the model
// to fill the blank, which is exactly the invention the system prompt's no-invented-facts rule
// forbids. A fact that was not scraped is simply never mentioned.
//
// Every value is bounded BEFORE it is written, so the block-level truncation at the end is a
// backstop the per-field bounds already make unreachable (TestEventFactsBlockHonoursItsBound
// measures that rather than asserting it). Bounding per field is what makes the URLs safe: a URL
// truncated at a rune boundary is a plausible-looking dead link printed into a marketing email, so
// factURL DROPS an over-long one instead, and the only value a whole-block truncation could ever
// reach is the description at the end.
func (d emailCopyEventDetails) eventFactsBlock() string {
	lines := make([]string, 0, 10)
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, label+": "+value)
		}
	}
	// URLs first, each through the same validator the registration URL goes through. The UI already
	// verified that the event page literally links to each of these, but event_details is an opaque
	// blob this service does not own, so it validates rather than trusts -- see factURL.
	add("Agenda URL", factURL(d.AgendaURL))
	add("Call for Proposals URL", factURL(d.CFPURL))
	add("Venue URL", factURL(d.VenueURL))
	add("Sponsorship URL", factURL(d.SponsorshipURL))
	add("Audience", factText(d.Audience, maxFactTextRunes))
	add("Format", factText(d.FormatNotes, maxFactTextRunes))
	add("Topics", factText(joinFactList(d.topicList()), maxFactTextRunes))
	add("Speakers", factText(joinFactList(d.Speakers), maxFactTextRunes))
	add("Sponsors", factText(joinFactList(d.sponsorNames()), maxFactTextRunes))
	// Last deliberately: the only unbounded-at-source value, and so the only one the block-level
	// backstop below could ever cut.
	add("About this event", factText(d.Description, maxFactDescriptionRunes))
	if len(lines) == 0 {
		return ""
	}
	// truncateRunes, not factText: the leading newline is what separates this block from the Dates
	// line above it, and trimming the block would delete it.
	return truncateRunes("\n"+strings.Join(lines, "\n"), maxEventFactsBlockRunes)
}

// topicList is the event's subject matter, merged from both blob keys that carry it.
//
// `themes` is the key the UI's scrape actually writes; `topics` is the key the prototype's
// generator read and the only one this struct used to decode. Reading only `topics` is why no brief
// built through the current flow ever had a topic -- the data sat in the blob under the other name
// the whole time, and the urgency-fomo variant's three topic-conditional sections were correctly
// dropped for want of a fact that had in fact been supplied.
//
// Merged rather than one winning, so a brief carrying both keeps both. De-duplicated
// case-insensitively because two keys describing the same subject is the expected case here, not an
// unusual one.
func (d emailCopyEventDetails) topicList() []string {
	merged := make([]string, 0, len(d.Themes)+len(d.Topics))
	seen := make(map[string]struct{}, len(d.Themes)+len(d.Topics))
	for _, group := range [][]string{d.Themes, d.Topics} {
		for _, entry := range group {
			trimmed := strings.TrimSpace(entry)
			if trimmed == "" {
				continue
			}
			key := strings.ToLower(trimmed)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, trimmed)
		}
	}
	return merged
}

// sponsorNames is the sponsor names worth naming in prose.
//
// A name may legitimately be empty: it comes from a logo's alt text, which plenty of real event
// markup omits, and an unnamed sponsor has nothing to contribute to a sentence -- so it is skipped
// here while the dispatcher still renders its logo.
func (d emailCopyEventDetails) sponsorNames() []string {
	names := make([]string, 0, len(d.Sponsors))
	for _, sponsor := range d.Sponsors {
		if name := strings.TrimSpace(sponsor.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// joinFactList renders a list fact as a comma-separated value, bounded in ENTRY COUNT. Length is
// bounded separately by the caller; see maxFactListEntries for why both bounds exist.
func joinFactList(values []string) string {
	kept := make([]string, 0, maxFactListEntries)
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			kept = append(kept, trimmed)
			if len(kept) == maxFactListEntries {
				break
			}
		}
	}
	return strings.Join(kept, ", ")
}

// factURL validates one secondary event link and DROPS it when it is unusable or over-long.
//
// httpURL carries the validation -- absolute http(s), no embedded credentials, re-encoded query --
// for the same reason resolveRegistrationURL uses it rather than trimming: the value is
// interpolated into a prompt whose output is placed in an href, so a relative path, a `javascript:`
// scheme or an embedded quote must not reach the model.
//
// The length bound lives here and not in httpURL because httpURL's bound is maxPromptSize, the
// whole caller allowance -- four links each that long would be most of the prompt. Dropped rather
// than truncated: a URL cut at a rune boundary is a plausible-looking dead link in a sent email,
// which is worse than a section that simply carries no link.
func factURL(raw string) string {
	validated := httpURL(raw)
	if validated == "" || utf8.RuneCountInString(validated) > maxFactURLRunes {
		return ""
	}
	return validated
}

// factText prepares one fact value for the block: trimmed, bounded by the package's existing
// truncateRunes (email_reference.go), then trimmed AGAIN so a cut landing mid-word does not leave
// the line ending in a space before the next label.
func factText(value string, bound int) string {
	return strings.TrimSpace(truncateRunes(strings.TrimSpace(value), bound))
}

// composeEmailCopyPrompt builds the system and user prompts for email copy generation.
//
// Composes from fixed blocks rather than branching on prompt variants, per the lfx-one reference
// implementation -- with ONE deliberate exception. An absent stage returns the frozen
// `legacySystemPrompt` variant, because LFXV2-1940 requires a caller that sends no stage to keep
// receiving byte-identical prompts, and the stage-aware text is not a superset of the pre-stage
// one. Every stage that IS named composes, as the principle describes; the branch exists only to
// hold the no-stage case still.
// The subject and preheader limits stated to the model — 60 and 100 — are DELIBERATELY tighter
// than the enforced ones. `design/brief.go` caps them at 200 and 150 and `parseEmailCopyResponse`
// truncates to the same, but those are the backstop, not the target: a subject line is cut off
// around 60 characters in most inbox listings and a preheader around 100, so copy written to the
// schema's limit is copy the recipient never sees the end of. Aiming the model at the useful
// length and keeping headroom above it means an overrun is trimmed rather than rejected.
//
// Do not "fix" the mismatch by raising these numbers to 200/150. If they should ever move, the
// question is what renders in an inbox, not what the type allows.
func composeEmailCopyPrompt(vars emailCopyPromptVars) (systemPrompt, userPrompt string) {
	// System prompt: role instruction + constraints + factual grounding.
	// LFXV2-1940 requires that a caller sending no stage produces a byte-identical prompt to the
	// one this service emitted before stages existed. The stage-aware system prompt is not a
	// superset of that text -- it adds the placeholder/OMIT rules, which only mean anything next
	// to a stage brief -- so an absent stage takes the pre-stage prompt verbatim rather than a
	// resolved default. An explicit "Registration Push" still takes the template path.
	if strings.TrimSpace(vars.stage) == "" {
		return legacySystemPrompt, fmt.Sprintf(`Generate email copy for this event:
Event Name: %s
Location: %s
Dates: %s

Create compelling email copy that invites registration and highlights the value of attending.`,
			vars.eventName, vars.location, vars.dates)
	}

	systemPrompt = `You are an expert email copywriter for technology events and communities.
Your task is to generate compelling email copy for a campaign brief.

IMPORTANT: Use ONLY the event details provided below; never invent dates, names, locations,
prices, deadlines, counts, or any other fact. Every factual claim must come directly from what
you're given.

A stage brief below may name a placeholder in [BRACKETS] for a fact that was not supplied --
prices, attendee counts, session counts, deadlines. OMIT any sentence or section whose placeholder
has no supplied value. Do not guess one, and do not emit the bracketed placeholder itself.

THIS RULE OUTRANKS THE STAGE BRIEF. A brief may mark a section REQUIRED and still name a
placeholder in it -- "1. HEADLINE: Early Bird Pricing Ends [DEADLINE]" is required and has no
supplied deadline. Drop the section: a required section whose supporting fact is not among the
event details below cannot be written truthfully. A shorter email that says only what is known is
the correct output, never an invented price, deadline or count.

Generate JSON with these fields (no markdown fencing):
{
  "subject": "Email subject line (max 60 chars)",
  "preheader": "Email preheader text (max 100 chars)",
  "sections": [
    {"type": "rich_text", "html": "Inline HTML, max 8000 chars total across all rich_text sections"},
    {"type": "button", "text": "CTA text (max 50 chars)", "url": "omit if no Registration URL was supplied"}
  ]
}

Constraints:
- Subject: punchy, under 60 characters
- Preheader: summary of the email, under 100 characters
- Sections, not one HTML blob: "rich_text" (inline HTML, no outer <div>/<style>), "button"
  (never an <a> inside rich_text instead), "divider" (no other fields). One button, for the
  primary call to action, unless the brief below asks for another
- Links: the event details below may carry a "Registration URL". A button's "url" must be
  that URL, copied exactly. If no Registration URL is given, omit "url" -- never href="#",
  never invented
- Secondary links: the details may also carry an "Agenda URL", "Call for Proposals URL",
  "Venue URL" or "Sponsorship URL". Link each supplied one ONCE, as an inline <a href="...">
  inside the rich_text section that discusses it, URL copied exactly and link text naming what
  it is ("see the full agenda", not the bare URL). Never substitute one link for another and
  never link a label whose URL was not supplied
- Use every supplied detail the stage can carry. The event's own description, its audience, its
  format, its themes, its speakers and its sponsors are facts to build sections from, not
  optional colour -- prefer one more grounded section over one longer paragraph
- Write for a professional Linux Foundation / technology audience
- Make it about the event and community, not promotional
- No sign-off/signature -- the platform appends its own footer after these sections
- A greeting with a personalization token (e.g. "Hi {{contact.firstname}},") gets the comma
  right after the token, no space before it
- Name any supplied speakers/topics specifically; never "and more" or "and others"
- Make every rich_text section scannable, not a wall of text: short paragraphs (2-3 sentences),
  a bolded lead-in phrase (<strong>) at the start of a paragraph making a distinct point, and a
  <ul>/<li> list wherever three or more parallel items are listed (benefits, speakers, topics,
  agenda highlights) instead of a comma-separated sentence
- One idea per rich_text section -- never run a closed </ul> straight into the next heading with
  no space or tag between them; start the next idea in its own section instead
- Open each rich_text section after the first with an <h2> (or <h3> for a sub-point) naming what
  the section is about, so the email can be skimmed by its headings alone
` + bodyStyleRule

	// Stage-specific guidance, appended to the shared role/constraint block above rather than
	// replacing it: the JSON schema and the length limits hold for every stage, only the intent
	// changes. An unknown NON-EMPTY stage resolves to the default; an ABSENT stage never reaches
	// here at all -- it returned the frozen legacySystemPrompt above.
	tpl := emailstage.Resolve(vars.stage)
	systemPrompt += fmt.Sprintf(`

STAGE: %s
Purpose: %s
Tone: %s
Urgency (1-10): %d
Subject shape: %s
Preheader shape: %s
Call-to-action strategy: %s
%s`,
		tpl.StageName, tpl.Purpose, tpl.Tone, tpl.UrgencyLevel,
		tpl.SubjectPattern, tpl.PreviewPattern,
		strings.Join(tpl.CTAStrategy, "; "), tpl.FooterNote)

	// The urgency/FOMO variant restructures THIS stage's copy toward deadline pressure and social
	// proof; it does not replace the stage's purpose above -- a CFP Launch draft with this variant
	// is still about the CFP, just framed more urgently. Appended, not swapped in, for the same
	// reason the stage block above is appended to the shared role prompt: the JSON schema, the
	// no-invented-facts rule and the length limits hold regardless of variant.
	//
	// "ONLY genuine urgency" restates a rule this file already enforces for every stage (several
	// ContentPrompt templates explicitly say "NO fake urgency") because this variant is the one
	// most likely to invite a model to manufacture a deadline that was never supplied -- it is
	// worth saying twice here.
	//
	// The schema has no image or card section type (see parseEmailCopyResponse): sections are
	// rich_text/button/divider only. Speaker photos, sponsor logos and true visual "cards" from a
	// mockup are approximated as text/emoji structure inside rich_text, not literal images -- that
	// is a schema limit this prompt cannot work around, so it is stated plainly rather than left
	// for the model to improvise.
	switch vars.variant {
	case urgencyFomoVariant:
		systemPrompt += `

VARIANT: urgency-fomo -- restructure this stage's copy toward urgency and FOMO (fear of missing
out), using ONLY the facts supplied above. Never invent a deadline, capacity number, attendee
count or price that was not given -- if the urgency angle needs a fact you were not given, omit
that sentence rather than inventing one.

Structure the sections in this order:
1. Subject: personal, benefit- or curiosity-driven (never generic "Join us at [Event]")
2. Preheader: supports the subject, does not repeat it
3. Hero rich_text: event name, date + location, a strong headline, then the primary button
   ("Register Now" or the stage's own CTA text)
4. "Why attend" rich_text: 3-5 concrete benefits (learn from named speakers/topics if supplied,
   see real implementations, network with peers) -- never generic "great event" language
5. Personal relevance rich_text, ONLY if speakers or topics were supplied: tie a supplied
   topic or speaker to a specific audience (developers, engineering leads, business leads).
   Omit this section entirely if no speakers/topics were supplied -- do not invent an audience
   angle from nothing
6. Speaker/social-proof rich_text, ONLY if speakers were supplied: name them and their supplied
   topics as text (no image placeholders, no "[PHOTO]" markers)
7. Agenda-highlights rich_text, ONLY if topics were supplied: 3-5 sessions, not an exhaustive
   agenda
8. "What you'll experience" rich_text as a short emoji-led list (e.g. Keynotes, hands-on
   sessions, networking, demos) -- only the categories the supplied facts actually support
9. Urgency rich_text: ONLY genuine urgency from a fact actually supplied (a deadline, a stage
   whose tone signals lateness, limited capacity) -- omit this section rather than invent one
10. A secondary button for readers not ready to register (e.g. "View Agenda"), only if the
    brief supplies a destination for it; otherwise omit rather than link it to Register Now
    again
11. A final button repeating the primary call to action

Numbering is for ordering only; do not print "1."/"2." in the output. Each numbered item is its
own rich_text section -- never merge "Why attend" and "What you'll experience" into one section.
Every numbered section whose supporting fact is missing is OMITTED, per the placeholder rule
above -- a shorter email that only says what is known is correct, an invented capacity or
deadline is not.`
	case communityStoryVariant:
		systemPrompt += `

VARIANT: community-story -- restructure this stage's copy toward narrative and community proof,
using ONLY the facts supplied above. This variant's persuasion comes from what the event IS and
who is in the room, not from pressure: do NOT add a deadline, a countdown, a capacity warning or
"last chance" framing, even if the stage's tone would allow it. Never invent a story, a quote, an
attendee number or a named person who was not supplied.

Structure the sections in this order:
1. Subject: curiosity- or theme-driven, naming what the reader would take away (never generic
   "Join us at [Event]", and never a deadline or a scarcity claim)
2. Preheader: supports the subject, does not repeat it
3. Opening rich_text: event name, date + location, then a scene-setting sentence drawn from the
   event's own description or themes if supplied -- what this gathering is about
4. "What this event is" rich_text: 2-4 sentences from the supplied description, audience and
   format -- the substance of the event in prose, not a benefits list
5. Themes rich_text, ONLY if topics/themes were supplied: present them as the conversation the
   event is having, 3-5 of them, each with a clause on why it matters
6. Community rich_text, ONLY if speakers or sponsors were supplied: name who is taking part and
   what they work on, as text (no image placeholders, no "[PHOTO]" markers) -- the point is that
   the reader recognises the room
7. "Who comes to this" rich_text, ONLY if the audience or format facts were supplied: describe
   the peers the reader would meet, grounded in those facts and never in an invented headcount
8. Takeaway rich_text: 3-5 concrete things the reader leaves with, each traceable to a supplied
   theme, speaker, format or description detail -- never generic "great networking"
9. A primary button with the stage's own CTA text
10. Next-steps rich_text, ONLY if a secondary link was supplied (agenda, CFP, venue or
    sponsorship): one sentence per supplied link saying who it is for, with the link inline
11. A closing rich_text inviting the reader in, then a final button repeating the primary call
    to action

Numbering is for ordering only; do not print "1."/"2." in the output. Each numbered item is its
own rich_text section -- never merge "What this event is" and "Takeaway" into one section. Every
numbered section whose supporting fact is missing is OMITTED, per the placeholder rule above.

This variant exists to be A/B tested against urgency-fomo, so it must be as COMPLETE as that one:
reach for one more grounded section before one longer paragraph. If the supplied facts cannot fill
a section, omit that section -- never pad it, and never borrow urgency to fill the gap.`
	}

	// The segment guidance narrows which of the stage's (and, if present, the variant's) sections
	// are relevant to a named audience -- it does not add new facts or change the stage's purpose,
	// only which supplied facts are worth foregrounding versus omitting. Composes ADDITIVELY after
	// the stage block and the variant block, same appended-not-swapped-in reasoning as both: the
	// JSON schema, the no-invented-facts rule and the length limits still hold.
	switch vars.segment {
	case emailSegmentDeveloper:
		systemPrompt += `

SEGMENT: developer -- this reader evaluates the event on session/track substance, not business
value. Keep any agenda, session-track or speaker/topic detail the supplied facts support; drop
sponsorship, ROI or business-case framing entirely rather than including it thin.`
	case emailSegmentBusinessDecisionMaker:
		systemPrompt += `

SEGMENT: business-decision-maker -- this reader evaluates the event on business value, not
session substance. Keep any ROI, sponsorship or business-case framing the supplied facts support;
drop session-level agenda detail (specific talks, tracks, speaker bios) rather than listing it.`
	case emailSegmentAlumni:
		systemPrompt += `

SEGMENT: alumni -- this reader has attended before. Lead with what is NEW or DIFFERENT this time
versus a past edition, using only facts actually supplied -- never invent a comparison to a prior
year that was not given. Skip introductory "what this event is" framing; they already know.`
	case emailSegmentProspect:
		systemPrompt += `

SEGMENT: prospect -- this reader has never attended. Lead with what the event IS and why it
matters before any call to action; do not assume familiarity with past editions, recurring
tracks or the organisation running it.`
	}

	// User prompt: the specific event details and the stage's own content brief.
	//
	// The stage prompt carries [EVENT_NAME]/[LOCATION]/[DATES] placeholders. They are NOT
	// substituted here: the details are already stated above them as labelled facts, so leaving
	// the placeholders as shape keeps one source of truth rather than two that can disagree.
	//
	// The templates also carry placeholders for facts NOTHING supplies — [PROMO_CODE],
	// [REGULAR_PRICE], [SESSION_COUNT], [VENUE_NAME] and a dozen more — while marking those
	// sections required. Only three of roughly twenty placeholder kinds are ever filled.
	//
	// The system prompt handles that ONCE, above: omit the sentence or section whose placeholder
	// has no value, do not guess one, and do not emit the bracket itself. A second instruction
	// saying to KEEP the placeholder verbatim was added here and removed — it contradicted the
	// first outright, and a prompt that states both policies lets the model pick either, which is
	// worse than whichever one it replaced.
	// The Registration URL line is OMITTED when the brief has none rather than printed empty.
	// The link rule above keys off the line's ABSENCE to mean "write the call to action as plain
	// text"; a "Registration URL:" with nothing after it reads as supplied-but-blank, which is
	// the shape that produced href='#' when nothing was supplied at all.
	//
	// WITHHELD for a stage whose call to action is not registration. The rule above sends every
	// href to this URL, which is right while the button says "Register Now" and wrong for the
	// three stages whose RUNNING button says something else: "Submit Your Proposal" (CFP Launch),
	// "Share Feedback" (Post-Event -- the branch that actually runs, since nothing supplies
	// [RECORDINGS_URL]) and "See You There" (Final Countdown, whose purpose is to confirm
	// attendance for people who already registered).
	//
	// The brief has one url column and no CFP-form or survey field, so there is no correct
	// destination to substitute. A proposal button pointing at a registration form, a feedback
	// button pointing at registration for an event that already happened, or a confirmed attendee
	// sent to register again, are all worse than a button that is not a link.
	//
	// Omitting the line reuses the path the prompt already defines for a brief with no url: a
	// plain-text call to action. See emailstage.Stage.LinksToRegistration.
	registration := ""
	if vars.registrationURL != "" && tpl.LinksToRegistration {
		registration = "\nRegistration URL: " + vars.registrationURL
	}
	// The event-facts block arrives already rendered and already bounded -- labelled lines, each
	// omitted when the brief supplied nothing for it, exactly as the registration URL line above is
	// omitted. See emailCopyEventDetails.eventFactsBlock for the labels, the per-field bounds and
	// why the facts are rendered there rather than formatted here.
	extra := vars.eventFacts
	// The reference block is style guidance, not fact: it may name a different event, speakers,
	// prices or dates than this brief's, and the "use ONLY the event details provided" rule above
	// already forbids inventing facts from anywhere. The label says so explicitly so the model
	// does not treat a past email's content as a source of truth for this one.
	if ref := strings.TrimSpace(vars.referenceBlock); ref != "" {
		extra += "\n\nStyle reference from past sent emails (match tone and structure only; " +
			"never copy facts, names, dates, prices or links from them):\n" + ref
	}
	userPrompt = fmt.Sprintf(`Generate email copy for this event:
Event Name: %s
Location: %s
Dates: %s%s%s

%s`,
		vars.eventName, vars.location, vars.dates, registration, extra, tpl.ContentPrompt)

	return systemPrompt, userPrompt
}

// resolveRegistrationURL picks the destination the generated call-to-action should link to.
//
// The brief's TOP-LEVEL url column is the primary, matching decodeBriefFields in
// internal/dispatch: that is where the UI and the scraper put the event's registration page, and
// every paid platform already treats it as the landing page. A nested `registrationUrl` inside
// event_details is the fallback for briefs that carry only that.
//
// The value is VALIDATED, not merely trimmed, because it is interpolated into a prompt whose
// output goes straight into an href: a relative path, a bare hostname or a `javascript:` scheme
// would be pasted into a marketing email verbatim. An unusable value resolves to "" -- absent --
// and the prompt's link rule then has the model write the call to action as plain text.
//
// Absent used to be the ONLY case, because no URL was supplied at all: the model filled the gap
// with href='#', so the campaign's primary CTA was a dead link the dispatcher's UTM tagger could
// not tag either. Verified on a live staged draft.
func resolveRegistrationURL(briefURL string, d emailCopyEventDetails) string {
	for _, candidate := range []string{briefURL, d.RegistrationURL} {
		if u := httpURL(candidate); u != "" {
			return u
		}
	}
	return ""
}

// httpURL returns the trimmed value when it is an absolute http(s) URL, and "" otherwise.
func httpURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// RAW SIZE FIRST, before url.Parse touches it. `brief.url` carries no MaxLength in
	// design/brief.go and is stored as PostgreSQL TEXT, so the value arriving here is unbounded --
	// and normalising it allocates roughly TWICE its length (Parse, ParseQuery, Encode, String;
	// measured at ~21MB for a 10MB input). Doing that before the maxPromptSize guard runs would
	// perform the exact allocation that guard exists to prevent, using the guard's own input.
	//
	// The bound is maxPromptSize, not something smaller: a URL that alone exceeds the whole
	// caller-field allowance can never be part of a valid request, so refusing it here costs no
	// legitimate caller anything. Refusing rather than truncating also keeps the FALLBACK honest
	// -- an oversized primary yields "" and resolveRegistrationURL moves on to the nested
	// candidate, exactly as it does for any other unusable value.
	if utf8.RuneCountInString(trimmed) > maxPromptSize {
		return ""
	}
	u, err := url.Parse(trimmed)
	// Hostname(), not Host: `https://:443/path` parses with a NON-EMPTY Host (":443") and an
	// empty Hostname, so a Host check alone accepts a URL with no host at all. This matches the
	// registration-URL validators the paid adapters already use
	// (internal/platform/linkedin/client.go, internal/platform/reddit/client.go).
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		return ""
	}
	// url.Parse lower-cases the scheme (RFC 3986 3.1), so this needs no fold. Anything else --
	// mailto:, javascript:, data: -- is not a registration page and must not reach an href.
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	// Embedded credentials are refused rather than stripped, and the reason is specific to this
	// caller: the value is interpolated into a PROMPT, so `https://user:password@host/reg` would
	// be sent to the model and can be rendered verbatim into a marketing email -- a credential
	// disclosed to every recipient and to the LLM provider. The paid adapters refuse it for the
	// narrower reason that it is not a registration page; here it is also a leak.
	if u.User != nil {
		return ""
	}
	// A malformed percent-escape in the query is refused, matching buildAdFinalURL: url.Query()
	// SILENTLY DROPS the offending parameter, so a value that parses here can still describe a
	// different destination than the one the operator pasted.
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	// Re-serialise rather than return `trimmed`. url.Parse is a STRUCTURAL parser and accepts a
	// quote in a path or query -- `https://events.example/" onclick="alert(1)` parses cleanly and
	// every check above passes it. This value is interpolated into an LLM prompt and the model is
	// asked to place it in an href, so a raw quote can close the attribute in the generated HTML.
	// u.String() percent-encodes the delimiters in the PATH and FRAGMENT, but it emits RawQuery
	// verbatim -- so the query has to be re-encoded explicitly, which is what the Encode() below
	// is for. Without it `?a=1"><script>` survives intact and the path fix is cosmetic.
	//
	// Encode() SORTS parameters, so a returned URL can differ from the operator's in parameter
	// ORDER. That is accepted deliberately: query parameter order is not semantically meaningful
	// to any registration destination the LF uses, and the alternative -- refusing any URL whose
	// query needs escaping -- would reject a legitimate paste over a character the model would
	// never have been able to misuse anyway. The value is used for LINKING, never for display.
	//
	// This is the SEVENTH copy of essentially this validator (googleads, linkedin, meta,
	// microsoft, reddit, twitter, here). Each is unexported in its own platform package, which is
	// why the email path grew a thin reimplementation instead of a call. Extracting one shared
	// validator is the right fix and is deliberately NOT done here: it touches six adapters on a
	// PR scoped to email copy placement. Tracked separately -- until then, a change to any of the
	// rules above belongs in all seven.
	u.RawQuery = q.Encode()
	return u.String()
}

// resolveEventDates picks the best date string the brief actually carries.
//
// The structured pair wins when present -- it is unambiguous and a caller that set it meant it.
// The combined `dates` string is the fallback rather than the primary because it is free text
// from a scrape, and "Date TBD" is the honest answer when neither exists: the prompt instructs
// the model never to invent dates, so a wrong string is worse than an absent one.
func resolveEventDates(d emailCopyEventDetails) string {
	if formatted := formatEventDates(d.StartDate, d.EndDate); formatted != "Date TBD" {
		return formatted
	}
	if combined := strings.TrimSpace(d.Dates); combined != "" {
		return combined
	}
	return "Date TBD"
}

// formatEventDates builds a human-readable date range from start and end dates.
// The dates are not normalized from the brief (see EventDetailsResult's caveat),
// so this just formats what's provided.
func formatEventDates(startDate, endDate string) string {
	startDate = strings.TrimSpace(startDate)
	endDate = strings.TrimSpace(endDate)
	if startDate == "" && endDate == "" {
		return "Date TBD"
	}
	if startDate != "" && endDate != "" && startDate == endDate {
		return startDate
	}
	if startDate != "" && endDate != "" {
		return fmt.Sprintf("%s - %s", startDate, endDate)
	}
	if startDate != "" {
		return startDate
	}
	return endDate
}

// parseEmailCopyResponse parses the model's JSON response. If parsing fails, it returns
// an error (not a fallback); GenerateEmailCopy treats the error as a 503 because email copy
// is the primary output of this endpoint. Follows the reference implementation's principle
// of defensive parsing and fail-closed validation.
//
// Two response shapes are accepted. The stage-aware prompt asks for the current "sections"
// array. legacySystemPrompt is FROZEN byte-for-byte (LFXV2-1940) and still asks the model for
// the pre-sections "body"/"cta" pair, so a caller sending no stage still gets that shape back
// from the model -- this function repackages it into one rich_text section and, when present,
// one button section, rather than rewriting the pinned prompt to ask for something new.
//
// HTML content is NOT truncated: HTML truncated at an arbitrary rune boundary corrupts markup
// (cutting inside tags, attributes, entities, or dropping closing tags). Oversized content is
// rejected as an unusable response (same path as unparseable responses). Subject, preheader,
// and button text are truncated because they are plain text with no markup concerns.
// allowLegacyShape says whether a response carrying the flat pre-sections `body`/`cta` pair
// may be repackaged as sections. It is true ONLY on the frozen legacySystemPrompt path (an
// absent stage), which is the prompt that still asks the model for that shape.
//
// Gating it matters because the repackaging is otherwise a fail-open: a stage-aware request
// whose model output regresses to the legacy shape would be silently converted and returned as
// a normal success, so a prompt or model regression would look like ordinary operation. When
// sections were asked for, a legacy-only response is an unusable response, and the caller's
// existing 503 is the honest answer.
func parseEmailCopyResponse(raw string, allowLegacyShape bool) (*briefs.EmailCopy, error) {
	// Try JSON first, stripping code fences if present.
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	// The model is instructed to return JSON only, but observed live: it sometimes prepends a
	// conversational preamble ("Here is the email copy...") before the object anyway, which code
	// fence stripping alone does not catch and json.Unmarshal then rejects outright ("invalid
	// character 'H' looking for beginning of value"). Extracting the outermost {...} object
	// tolerates that preamble (and any trailing chatter) without weakening validation: a
	// genuinely malformed or missing object still falls through to the unmarshal error below.
	raw = extractJSONObject(raw)

	// maxHTMLRunes MIRRORS MaxLength(8000) on email-copy-section's `html` attribute in
	// design/brief.go, and the two have to move together. Goa validates the response against
	// that MaxLength, so content this function let through would fail there instead — turning
	// a bad model response into a 500 that names nothing actionable, rather than the 503 "the
	// model returned something unusable" it actually is.
	const maxHTMLRunes = 8000

	// MIRRORS MaxLength(2000) on email-copy-section's `url` attribute in design/brief.go.
	const maxButtonURLRunes = 2000

	var parsed struct {
		Subject   string `json:"subject"`
		Preheader string `json:"preheader"`
		Sections  []struct {
			Type string `json:"type"`
			HTML string `json:"html"`
			Text string `json:"text"`
			URL  string `json:"url"`
		} `json:"sections"`
		// Legacy (pre-sections) fields, populated only by legacySystemPrompt's model output.
		Body string `json:"body"`
		Cta  string `json:"cta"`
	}
	err := json.Unmarshal([]byte(raw), &parsed)
	if err != nil {
		// JSON failed; treat as a generation failure rather than falling back to a raw-text
		// field. Email copy is the primary output of this endpoint.
		return nil, fmt.Errorf("failed to parse model response as json: %w", err)
	}

	if len(parsed.Sections) == 0 {
		// No sections array: this is the legacy flat shape. An entirely empty response (no
		// sections, no body) falls through as zero sections, which GenerateEmailCopy's
		// required-field check below rejects the same way an empty legacy body always did.
		//
		// Refuse the repackaging when sections were the shape asked for. The check is on
		// content rather than on `len(Sections) == 0` alone so a genuinely empty response
		// keeps its existing path (rejected below as missing required fields) rather than
		// being reported as a shape mismatch it is not.
		if !allowLegacyShape && (strings.TrimSpace(parsed.Body) != "" || strings.TrimSpace(parsed.Cta) != "") {
			return nil, fmt.Errorf("model returned the legacy body/cta shape for a stage-aware request that asked for sections; model response is unusable")
		}
		if strings.TrimSpace(parsed.Body) != "" {
			if utf8.RuneCountInString(parsed.Body) > maxHTMLRunes {
				return nil, fmt.Errorf("email body exceeds maximum length of %d characters; model response is unusable", maxHTMLRunes)
			}
			parsed.Sections = append(parsed.Sections, struct {
				Type string `json:"type"`
				HTML string `json:"html"`
				Text string `json:"text"`
				URL  string `json:"url"`
			}{Type: "rich_text", HTML: parsed.Body})
		}
		if strings.TrimSpace(parsed.Cta) != "" {
			parsed.Sections = append(parsed.Sections, struct {
				Type string `json:"type"`
				HTML string `json:"html"`
				Text string `json:"text"`
				URL  string `json:"url"`
			}{Type: "button", Text: parsed.Cta})
		}
	}

	sections := make([]*briefs.EmailCopySection, 0, len(parsed.Sections))
	for _, s := range parsed.Sections {
		sectionType := strings.TrimSpace(s.Type)
		switch sectionType {
		case "rich_text":
			if utf8.RuneCountInString(s.HTML) > maxHTMLRunes {
				return nil, fmt.Errorf("email section html exceeds maximum length of %d characters; model response is unusable", maxHTMLRunes)
			}
		case "button", "divider":
			// No length guard: button text is truncated below and divider carries no content.
		default:
			// An unrecognized section type is a malformed response, not a partial success --
			// dropping it silently would let the model emit anything and have it vanish.
			return nil, fmt.Errorf("email response contains unknown section type %q; model response is unusable", sectionType)
		}

		section := &briefs.EmailCopySection{Type: sectionType}
		if sectionType == "rich_text" {
			// The ONE place model-authored body HTML becomes a response, so the one place the
			// service's design is applied. styledBodyHTMLWithinBound drops every attribute the
			// model wrote and re-dresses the surviving semantic tags in this service's palette and
			// type scale (email_body_style.go); it falls back to the unstyled HTML if styling
			// would push the section past maxHTMLRunes, so styling can never turn a valid model
			// response into a 503.
			//
			// At generation, not at render: the BFF flattens these sections into the single body
			// string that BOTH the operator's preview and the HubSpot draft are built from, so
			// styling here is what makes the preview show what the recipient will see. Styling in
			// addBodySection instead would leave the preview permanently unstyled, or require the
			// same palette written a second time in TypeScript.
			//
			// No truncation: oversized HTML is rejected above.
			html := styledBodyHTMLWithinBound(s.HTML, maxHTMLRunes)
			section.HTML = &html
		}
		if sectionType == "button" {
			text := truncateString(s.Text, 50)
			section.Text = &text
			if url := strings.TrimSpace(s.URL); url != "" {
				// REJECTED, not truncated. maxButtonURLRunes mirrors MaxLength(2000) on
				// email-copy-section's `url` in design/brief.go, and the two move together: Goa
				// validates the response against it, so a value this function let through would
				// fail there as a 500 naming nothing actionable, rather than the 503 "the model
				// returned something unusable" it actually is -- the same reasoning as
				// maxHTMLRunes above. Truncating is worse than rejecting for a URL: a cut
				// destination is a live link to the wrong place, not a shorter one.
				if utf8.RuneCountInString(url) > maxButtonURLRunes {
					return nil, fmt.Errorf("email button url exceeds maximum length of %d characters; model response is unusable", maxButtonURLRunes)
				}
				section.URL = &url
			}
		}
		sections = append(sections, section)
	}

	return &briefs.EmailCopy{
		Subject:   truncateString(parsed.Subject, 200),
		Preheader: truncateString(parsed.Preheader, 150),
		Sections:  sections,
	}, nil
}

// extractJSONObject returns the outermost {...} object in s, tolerating any conversational
// prose the model wrapped around it. Brace counting ignores braces inside JSON string literals
// (tracking escape sequences so an escaped quote does not end the string early) so that a curly
// brace inside generated HTML or copy text never miscounts as structural. Returns s unchanged
// if no balanced object is found, so a genuinely non-JSON response still reaches json.Unmarshal
// and produces its normal, actionable parse error rather than being silently swallowed here.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start == -1 {
		return s
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s
}

// truncateString limits a string to maxLen runes (not bytes), stripping trailing whitespace.
// Follows the pattern in internal/platform/googleads/ad_copy.go: bounded truncation
// on rune boundaries so multibyte UTF-8 sequences are never split.
// Trailing whitespace is always stripped.
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	// Strip trailing whitespace after truncation.
	return strings.TrimRight(string(runes), " \t\n\r")
}

// GenerateEmailCopy implements the briefs.Service GenerateEmailCopy method.
// It generates AI-written email copy (subject, preheader, body, CTA) for a campaign brief.
// It does NOT persist the generated copy to the brief.
func (s *BriefService) GenerateEmailCopy(ctx context.Context, p *briefs.GenerateEmailCopyPayload) (*briefs.EmailCopy, error) {
	// Fetch the brief and snapshot the llmClient dependency.
	briefRepo, _, _, _, err := s.ready()
	if err != nil {
		return nil, err
	}
	llmClient := s.snapshotLLMClient()
	if llmClient == nil {
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "AI model is not configured; email copy generation is unavailable",
		}
	}

	// Load the brief to extract its event details.
	brief, gerr := briefRepo.GetBrief(ctx, p.ProjectID, p.BriefID)
	if gerr != nil {
		return nil, mapBriefErr(gerr)
	}

	// Decode and validate the event details from the brief.
	details, derr := decodeEmailCopyEventDetails(brief.EventDetails)
	if derr != nil {
		slog.WarnContext(ctx, "email copy generation blocked: could not decode event details",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "error", derr)
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "brief's event details are incomplete or invalid; provide at least eventName before generating copy",
		}
	}

	// Compose the generation prompt using the brief's event details.
	// Only scrape, never invent: see composeEmailCopyPrompt.
	promptVars := emailCopyPromptVars{
		eventName: strings.TrimSpace(details.EventName),
		location:  strings.TrimSpace(details.Location),
		dates:     resolveEventDates(details),
		// The brief's own url column, not part of the event-details blob -- see
		// resolveRegistrationURL for why that is the primary.
		registrationURL: resolveRegistrationURL(brief.URL, details),
		// Rendered and bounded here rather than in composeEmailCopyPrompt, which receives the
		// finished string. Built unconditionally even for a no-stage caller, whose frozen legacy
		// prompt never reads it -- the same bounded-work-then-discarded shape resolveRegistrationURL
		// already has, and cheaper than teaching this site which prompt path it is feeding.
		eventFacts: details.eventFactsBlock(),
		// Absent is not an error: the design leaves `stage` optional. An absent one takes the
		// frozen legacy prompt (byte-identical to the pre-stage behaviour, LFXV2-1940); only a
		// non-empty unrecognised value falls through Resolve to Registration Push.
		stage: strVal(p.Stage),
		// Same absent-is-a-no-op shape as stage: composeEmailCopyPrompt only appends the
		// urgency-fomo block for an EXACT match, so an unrecognised value here silently produces
		// ordinary stage-based copy rather than an error.
		variant: strVal(p.Variant),
		// Same absent-is-a-no-op shape as stage/variant: composeEmailCopyPrompt only appends
		// segment guidance for an EXACT match, so an unrecognised value here silently produces
		// ordinary stage-based copy rather than an error.
		segment: strVal(p.Segment),
	}
	// BEST-EFFORT reference-email lookup, never a hard dependency: a project with no HubSpot
	// connection, no published emails, or an unreachable portal still gets copy generated, just
	// without style mirroring. See EmailReferenceSource for why this cannot fail the request.
	if ref := s.snapshotEmailReferenceSource(); ref != nil {
		promptVars.referenceBlock = ref.BuildReferenceBlock(ctx, brief.ProjectID)
	}
	// Enforce a bound on the prompt size to prevent unbounded input-token cost and large
	// allocations. Four strings reach the prompt — eventName, location, the formatted dates
	// and the registration URL — but `event_details` is declared `Any` in design/brief.go and
	// `url` is the brief's own free-text column, so none of the four carries a length
	// constraint of its own and a single one of them can be arbitrarily large.
	//
	// The URL is counted here rather than capped on its own so the contract between the two
	// bounds stays exact: the composed bound must clear (worst stage floor + maxPromptSize), and
	// a field bounded separately would sit outside that sum and could push a valid request into
	// the 503 branch.
	//
	// MEASURED, not estimated, and this bound counts ONLY what the caller supplies: eventName,
	// location and dates always, plus the registration URL on a stage that formats it. A realistic
	// set ("KubeCon + CloudNativeCon North America 2026", "Salt Lake City, Utah",
	// "November 10-13, 2026",
	// "https://events.linuxfoundation.org/kubecon-cloudnativecon-north-america/register/") is 164
	// runes with the URL and 83 without, so 2400 leaves at least ~2236 of headroom — far above any
	// real event name, far below a payload worth paying input tokens for. Oversized input is
	// rejected as 400 BadRequest, which is correct here: the caller CAN edit these fields, unlike
	// the composed bound below.
	//
	// The URL is HALF of that realistic figure (81 of the 164) and is the one field a caller does
	// not type, so it is the term most likely to grow: a tracking-parameter suffix costs more
	// headroom than an event rename ever will. Re-measure with a real registration URL, never a
	// bare origin.
	//
	// The fixed prompt (1751 runes of system text plus the stage template) is deliberately NOT
	// in this figure -- it is service-owned and no caller can grow it, which is exactly why the
	// composed bound is a separate constant with a separate status code. Re-measure both when
	// the shared prompt or a template changes; the numbers here have gone stale twice.
	//
	// RUNES, not bytes. The limit is stated to the caller and logged as a character count,
	// and every other limit in this file counts runes (parseEmailCopyResponse's body bound,
	// truncateString). Counting bytes here rejected a name in Japanese or an accented
	// location at a third of the advertised budget, and only for those callers — a limit
	// that means something different depending on the alphabet the event is named in.

	// The COMPOSED prompt is bounded separately, and higher, because the two checks bound
	// different things. `maxPromptSize` bounds what the CALLER supplies -- three `Any`-typed
	// fields with no length constraint of their own -- and that is the guard against unbounded
	// input-token cost and large allocations. The composed total additionally includes the fixed
	// system prompt and the stage template, which are service-owned constants no caller can grow.
	//
	// This bound guards TEMPLATE growth, not caller input, and it cannot guard both.
	//
	// The two properties are mutually exclusive BY CONSTRUCTION, for any pair of numbers: "never
	// refuse what the pre-check accepted" needs this at or above the worst valid composition,
	// while "reachable by caller input" needs it below. Three revisions tried to satisfy both by
	// tuning the numbers; none could, and the arithmetic says none can.
	//
	// So the first property wins -- a caller must never be told their input is too large by the
	// second of two checks after the first accepted it -- and this one is sized to catch the case
	// that remains: a stage template growing past the budget in a future edit. That is a real
	// failure mode, since the templates are large (Post-Event's ContentPrompt is 3637 runes, and
	// its COMPOSED floor -- system and user framing plus the template, at zero caller input -- is
	// 6078) and are
	// edited by hand.
	//
	// MEASURED, and re-measured after the link policy landed: worst stage-only floor 6388
	// (Post-Event), so with the 2400-rune input bound the worst valid composition is 8788. The
	// bound is 9300, leaving 512 runes of headroom for template growth -- about one section.
	//
	// Post-Event is the worst floor even though it WITHHOLDS the registration URL: its content
	// prompt is the longest, and dropping the 19-rune URL line does not change which stage leads.
	// TestConceptDocSizingArithmetic computes these rather than trusting this comment; it caught
	// the figures above being 20 runes stale the moment the link policy changed them.
	//
	// This comment has now been wrong THREE times, most recently by my own hand: the precedence
	// paragraph added to the shared system prompt earlier today grew every stage by ~130 runes and
	// I did not re-measure, so 7600 would have refused 303 runes of PERFECTLY VALID caller input
	// with a 503 blaming the service. A prose instruction to "re-measure" plainly does not survive
	// contact, so TestComposedBoundClearsEveryStageFloor now computes the real floors and fails if
	// any stage plus the input bound exceeds this -- the number cannot silently go stale again.
	//
	// Both figures were wrong twice before, in opposite directions, from a stale 4700/7700
	// measurement taken before the templates grew:
	//
	//   - 6500 REJECTED VALID INPUT: Post-Event left only ~1459 runes for caller fields, so 1618
	//     runes of event details passed the 3000 pre-check and were refused here.
	//   - 8000 was believed unreachable and was not -- 8041 > 8000, so it fired only for the very
	//     largest Post-Event input, and the reachability test passed for a reason nobody checked.
	//
	// The input bound came DOWN from 3000 rather than this one going up, because raising it above
	// 8041 would have satisfied (1) by destroying (2). 2400 runes is far more event-detail text
	// than any real event carries. Re-measure both whenever the shared prompt or any template
	// changes: it has now been wrong THREE times from exactly that -- see the note above the
	// composed bound for the third.

	// Checked BEFORE composing, and again after. The pre-check is what makes the bound real:
	// composeEmailCopyPrompt formats these unbounded fields into a new string, so a 50MB stored
	// eventName is copied in full before a post-hoc check could reject it — the allocation this
	// guard exists to prevent, performed by the guard's own input. The COUNTED fields alone cannot
	// exceed the total, so this is a sound necessary condition; it is not sufficient, because the
	// fixed template counts too, which is what the second check is for. Rejecting here bounds the
	// compose to O(maxPromptSize).
	//
	// "Counted" rather than a fixed number, because the set is conditional: three always, four when
	// the stage formats the registration URL. Naming a count here is what made this comment
	// contradict itself in consecutive sentences ("these four unbounded fields" above "the three
	// fields alone"), so the invariant is stated in terms of what the code below actually sums.
	inputSize := utf8.RuneCountInString(promptVars.eventName) +
		utf8.RuneCountInString(promptVars.location) + utf8.RuneCountInString(promptVars.dates)
	// The URL counts only on the STAGE-AWARE branch, because only that branch formats it into the
	// prompt. composeEmailCopyPrompt returns the frozen legacy prompt for a blank stage using
	// eventName/location/dates alone (LFXV2-1940 requires it byte-identical to the pre-stage
	// output), so counting the URL there would let a no-stage caller be rejected with a 400 for a
	// value that never reaches their prompt and cannot affect their result -- a behaviour change
	// on the one path documented as unchanged.
	//
	// A no-stage call therefore RESOLVES a URL it never counts and never uses. That is bounded
	// work, not a leak: httpURL already refused anything over maxPromptSize, so the parse and
	// re-encode are at most ~2x of 2400 runes, and the value is then discarded. Skipping the
	// resolve entirely for a blank stage would couple this function to the composer's branching,
	// which is the coupling the frozen legacy prompt exists to avoid.
	//
	// The same reasoning now applies WITHIN the stage-aware branch. A stage whose call to action is
	// not registration has the URL withheld from its prompt (emailstage.LinksToRegistration), so
	// counting it for CFP Launch, Post-Event or Final Countdown would refuse a caller for a value
	// those prompts never receive either. The predicate is therefore "does this call FORMAT the
	// url", which is exactly the condition composeEmailCopyPrompt uses -- not "is a stage set".
	if stage := strings.TrimSpace(promptVars.stage); stage != "" && emailstage.Resolve(stage).LinksToRegistration {
		inputSize += utf8.RuneCountInString(promptVars.registrationURL)
	}
	// The event-facts block is deliberately NOT counted here, and speakers/topics stopped being
	// counted when they moved into it. It is TRUNCATED by its producer instead
	// (emailCopyEventDetails.eventFactsBlock, bounded by maxEventFactsBlockRunes), which makes it a
	// fixed-size contributor to maxComposedPromptSize exactly as the reference block is, not caller
	// input guarded by this bound.
	//
	// Counting it would be the wrong answer on both halves. The 400 below names "the event name,
	// location, dates, or url", so adding a scraped `description` to the sum would reject a brief
	// for an event whose own page writes three paragraphs about itself while telling the operator to
	// shorten four fields that are not at fault -- and there is no field they could shorten, since
	// the description is scraped, not typed. Truncation is also the stronger guarantee: counting
	// only detects an overrun, while a bound applied at the producer cannot be exceeded at all.
	if inputSize > maxPromptSize {
		slog.WarnContext(ctx, "email copy generation blocked: event details exceed prompt size limit",
			"project_id", p.ProjectID, "brief_id", p.BriefID,
			"input_size", inputSize, "limit", maxPromptSize)
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "brief's event details are too large; reduce the event name, location, dates, or url",
		}
	}

	systemPrompt, userPrompt := composeEmailCopyPrompt(promptVars)

	totalPromptSize := utf8.RuneCountInString(systemPrompt) + utf8.RuneCountInString(userPrompt)
	if totalPromptSize > maxComposedPromptSize {
		// ERROR, not Warn, and 503 rather than 400. This branch is unreachable by caller input --
		// TestComposedBoundClearsEveryStageFloor measures the worst valid composition against
		// maxComposedPromptSize on every build -- so if it fires, a service-owned stage template or
		// prompt block has outgrown its budget. That is a service defect, and a
		// 400 would file it under client error on every 4xx/5xx dashboard while telling the caller
		// to edit a brief that is not the problem. The message already said as much; the status
		// code contradicted it.
		slog.ErrorContext(ctx, "email copy generation blocked: composed prompt exceeds size limit; a stage template has outgrown the budget",
			"project_id", p.ProjectID, "brief_id", p.BriefID,
			"stage", emailstage.Resolve(promptVars.stage).StageName,
			"prompt_size", totalPromptSize, "limit", maxComposedPromptSize)
		// NOT "temporarily": this is a compiled-in template exceeding a compiled-in bound, so every
		// retry returns this same 503 until a corrected deployment ships. Promising transience
		// would have the caller retry a request that cannot start succeeding on its own.
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "email copy generation is unavailable for this stage; retrying will not help until this service is fixed",
		}
	}

	// Call the model.
	raw, cerr := llmClient.Complete(ctx, systemPrompt, userPrompt)
	if cerr != nil {
		// Map llm.Client errors onto brief-service sentinels.
		if errors.Is(cerr, llm.ErrNotConfigured) {
			// This shouldn't happen (we checked llmClient above), but defensive.
			return nil, &briefs.ConnServiceUnavailableError{
				Code:    "503",
				Message: "AI model is not configured",
			}
		}
		// Log and report as a platform-unavailable error (503).
		slog.WarnContext(ctx, "email copy generation failed on the AI platform",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "error", safeErrSummary(cerr))
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "email copy could not be generated from the AI platform",
		}
	}

	// Parse and validate the response, enforcing length limits in code.
	// Legacy repackaging is permitted only on the path that actually requested that shape --
	// the same `stage == ""` condition composeEmailCopyPrompt branches on for the frozen prompt.
	copy, perr := parseEmailCopyResponse(raw, strings.TrimSpace(promptVars.stage) == "")
	if perr != nil {
		slog.WarnContext(ctx, "email copy generation: could not parse model response",
			"project_id", p.ProjectID, "brief_id", p.BriefID, "error", perr)
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the AI platform returned an unreadable response",
		}
	}

	// Validate the required fields are present and non-empty.
	//
	// TRIMMED, because the point is whether a human receives usable copy, and a rich_text
	// section of "   " is as unusable as an absent one. The distinction is not academic for
	// HTML content specifically: it is the one field parseEmailCopyResponse deliberately does
	// NOT put through truncateString (truncating HTML corrupts markup), and truncateString is
	// what strips trailing whitespace. So a whitespace-only body was the one shape that reached
	// here non-empty, and the endpoint answered 200 with a blank email. Subject/preheader are
	// trimmed for the same reason rather than relying on truncateString to have done it.
	//
	// A usable email needs at least one non-blank rich_text section (the message itself); a
	// button is not required on its own, since a non-registration CTA can legitimately be
	// plain text (see composeEmailCopyPrompt's WITHHELD registration-URL note) -- but this
	// service only ever asks the model for a button CTA, so the absence of ANY section at all
	// still means the response is missing everything.
	hasContent := false
	for _, sec := range copy.Sections {
		if sec.Type == "rich_text" && sec.HTML != nil && strings.TrimSpace(*sec.HTML) != "" {
			hasContent = true
			break
		}
	}
	if strings.TrimSpace(copy.Subject) == "" || strings.TrimSpace(copy.Preheader) == "" || !hasContent {
		slog.WarnContext(ctx, "email copy generation: model response missing required fields",
			"project_id", p.ProjectID, "brief_id", p.BriefID)
		return nil, &briefs.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the AI platform generated incomplete copy",
		}
	}

	return copy, nil
}

// snapshotLLMClient returns a snapshot of the llmClient under the read lock.
// Mirrors eventURLDeps() for consistency.
func (s *BriefService) snapshotLLMClient() *llm.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.llmClient
}

// legacySystemPrompt is the system prompt this service emitted before stages existed, preserved
// byte-for-byte so a caller that sends no stage sees no change at all (LFXV2-1940). It is a
// FROZEN copy: edits to the stage-aware prompt above must NOT be mirrored here, and the test
// TestAbsentStageProducesLegacyPrompt pins it against the pre-stage commit's text.
const legacySystemPrompt = `You are an expert email copywriter for technology events and communities.
Your task is to generate compelling email copy for a campaign brief.

IMPORTANT: Use ONLY the event details provided below; never invent dates, names, or locations.
Every factual claim must come directly from what you're given.

Generate JSON with these fields (no markdown fencing):
{
  "subject": "Email subject line (max 60 chars)",
  "preheader": "Email preheader text (max 100 chars)",
  "body": "Email body in HTML (max 8000 chars, include <p> tags)",
  "cta": "Call-to-action button text (max 50 chars)"
}

Constraints:
- Subject: punchy, under 60 characters
- Preheader: summary of the email, under 100 characters
- Body: professional HTML email, inviting and focused on the event
- CTA: action-oriented, under 50 characters (e.g. "Register Now", "Join Us")
- Write for a professional Linux Foundation / technology audience
- Make it about the event and community, not promotional`
