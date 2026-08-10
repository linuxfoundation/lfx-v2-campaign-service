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

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
)

// emailCopyEventDetails is the slice of a brief's EventDetails this generation needs.
// It mirrors the pattern in audience_build.go and is decoded opportunistically.
type emailCopyEventDetails struct {
	EventName string `json:"event_name"`
	Location  string `json:"location"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

// emailCopyPromptVars holds the values needed to compose the generation prompt.
type emailCopyPromptVars struct {
	eventName string
	location  string
	dates     string
}

// decodeEmailCopyEventDetails pulls the fields email generation needs from the brief's opaque
// EventDetails blob. It mirrors the pattern in audience_build.go: a blob that isn't this shape
// is skipped rather than failing the request.
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
		return details, errors.New("event details have no event_name; copy generation requires it")
	}
	return details, nil
}

// composeEmailCopyPrompt builds the system and user prompts for email copy generation.
// Follows the lfx-one reference implementation's principle of composing from fixed blocks
// rather than branching on prompt variants.
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

	// User prompt: the specific event details and request.
	userPrompt = fmt.Sprintf(`Generate email copy for this event:
Event Name: %s
Location: %s
Dates: %s

Create compelling email copy that invites registration and highlights the value of attending.`,
		vars.eventName, vars.location, vars.dates)

	return systemPrompt, userPrompt
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

// parseEmailCopyResponse parses the model's JSON response, defaulting gracefully
// to raw-text fallback if parsing fails. Follows the reference implementation's
// principle of defensive parsing and soft-fail-non-essential.
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
		// JSON parse succeeded; enforce truncation limits.
		return &briefs.EmailCopy{
			Subject:   truncateString(parsed.Subject, 200),
			Preheader: truncateString(parsed.Preheader, 150),
			Body:      truncateString(parsed.Body, 8000),
			Cta:       truncateString(parsed.Cta, 50),
		}, nil
	}

	// JSON failed; treat as a generation failure rather than falling back to a raw-text
	// field. Email copy is the primary output of this endpoint.
	return nil, fmt.Errorf("failed to parse model response as json: %w", err)
}

// truncateString limits a string to maxLen characters, stripping trailing whitespace.
// Follows the pattern in internal/platform/googleads/ad_copy.go: bounded truncation
// with documented rationale. Trailing whitespace is always stripped.
func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	// Strip trailing whitespace after truncation.
	return strings.TrimRight(s, " \t\n\r")
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
			Message: "brief's event details are incomplete or invalid; provide at least event_name before generating copy",
		}
	}

	// Compose the generation prompt using the brief's event details.
	// Only scrape, never invent: see composeEmailCopyPrompt.
	promptVars := emailCopyPromptVars{
		eventName: strings.TrimSpace(details.EventName),
		location:  strings.TrimSpace(details.Location),
		dates:     formatEventDates(details.StartDate, details.EndDate),
	}
	systemPrompt, userPrompt := composeEmailCopyPrompt(promptVars)

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
	if copy.Subject == "" || copy.Preheader == "" || copy.Body == "" || copy.Cta == "" {
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
