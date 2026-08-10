# 2026-08-10 — AI-generated email copy for campaign briefs (LFXV2-2775, part 2 of 2)

**Update** — `internal/service` adds the `GenerateEmailCopy` method and supporting functions to generate email copy (subject, preheader, body, CTA) for campaign briefs using the LiteLLM proxy client delivered in part 1. The endpoint is POST `/projects/{project_id}/briefs/{brief_id}/email-copy`, returns a typed `EmailCopy` result, and implements the six design principles from the lfx-one reference implementation.

## What Was Built

AI-generated email copy (subject, preheader, body, CTA) for campaign briefs, using the LiteLLM proxy client delivered in PR #104. The feature implements the six design principles from the lfx-one reference implementation:

1. **Scrape, don't recall**: All factual claims originate from the brief's EventDetails (event name, location, date), never the model's training data.
2. **Compose prompts, don't branch**: Prompts built from fixed blocks, not if/else variants.
3. **Prompt limits advisory; code limits real**: Length limits enforced in Go after model response (subject ≤200, preheader ≤150, body ≤8000, CTA ≤50 chars).
4. **Parse defensively**: JSON parsing tolerates code fences and case variants; fails cleanly on invalid input.
5. **One concern per call**: Single LLM call per email generation, no split flows.
6. **Soft-fail non-essential, hard-fail essential**: LLM unavailable → 503 ServiceUnavailable; generation failure → 503, not silent degradation.

## Files Changed

### Design Layer
- **`design/brief.go`**: 
  - Added `EmailCopy` type with four required string fields (subject, preheader, body, cta), each bounded.
  - Added `generate-email-copy` method to the briefs service: POST `/projects/{project_id}/briefs/{brief_id}/email-copy`, returns `EmailCopy`, uses standard error envelope (`commonBriefErrors`).

### Service Layer
- **`internal/service/email_copy.go`** (new):
  - `emailCopyEventDetails`: Decode target for brief's EventDetails.
  - `decodeEmailCopyEventDetails()`: Opportunistic decode, validates event_name required.
  - `composeEmailCopyPrompt()`: Builds system and user prompts from fixed blocks.
  - `formatEventDates()`: Formats date ranges.
  - `parseEmailCopyResponse()`: Parses model JSON, strips fences, enforces limits.
  - `truncateString()`: Code-side truncation with trailing-whitespace stripping.
  - `GenerateEmailCopy()`: Main handler, full flow from brief load → prompt → LLM call → response parse → validation.

- **`internal/service/brief.go`** (modified):
  - Added `llmClient *llm.Client` field to `BriefService`.
  - Added `SetLLMClient(*llm.Client)` injection method (opt-in, mirrors `SetEventURL` pattern).
  - Added `snapshotLLMClient()` helper to read under lock.

### Testing
- **`internal/service/email_copy_test.go`** (new):
  - 9 test functions covering decode, parsing, truncation, date formatting, prompt composition, full generation flow, and three mutation-verification tests.
  - All tests mutation-verified by reverting logic and confirming failure.

### Container / Dependency Injection
- **`internal/container/container.go`** (modified):
  - Added `import "github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"`.
  - Added `newLLMClient()` method: builds the client from config (AI_PROXY_URL, AI_API_KEY, AI_MODEL). Returns nil if proxy URL or key missing (no error).
  - Modified `newBriefService()` to call `s.SetLLMClient(c.newLLMClient())` for every BriefService instance.

### Documentation
- **`docs/knowledge/code/internal-service-email-copy.md`** (new): Design principles, key types/functions, config & injection, error handling, testing approach, architectural notes.

### Generated Code
- **`gen/lfx_v2_campaign_service_briefs/`**: Regenerated after adding `generate-email-copy` method and `EmailCopy` type. Includes service interface, payload type, HTTP encoder/decoder, OpenAPI spec.

## Scope Decisions

### In Scope
- AI-generated email copy (subject, preheader, body, CTA).
- LLM unavailable handling (503 ServiceUnavailable).
- Code-side length enforcement (truncation + validation).
- Comprehensive test coverage with mutation verification.

### Out of Scope (Noted for Future)
- **No persistence**: Generated copy is NOT written to the brief's `Copy` field by this endpoint. A later flow (UI → generate → update-brief) would persist if needed.
- **No refinement**: The lfx-one reference includes `POST .../email-copy/refine` (skips re-scraping, feeds prior copy capped at 10k chars). Deferred to keep this PR under 1000 lines.
- **Copy field shape**: Currently using existing `Copy` JSON field (could nest by channel: `{"email": {...}}`). No new migration required; UI handles merge-and-persist.

## Validation

```
go build ./... ✓
go vet ./...   ✓
go test -race ./... ✓ (all 9 email-copy tests pass)
go run ./cmd/okfvalidate ./docs/knowledge ✓ (OKF-conformant)
```

Lines of code:
- `email_copy.go`: ~280 (logic + comments)
- `email_copy_test.go`: ~330 (tests)
- Modified existing files: ~30 (container + service struct + design)
- Total: ~640 lines

## Integration Points

1. **LLM Client** (PR #104, already landed): `internal/platform/llm/client.go` is stable. This PR calls `llmClient.Complete(ctx, systemPrompt, userPrompt)`.
2. **Brief Repository**: Unchanged; `GenerateEmailCopy` calls existing `briefRepo.GetBrief()`.
3. **Config** (already extended in PR #104): Uses AI_PROXY_URL, AI_API_KEY, AI_MODEL from environment.
4. **OpenAPI**: Auto-generated; new endpoint documented in spec with 200/400/503 responses.

## Next Steps (Not in This PR)

1. **Refinement endpoint** (`POST .../email-copy/refine`): For iterative copy improvement.
2. **Persistence flow**: UI integration to merge generated copy into brief's `Copy` field.
3. **Telemetry**: Log generated copy tokens/cost for billing/budgeting.
4. **Config UI**: Allow operators to enable/disable email generation per deployment.
