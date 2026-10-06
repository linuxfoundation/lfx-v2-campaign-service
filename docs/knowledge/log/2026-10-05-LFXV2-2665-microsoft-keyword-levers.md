# 2026-10-05 — Microsoft Advertising keyword levers: pause/remove and negative keywords

**Creation** — two keyword levers for live Microsoft Advertising campaigns.

- **`apply-keyword-actions` now accepts Microsoft campaigns.** PAUSE is one `UpdateKeywords`
  (`PUT Keywords`), REMOVE one `DeleteKeywords` (`DELETE Keywords`), pause first. Same guard order
  as Google's adapter — batch, provisioning, the row's ad group, provenance failing closed,
  account match — plus a `GetKeywordsByAdGroupId` ownership read that refuses any id that is not a
  live keyword of this campaign's ad group, all before anything is mutated.
- **Not atomic, so outcomes are per action.** `KeywordActionResult` gains optional `outcome`
  (`APPLIED`/`FAILED`/`UNCONFIRMED`) and `error_code`, and `resource_name` becomes optional
  (Microsoft has none). One result per action, in request order; `applied_count` counts `APPLIED`.
  Whole-call ambiguity — 5xx, timeout, unreadable 200, a refusal after a retried 429 — is
  UNCONFIRMED. Google responses are byte-identical: Google sets `resource_name` on every result
  and never sets the new fields (pinned against the generated encoder).
- **New `add-negative-keywords`** (`POST …/campaigns/{campaign_id}/negative-keywords`), a separate
  `NegativeKeywordAdder` capability rather than a `KeywordAction` kind, so it cannot reach Google's
  adapter. Microsoft `AddNegativeKeywordsToEntities` at campaign level; Exact/Phrase, ≤100
  characters, a narrow character set, 1–60 per request. Per-item outcomes; a 4335 already-exists is
  `ALREADY_PRESENT`. Not retried on 429. Persists nothing, so no If-Match — like keyword actions.
- **Status cascade fix that REMOVE made necessary.** `ToggleStatus` narrows the row's recorded
  keyword ids to the live ones before cascading, so a removed keyword cannot turn every later
  toggle into an unconfirmed partial cascade.
- No `MICROSOFT_*` gate: only the Reporting reads are gated, never a Microsoft write.
