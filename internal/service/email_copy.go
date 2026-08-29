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

// emailCopyPromptVars holds the values needed to compose the generation prompt.
type emailCopyPromptVars struct {
	eventName string
	location  string
	dates     string
	// stage selects the generation spec. Empty means the caller did not say, which resolves to
	// emailstage.DefaultStage rather than erroring -- see Resolve.
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
// Follows the lfx-one reference implementation's principle of composing from fixed blocks
// rather than branching on prompt variants.
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
	systemPrompt = `You are an expert email copywriter for technology events and communities.
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

	// Stage-specific guidance, appended to the shared role/constraint block above rather than
	// replacing it: the JSON schema and the length limits hold for every stage, only the intent
	// changes. An unknown stage resolves to the default, so a caller that sends none keeps exactly
	// the copy it produced before stages existed.
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
	// substituted here: the details are already stated above them as labelled facts, and the
	// system prompt forbids inventing any others, so leaving the placeholders as shape keeps one
	// source of truth for the values rather than two that can disagree.
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
		// Absent is not an error: the design leaves `stage` optional, and Resolve reads an empty
		// string as "the caller did not say" -> Registration Push, the pre-stage behaviour.
		stage: strVal(p.Stage),
	}
	// Enforce a bound on the prompt size to prevent unbounded input-token cost and large
	// allocations. Only three strings from event_details reach the prompt — eventName,
	// location and the formatted dates — but `event_details` is declared `Any` in
	// design/brief.go, so none of the three carries a length constraint of its own and a
	// single one of them can be arbitrarily large.
	//
	// MEASURED, not estimated: the fixed system prompt is 962 runes and a realistic user
	// prompt ("KubeCon + CloudNativeCon North America 2026", "Salt Lake City, Utah",
	// "November 10-13, 2026") is 245, for 1207 total. 3000 leaves roughly 1800 runes of
	// headroom across the three fields — far above any real event name, far below a payload
	// worth paying input tokens for. Oversized prompts are rejected as 400 BadRequest.
	//
	// RUNES, not bytes. The limit is stated to the caller and logged as a character count,
	// and every other limit in this file counts runes (parseEmailCopyResponse's body bound,
	// truncateString). Counting bytes here rejected a name in Japanese or an accented
	// location at a third of the advertised budget, and only for those callers — a limit
	// that means something different depending on the alphabet the event is named in.
	const maxPromptSize = 3000 // runes

	// The COMPOSED prompt is bounded separately, and higher, because the two checks bound
	// different things. `maxPromptSize` bounds what the CALLER supplies -- three `Any`-typed
	// fields with no length constraint of their own -- and that is the guard against unbounded
	// input-token cost and large allocations. The composed total additionally includes the fixed
	// system prompt and the stage template, which are service-owned constants no caller can grow.
	//
	// MEASURED, like the bound above: the largest stage (Post-Event) contributes 3552 runes, and
	// the shared system prompt 962. 8000 leaves headroom for a stage longer than today's largest
	// without re-tuning, while still rejecting a payload worth paying input tokens for. Sizing
	// this at 3000 would reject EVERY stage-aware generation -- the constant text alone exceeds
	// it -- which is what the test suite caught when stages were introduced.
	const maxComposedPromptSize = 8000 // runes

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
		slog.WarnContext(ctx, "email copy generation blocked: composed prompt exceeds size limit",
			"project_id", p.ProjectID, "brief_id", p.BriefID,
			"prompt_size", totalPromptSize, "limit", maxComposedPromptSize)
		return nil, &briefs.BadRequestError{
			Code:    "400",
			Message: "brief's event details are too large; reduce the event name, location, or dates",
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
