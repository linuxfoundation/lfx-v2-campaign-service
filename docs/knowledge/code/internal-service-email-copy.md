---
type: "Code Concept"
title: "Email Copy Generation (internal/service/email_copy.go)"
description: "AI-generated email copy (subject, preheader, body, CTA) for campaign briefs using the LiteLLM proxy client. Implements scrape-not-invent, defensive parsing, code-enforced length limits, and graceful degradation when the model is unconfigured."
resource: "internal/service"
---

# Email Copy Generation (`internal/service/email_copy.go`)

## Overview

Email copy generation uses the LiteLLM client in `internal/platform/llm` to generate AI-written email subject lines, preheaders, bodies, and CTAs for campaign briefs. It is an **enrichment, not a core feature**: when the AI model is not configured, `GenerateEmailCopy` returns 503 ServiceUnavailable, leaving other brief routes unaffected.

## Design Principles

The implementation follows six principles from the reference lfx-one implementation:

1. **Scrape, don't recall.** Every factual claim originates from the brief's `EventDetails` (event name, location, date, etc.), never the model's training data. The system prompt explicitly forbids invention: `"use ONLY the event details provided below; never invent dates, names, or locations"`.

2. **Compose prompts, don't branch — with one named exception.** Prompts are built by concatenating fixed blocks (role instruction, constraints, event-details block, stage brief) rather than if/else-ing variants. The exception is an ABSENT stage, which returns a frozen copy of the pre-stage prompt: LFXV2-1940 requires those callers to keep receiving byte-identical prompts, and the stage-aware text is not a superset of the old one. Naming the exception here is the point — an invariant with a silent hole in it is worse than one with a stated boundary.

3. **Prompt limits are advisory; code limits are real.** The system prompt tells the model "subject under 60 characters", but `GenerateEmailCopy` also enforces the limits in code: `truncateString()` cuts subject at 200, preheader at 150 and CTA at 50 runes, while an over-long body is rejected rather than cut (see `truncateString` below for why the body is the exception).

4. **Parse defensively, fail-closed.** The model's response must be JSON, optionally wrapped in ```json fences. The parser strips fences and calls `json.Unmarshal` directly; any leading or trailing prose outside the fences will cause JSON parsing to fail, triggering a 503 response. This is intentional: email copy is the primary output of this endpoint, so a malformed response is a generation failure, not a fallback case.

5. **One concern per AI call.** Email copy has one shape (subject/preheader/body/CTA), so a single call suffices. No need to split further.

6. **Soft-fail non-essential, hard-fail essential.** Copy generation IS the essential output of this endpoint — an LLM failure here returns 503 ServiceUnavailable, not a silent degradation.

## Key Types and Functions

### `emailCopyEventDetails`
Decode target for the brief's opaque `EventDetails` blob. Contains `eventName`, `location`, `startDate`, `endDate` and `dates`. Only `eventName` is required (validated in `decodeEmailCopyEventDetails`).

`dates` is the COMBINED free-text form the scraper produces ("19-20 November 2026"), and it is what briefs written by the UI actually carry: `startDate`/`endDate` are paid-platform config fields and are empty on an email brief. Reading only the structured pair yielded "Date TBD" for every email while the real dates sat in the brief beside them.

### `decodeEmailCopyEventDetails(blob)`
Decodes the brief's `EventDetails` and validates that at least an event name is present. Unlike `audience_build.go` (which skips mismatched shapes), returns an error for missing or invalid event details, causing `GenerateEmailCopy` to return 400 BadRequest.

### `composeEmailCopyPrompt(vars)`
Returns (systemPrompt, userPrompt) built from fixed blocks. The system prompt contains role instruction, constraints, and the scrape-not-recall mandate. The user prompt carries the specific event details (name, location, dates). Prompt composition is deterministic and auditable.

### `resolveEventDates(details)`
Picks the best date string the brief actually carries. The structured `startDate`/`endDate` pair wins when present — a caller that set it meant it. The combined `dates` string is the fallback rather than the primary because it is free text from a scrape. Returns "Date TBD" when neither exists: the prompt instructs the model never to invent dates, so a wrong string is worse than an absent one.

### `formatEventDates(startDate, endDate)`
Formats the structured pair into a human-readable string. Both present and equal returns the single date; both present and different joins them with " - "; one present returns it; neither returns "Date TBD".

### `parseEmailCopyResponse(raw)`
Parses the model's JSON response, stripping code fences (e.g., ```json). Returns `(*briefs.EmailCopy, error)`. If JSON parsing fails, returns an error; this is fail-closed because email copy is the primary output of this endpoint (no fallback to raw text).

### `truncateString(s, maxLen)`
Enforces length limits in code, stripping trailing whitespace. Applied after parsing to **three** of the four copy fields — subject (200), preheader (150) and CTA (50). **Not** to `body`, deliberately: truncating HTML at an arbitrary rune boundary cuts inside tags, attributes or entities and drops closing tags, so an oversized body is *rejected* instead, at the 8000-rune bound that mirrors `MaxLength(8000)` on the design's `body` attribute. Do not "restore consistency" by truncating it.

That asymmetry is why `GenerateEmailCopy`'s required-field check trims before comparing. `truncateString` is what strips trailing whitespace, so a whitespace-only subject, preheader or CTA already arrived empty; a whitespace-only **body** did not, and used to pass the check and return 200 with a blank email.

### The prompt bound is checked twice, in runes, against TWO different limits

`maxPromptSize` is 2400 **runes** and bounds the three event-detail fields BEFORE
`composeEmailCopyPrompt`; `maxComposedPromptSize` is 8400 and bounds the composed prompt after.
They are separate constants because they measure different things — the caller's input versus that
input plus the stage template — and getting the second number wrong fails in TWO opposite
directions, both of which this file has actually shipped:

- **Too low rejects valid input.** At 6500 the Post-Event template (5041 runes on its own) left
  only ~1459 runes for caller fields, so 1618 runes of event details passed the 3000 pre-check and
  were then refused by the composed one — two bounds contradicting each other, with the caller told
  their input was too large immediately after the first accepted it.
- **Too high never fires.** 8000 was set against a believed ceiling of 7700 and the real ceiling
  was 8041, so it fired only for the very largest Post-Event input.
  `TestGenerateEmailCopy_ComposedBoundIsReachable` exists to catch this, and it passed for a reason
  nobody had checked.

**The two are not satisfiable together — by construction, not by choice of numbers.** "Never refuse
what the pre-check accepted" needs the composed bound at or above the worst valid composition;
"reachable by caller input" needs it below. Three revisions tried to find a pair that did both.
None can.

The first property wins, because a caller must never be told their input is too large by the second
of two checks after the first accepted it. The composed bound is then sized for the case that
remains: **a stage template growing past the budget in a future edit**. That is a real failure mode
— the templates are large (Post-Event is 5503 runes on its own) and hand-edited.

With the input bound at 2400 the worst valid composition is 7903 (Post-Event floors at 5503), so
8400 clears it with ~497 runes of headroom. `TestGenerateEmailCopy_ComposedBoundIsReachable` drives it that
way, by injecting an oversized stage into `emailstage.Templates` rather than a long event name.

That test was a **false green** for one revision: once the input bound moved to 2400, its 2500-rune
event name hit the PRE-check instead, and both guards returned the same `BadRequestError` message,
so nothing could tell them apart — neutralising the composed guard entirely broke no test. The two
messages now differ, and the test asserts the composed one specifically.

Neither check is redundant. The pre-check is what makes the bound real: `event_details` is declared `Any` in
`design/brief.go`, so none of the three fields carries a length constraint, and a post-hoc
check formats a 50MB stored event name into a new string before measuring it — the allocation
the guard exists to prevent, performed by the guard's own input. The three fields alone cannot
exceed the total, so the pre-check is a sound necessary condition; it is not sufficient,
because the fixed template counts too, which is what the second check is for.

Runes, not bytes, because the limit is stated to the caller and logged as a character count and
every other bound in this file counts runes. `len()` gave an event named in Japanese a third of
the advertised budget and an event named in English all of it — a limit that means something
different depending on the alphabet. Measured, not estimated, and re-measured whenever the
shared prompt or any template changes: Post-Event is the largest stage at 5503 runes on its own,
and with the maximum 2400 runes of caller input it composes to 7903 against the 8400 bound.

Every figure in this section has been wrong at least once from a measurement taken before a
template grew — three times, most recently when a paragraph added to the shared system prompt grew
every stage by ~130 runes. A prose instruction to re-measure did not survive contact, so
`TestComposedBoundClearsEveryStageFloor` now COMPUTES every stage floor from the real constants and
fails if `worst + maxPromptSize` exceeds `maxComposedPromptSize`. Derive these numbers from that
test rather than carrying them forward.

### The stage selects the template, and an unrecognised one does not fail

`emailstage.Resolve` maps the requested stage onto one of six templates and falls back to
Registration Push for anything it does not recognise. That fallback is the CONTRACT (LFXV2-1940),
not an implementation convenience: a caller is never blocked from generating copy by a stage it
cannot spell.

An ABSENT or blank stage never reaches `Resolve`. LFXV2-1940 also requires that a caller sending
no stage produce a BYTE-IDENTICAL prompt to the pre-stage one, and the stage-aware system prompt
is not a superset of that text -- it adds placeholder/OMIT rules that only mean anything beside a
stage brief. So `composeEmailCopyPrompt` returns the frozen `legacySystemPrompt` for that case
instead. `Resolve` still treats the empty string as Registration Push for any other caller.

The `stage` attribute is therefore free text with no `Enum`. An enum was tried and removed: Goa
validates it in the generated decoder, so an unrecognised value became a 400 before `Resolve`
could run, which is the opposite of the specified behaviour.

The cost is real and worth naming — a TYPO produces Registration Push copy under a 200, so a
caller asking for "Fnal Countdown" gets the wrong kind of email and is told it succeeded. The
response does not report which stage was actually used, so a caller that needs to know must
compare what it sent against `emailstage.Names()`.

It is a QUERY parameter, not a body field. As a body attribute it made the whole request body
mandatory — Goa emits `requestBody.required: true` and the decoder answers `MissingPayloadError`
on EOF — so every existing body-less POST began failing with a 400.

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

The test suite lives in `internal/service/email_copy_test.go`. The inventory below names every
test rather than counting them: a count goes stale on the next commit and, unlike a missing name,
nothing greps for it.

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
- **TestGenerateEmailCopy_RejectsOversizedPrompt**: Validates 400 when the caller's EVENT DETAILS exceed 2400 runes (prevents unbounded input-token cost). This is the input pre-check, not the composed bound — it drives a ~4000-rune name, which never reaches composition.
- **TestGenerateEmailCopy_PromptLimitCountsRunesNotBytes**: Pins the UNIT of that limit. A 998-rune / 2994-byte Japanese event name is inside the documented allowance and must reach the model; counting bytes rejected it, giving a caller who names their event in Japanese a third of the budget an English name gets.
- **TestGenerateEmailCopy_RejectsOversizedInputBeforeComposing**: Pins WHERE the limit is enforced, by asserting the warn line — `input_size` for the pre-composition check, `prompt_size` for the post-composition one. The two outcomes are now distinguishable from outside as well (400 vs 503, different messages), but the log assertion is the sharper one and stays.
- **TestGenerateEmailCopy_ComposedBoundIsReachable**: Validates 503 when a STAGE TEMPLATE overflows the composed bound, by injecting an oversized stage into `emailstage.Templates`. It cannot be driven by caller input — see the two-bound section above — and it asserts the 503 specifically, so a pre-check 400 cannot masquerade as coverage.
- **TestGenerateEmailCopy_AcceptsSizeablePrompt**: Validates the full path for normally-sized event details (within prompt limit).
- **TestDecodeEmailCopyEventDetails_FailsWithoutName**: Mutation test validating scrape principle (no name → no generation).
- **TestFormatEventDates_RangeFormat**: Mutation test for date range format.
- **TestTruncateString_EnforcesLimit**: Mutation test for truncation limits.
- **TestParseEmailCopyResponse_EnforcesMaxLengths**: Mutation test for plain-text field truncation limits.
- **TestResolveEventDates**: Pins the fallback order — the structured `startDate`/`endDate` pair wins, the scraper's combined `dates` string is the fallback, and "Date TBD" is the answer when neither exists.
- **TestGenerateEmailCopy_StageReachesThePrompt**: Pins that the caller's `stage` actually reaches the composed prompt, rather than being accepted and dropped.
- **TestGenerateEmailCopy_NilStageIsNotAnError**: A caller that names no stage gets generated copy, not a 400 — the stage is optional by contract.
- **TestAbsentStageProducesLegacyPrompt**: That same caller gets the pre-stage prompt byte for byte, pinned against goldens extracted from `012fa822^`; an explicit stage must NOT produce it, so the branch cannot swallow stage selection.

Each test is mutation-verified by reverting the corresponding logic and confirming the test fails with a meaningful diagnostic.

## Architectural Notes

- **No persistence**: `GenerateEmailCopy` returns a result but does NOT write to the brief. A later flow (e.g., a UI that calls both generate and update) would merge the copy into the brief's `Copy` field.
- **No refinement in scope**: The lfx-one reference includes a "refine" endpoint that skips re-scraping and feeds prior copy back capped at 10k chars. This initial slice omits refinement to keep the PR under the 1000-line cap.
- **Copy storage**: The generated copy can be stored in the brief's existing `Copy` field (keyed by channel, e.g. `{"email": {...}}`) or as a dedicated column in a future migration. For now, the UI/client is responsible for merge-and-persist if needed.
