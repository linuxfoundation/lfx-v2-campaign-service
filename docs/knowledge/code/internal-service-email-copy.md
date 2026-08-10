---
type: "Code Concept"
title: "Email Copy Generation (internal/service/email_copy.go)"
description: "AI-generated email copy (subject, preheader, body, CTA) for campaign briefs using the LiteLLM proxy client. Implements scrape-not-invent, defensive parsing, code-enforced length limits, and graceful degradation when the model is unconfigured."
resource: "internal/service"
---

# Email Copy Generation (`internal/service/email_copy.go`)

## Overview

Email copy generation uses the LiteLLM client (built in PR #104) to generate AI-written email subject lines, preheaders, bodies, and CTAs for campaign briefs. It is an **enrichment, not a core feature**: when the AI model is not configured, `GenerateEmailCopy` returns 503 ServiceUnavailable, leaving other brief routes unaffected.

## Design Principles

The implementation follows six principles from the reference lfx-one implementation:

1. **Scrape, don't recall.** Every factual claim originates from the brief's `EventDetails` (event name, location, date, etc.), never the model's training data. The system prompt explicitly forbids invention: `"use ONLY the event details provided below; never invent dates, names, or locations"`.

2. **Compose prompts, don't branch.** Prompts are built by concatenating fixed blocks (role instruction, constraints, event-details block) rather than if/else-ing variants. This keeps the prompt construction centred and auditable.

3. **Prompt limits are advisory; code limits are real.** The system prompt tells the model "subject under 60 characters", but `GenerateEmailCopy` also enforces truncation in code via `truncateString()`: subject max 200, preheader max 150, body max 8000, CTA max 50 characters.

4. **Parse defensively, fail-closed.** The model's response must be JSON, optionally wrapped in ```json fences. The parser strips fences and calls `json.Unmarshal` directly; any leading or trailing prose outside the fences will cause JSON parsing to fail, triggering a 503 response. This is intentional: email copy is the primary output of this endpoint, so a malformed response is a generation failure, not a fallback case.

5. **One concern per AI call.** Email copy has one shape (subject/preheader/body/CTA), so a single call suffices. No need to split further.

6. **Soft-fail non-essential, hard-fail essential.** Copy generation IS the essential output of this endpoint — an LLM failure here returns 503 ServiceUnavailable, not a silent degradation.

## Key Types and Functions

### `emailCopyEventDetails`
Decode target for the brief's opaque `EventDetails` blob. Contains `eventName`, `location`, `startDate`, `endDate`. Only `eventName` is required (validated in `decodeEmailCopyEventDetails`).

### `decodeEmailCopyEventDetails(blob)`
Decodes the brief's `EventDetails` and validates that at least an event name is present. Unlike `audience_build.go` (which skips mismatched shapes), returns an error for missing or invalid event details, causing `GenerateEmailCopy` to return 400 BadRequest.

### `composeEmailCopyPrompt(vars)`
Returns (systemPrompt, userPrompt) built from fixed blocks. The system prompt contains role instruction, constraints, and the scrape-not-recall mandate. The user prompt carries the specific event details (name, location, dates). Prompt composition is deterministic and auditable.

### `formatEventDates(startDate, endDate)`
Formats start/end dates into a human-readable string. If both dates are present, joins them with " - ". If only one, returns it. If neither, returns "Date TBD".

### `parseEmailCopyResponse(raw)`
Parses the model's JSON response, stripping code fences (e.g., ```json). Returns `(*briefs.EmailCopy, error)`. If JSON parsing fails, returns an error; this is fail-closed because email copy is the primary output of this endpoint (no fallback to raw text).

### `truncateString(s, maxLen)`
Enforces length limits in code, stripping trailing whitespace. Applied after parsing to all four copy fields (subject, preheader, body, CTA).

### `(s *BriefService) GenerateEmailCopy(ctx, payload)`
Main handler. Loads the brief, decodes its event details, builds the prompt, calls the LLM client, parses and validates the response, and returns the `EmailCopy` result. Does NOT persist anything to the brief; it is a pure generation call.

## Configuration and Injection

The LLM client is optional: it is injected via `SetLLMClient()` in the container (`internal/container/container.go`). When not configured (AI_PROXY_URL or AI_API_KEY missing), the client is nil and `GenerateEmailCopy` returns 503.

- **`newLLMClient()` in container**: Builds the client from config. Returns nil if the proxy URL or API key is missing (no error, no crash).
- **`(c *Container).newBriefService()`**: Calls `s.SetLLMClient()` to inject it into every BriefService instance.

## Error Handling

- **400 BadRequest**: Event details missing or invalid (no event name).
- **503 ServiceUnavailable**:
  - LLM client is nil (model not configured).
  - LLM platform error (timeout, 5xx, etc.).
  - Model response is unparseable JSON.
  - Model response is missing required fields.
- **Other brief errors**: Propagated unchanged (e.g., brief not found → 404).

## Testing

The test suite (`internal/service/email_copy_test.go`) includes 18 test functions:

- **TestDecodeEmailCopyEventDetails**: Validates the opportunistic event-details decoder handles valid, partial, empty, invalid, and missing-name inputs.
- **TestParseEmailCopyResponse**: Validates JSON parsing with fence stripping and truncation of plain-text fields (subject, preheader, CTA).
- **TestFormatEventDates**: Validates date range formatting.
- **TestTruncateString**: Validates truncation and trailing-whitespace stripping.
- **TestComposeEmailCopyPrompt**: Validates prompt composition includes constraints and event details.
- **TestGenerateEmailCopy_NoLLMClient**: Validates 503 response when llmClient is nil.
- **TestGenerateEmailCopy_BriefNotFound**: Validates 404 when brief does not exist.
- **TestGenerateEmailCopy_InvalidEventDetails**: Validates 400 when event details lack a required name.
- **TestGenerateEmailCopy_LLMError**: Validates 503 when the LLM platform returns an error.
- **TestGenerateEmailCopy_HappyPath**: Validates the full flow with valid brief and LLM response.
- **TestGenerateEmailCopy_RejectsIncompleteCopy**: Validates 503 when any required field (subject/preheader/body/CTA) is blank.
- **TestGenerateEmailCopy_RejectsOverlongBody**: Validates 503 when body HTML exceeds 8000 chars (not truncated, as truncation corrupts markup).
- **TestGenerateEmailCopy_RejectsOversizedPrompt**: Validates 400 when composed prompt exceeds 3000 chars (prevents unbounded input-token cost).
- **TestGenerateEmailCopy_AcceptsSizeablePrompt**: Validates the full path for normally-sized event details (within prompt limit).
- **TestDecodeEmailCopyEventDetails_FailsWithoutName**: Mutation test validating scrape principle (no name → no generation).
- **TestFormatEventDates_RangeFormat**: Mutation test for date range format.
- **TestTruncateString_EnforcesLimit**: Mutation test for truncation limits.
- **TestParseEmailCopyResponse_EnforcesMaxLengths**: Mutation test for plain-text field truncation limits.

Each test is mutation-verified by reverting the corresponding logic and confirming the test fails with a meaningful diagnostic.

## Architectural Notes

- **No persistence**: `GenerateEmailCopy` returns a result but does NOT write to the brief. A later flow (e.g., a UI that calls both generate and update) would merge the copy into the brief's `Copy` field.
- **No refinement in scope**: The lfx-one reference includes a "refine" endpoint that skips re-scraping and feeds prior copy back capped at 10k chars. This initial slice omits refinement to keep the PR under the 1000-line cap.
- **Copy storage**: The generated copy can be stored in the brief's existing `Copy` field (keyed by channel, e.g. `{"email": {...}}`) or as a dedicated column in a future migration. For now, the UI/client is responsible for merge-and-persist if needed.
