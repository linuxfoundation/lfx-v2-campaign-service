# 2026-10-15 — Keyword-lever pre-reads refuse untrustworthy bytes

**Fix** — the three keyword-lever pre-reads now run `identityjson.Check` over the raw body before
decoding, as their sibling reads on the same platforms already did.

- Reddit `GetAdGroupTargeting`: a refused body is `ErrTargetingUnreadable` (definite, no response
  bytes in the error). It feeds `RemoveAdGroupKeywords` and `AddAdGroupExcludedKeywords`, which
  PATCH the whole targeting back, so a duplicated `keywords` inside `targeting` or a duplicated
  data-level `id`/`campaign_id` could have written back a last-wins reading. Precedent:
  `campaign_settings.go` and `campaign_lookup.go`.
- X `ListLineItemTargetingCriteria`: every page is checked; a refusal is the existing all-or-error
  `ErrTargetingUnreadable`. The `DeleteTargetingCriterion` and `CreateNegativeKeywordCriterion`
  echoes are checked too, but those writes were SENT, so a refused echo is the existing
  UNCONFIRMED `transportError` — not a definite refusal, and not retried. Precedent: `audience.go`
  and `campaign_settings.go`.
- Microsoft `GetAdGroupKeywords`: `identityjson.Check(body)` before the decode; a definite read
  failure, so `ApplyKeywordActions` sends no mutation. Precedent: `campaign_lookup.go` and
  `campaign_settings.go` (`queryCampaignByIDGuarded`).
- Tests: platform tests per read (duplicate keys at several levels, case-folded duplicates,
  unpaired surrogate, invalid UTF-8) and per echo (IsOutcomeUnconfirmed, exactly one request);
  dispatch tests that a Reddit removal / excluded-keyword add sends no PATCH, an X removal no
  DELETE and an X negative add no POST, and a Microsoft keyword action no mutation. Each body's
  last-wins reading is otherwise valid, so every guard was mutation-checked: replacing each
  `Check` argument with `nil` fails its tests.
- Concepts updated: `code/internal-platform-reddit.md`, `code/internal-platform-twitter.md`,
  `code/internal-platform-microsoft.md`, `code/internal-platform-identityjson.md`,
  `architecture/keyword-targeting-reddit-x.md`.
