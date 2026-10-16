# 2026-10-15 — LFXV2-2665: negative-keyword code documented in the platform and dispatch concepts

**Docs** — The Reddit, X and Microsoft negative-keyword code was described only in the
architecture concept (`architecture/keyword-targeting-reddit-x.md`) and the Microsoft client
concept. The per-package concepts did not cover `internal/platform/reddit/excluded_keywords.go`,
`internal/platform/twitter/negative_keywords.go`, `internal/dispatch/negative_keywords.go` or the
Reddit and X dispatch adapters, and only touched on the Microsoft adapter.

Added, each checked against origin/main code:

- `code/internal-platform-reddit.md` — "Ad-group excluded keywords": the accessors, the
  whole-targeting PATCH request shape, refusals before any request, the single send (429
  UNCONFIRMED), `IsPlatformRefusal`, the echo check, and the tests.
- `code/internal-platform-twitter.md` — "Line-item negative keywords": `NegativeKeyword()`, the
  `NE` criterion POST, refusals before sending, the write pacer and `ErrWriteNotSent`, the single
  send, the echo check, and the tests. Also the pre-existing gap first recorded in
  `2026-10-16-LFXV2-2665-google-request-not-transmitted.md`: two unmarked pre-send exits reach the
  dispatcher as `REJECTED`.
- `code/internal-dispatch.md` — "Reddit, X and Microsoft negative-keyword adapters": the shared
  batch check, a per-platform table (match types, caps, `revision`, gate, target, provenance,
  account, ownership read, read before write), the guard order, the absence of a last-keyword
  guard, idempotency and duplicate keys, the outcome classification, when the call errors, and the
  tests.
- `code/internal-platform-microsoft.md` — a paragraph on what the dispatcher adds around
  `AddCampaignNegativeKeywords`, and the tests that pin it.
- `architecture/keyword-targeting-reddit-x.md` — links from its Code list to these sections.

The text describes main. Open PR #359 (`fix/keyword-reads-identityjson`) proposes `identityjson`
checks on the Reddit and X pre-reads and the X create echo; those checks are not described as
existing. No concept description changed, so no index bullet changed. No code changed.
