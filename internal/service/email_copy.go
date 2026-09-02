// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
}

// maxPromptSize bounds the CALLER's three event-detail fields, checked before composing.
const maxPromptSize = 2400 // runes

// maxComposedPromptSize bounds the WHOLE composed prompt, checked after.
//
// It must stay at or above (worst stage floor + maxPromptSize) or a caller is told their input is
// too large by the second check after the first accepted it -- with a 503 that blames the service.
// TestComposedBoundClearsEveryStageFloor computes the floors and enforces exactly that.
const maxComposedPromptSize = 8400 // runes

// emailCopyPromptVars holds the values needed to compose the generation prompt.
type emailCopyPromptVars struct {
	eventName string
	location  string
	dates     string
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
}

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
supplied deadline. Drop the section: only eventName, location and dates are ever supplied, so a
required section built on anything else cannot be written truthfully. A shorter email that says
only what is known is the correct output, never an invented price, deadline or count.

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
	userPrompt = fmt.Sprintf(`Generate email copy for this event:
Event Name: %s
Location: %s
Dates: %s

%s`,
		vars.eventName, vars.location, vars.dates, tpl.ContentPrompt)

	return systemPrompt, userPrompt
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
// The Body field is NOT truncated: HTML truncated at an arbitrary rune boundary corrupts markup
// (cutting inside tags, attributes, entities, or dropping closing tags). Oversized bodies are
// rejected as unusable responses (same path as unparseable responses). Subject, preheader, and
// CTA are truncated because they are plain text with no markup concerns.
func parseEmailCopyResponse(raw string) (*briefs.EmailCopy, error) {
	// Try JSON first, stripping code fences if present.
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed struct {
		Subject   string `json:"subject"`
		Preheader string `json:"preheader"`
		Body      string `json:"body"`
		Cta       string `json:"cta"`
	}
	err := json.Unmarshal([]byte(raw), &parsed)
	if err == nil {
		// Check that the HTML body is within bounds. Unlike subject/preheader/CTA,
		// the body cannot be silently truncated: truncating HTML at an arbitrary rune
		// boundary corrupts markup (cuts inside tags/attributes/entities, drops closing tags).
		// Oversized bodies are rejected outright as unusable responses.
		//
		// maxBodyRunes MIRRORS MaxLength(8000) on email-copy's `body` attribute in
		// design/brief.go, and the two have to move together. Goa validates the response
		// against that MaxLength, so a body this function let through would fail there
		// instead — turning a bad model response into a 500 that names nothing actionable,
		// rather than the 503 "the model returned something unusable" it actually is.
		const maxBodyRunes = 8000
		if utf8.RuneCountInString(parsed.Body) > maxBodyRunes {
			return nil, fmt.Errorf("email body exceeds maximum length of %d characters; model response is unusable", maxBodyRunes)
		}

		// JSON parse succeeded; enforce truncation limits on plain-text fields only.
		return &briefs.EmailCopy{
			Subject:   truncateString(parsed.Subject, 200),
			Preheader: truncateString(parsed.Preheader, 150),
			Body:      parsed.Body, // No truncation: oversized bodies are rejected above.
			Cta:       truncateString(parsed.Cta, 50),
		}, nil
	}

	// JSON failed; treat as a generation failure rather than falling back to a raw-text
	// field. Email copy is the primary output of this endpoint.
	return nil, fmt.Errorf("failed to parse model response as json: %w", err)
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
		// Absent is not an error: the design leaves `stage` optional. An absent one takes the
		// frozen legacy prompt (byte-identical to the pre-stage behaviour, LFXV2-1940); only a
		// non-empty unrecognised value falls through Resolve to Registration Push.
		stage: strVal(p.Stage),
	}
	// Enforce a bound on the prompt size to prevent unbounded input-token cost and large
	// allocations. Only three strings from event_details reach the prompt — eventName,
	// location and the formatted dates — but `event_details` is declared `Any` in
	// design/brief.go, so none of the three carries a length constraint of its own and a
	// single one of them can be arbitrarily large.
	//
	// MEASURED, not estimated, and this bound counts ONLY what the caller supplies: the three
	// event-detail strings. A realistic set ("KubeCon + CloudNativeCon North America 2026",
	// "Salt Lake City, Utah", "November 10-13, 2026") is 83 runes, so 2400 leaves ~2317 of
	// headroom across the three — far above any real event name, far below a payload worth
	// paying input tokens for. Oversized input is rejected as 400 BadRequest, which is correct
	// here: the caller CAN edit these fields, unlike the composed bound below.
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
	// failure mode, since the templates are large (Post-Event is 5041 runes on its own) and are
	// edited by hand.
	//
	// MEASURED 2026-09-01: worst stage-only floor 5503 (Post-Event), so with the 2400-rune input
	// bound the worst valid composition is 7903. The bound is 8400, leaving ~497 runes of headroom
	// for template growth.
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
	// composeEmailCopyPrompt formats these three unbounded fields into a new string, so a
	// 50MB stored eventName is copied in full before a post-hoc check could reject it — the
	// allocation this guard exists to prevent, performed by the guard's own input. The three
	// fields alone cannot exceed the total, so this is a sound necessary condition; it is not
	// sufficient, because the fixed template counts too, which is what the second check is
	// for. Rejecting here bounds the compose to O(maxPromptSize).
	inputSize := utf8.RuneCountInString(promptVars.eventName) +
		utf8.RuneCountInString(promptVars.location) + utf8.RuneCountInString(promptVars.dates)
	if inputSize > maxPromptSize {
		slog.WarnContext(ctx, "email copy generation blocked: event details exceed prompt size limit",
			"project_id", p.ProjectID, "brief_id", p.BriefID,
			"input_size", inputSize, "limit", maxPromptSize)
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "brief's event details are too large; reduce the event name, location, or dates",
		}
	}

	systemPrompt, userPrompt := composeEmailCopyPrompt(promptVars)

	totalPromptSize := utf8.RuneCountInString(systemPrompt) + utf8.RuneCountInString(userPrompt)
	if totalPromptSize > maxComposedPromptSize {
		// ERROR, not Warn, and 503 rather than 400. This branch is unreachable by caller input --
		// the worst valid composition is 7903 against an 8400 bound -- so if it fires, a
		// service-owned stage template has outgrown its budget. That is a service defect, and a
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
	copy, perr := parseEmailCopyResponse(raw)
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
	// TRIMMED, because the point is whether a human receives usable copy, and a body of
	// "   " is as unusable as an absent one. The distinction is not academic for `body`
	// specifically: it is the one field parseEmailCopyResponse deliberately does NOT put
	// through truncateString (truncating HTML corrupts markup), and truncateString is what
	// strips trailing whitespace. So a whitespace-only body was the one shape that reached
	// here non-empty, and the endpoint answered 200 with a blank email. The other three are
	// trimmed for the same reason rather than relying on truncateString to have done it.
	if strings.TrimSpace(copy.Subject) == "" || strings.TrimSpace(copy.Preheader) == "" ||
		strings.TrimSpace(copy.Body) == "" || strings.TrimSpace(copy.Cta) == "" {
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
