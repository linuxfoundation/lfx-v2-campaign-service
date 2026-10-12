# 2026-10-07 — LFXV2-2665 Meta audience read: pre-PR review fixes

**Fix** — Fixes from the pre-PR review of the Meta audience read (`get-meta-ads-audience`):

- **Case-folded duplicate keys.** `meta.rejectDuplicateKeys` compared row keys with `==`, but
  encoding/json matches keys to struct fields case-insensitively (including KELVIN SIGN → `k` and
  LONG S → `s`) and keeps the last match, so `{"Campaign_ID":"<theirs>","campaign_id":"<ours>"}`
  passed the guard and the scope check. Keys are now compared with `strings.EqualFold`; such a
  row fails the whole read (503, no partial rows). Audit of the other guards: Meta, Reddit and X
  adoption reads use `identityjson.Check`, whose `foldKey` already folds case, KELVIN SIGN and
  LONG S, so it needed no change.
- **Route-neutral 409.** The account-mismatch message shared by every insights read said "to read
  their keywords"; it now says "to read this data".
- **Shared orchestrator helper.** `ReadAudienceInsights` and `ReadMetaAudienceInsights` now share
  the generic `readScopedAudience` (scope, empty-scope early return, `metricsCallTimeout`,
  `read_audience` metric, nil-result guard, nil-slice normalisation). No behaviour change; the
  Google tests pass unchanged.
- **Docs.** `docs/api-catalog.md` now says that the same connection defects that are 400 on the
  project's own connection are 500 on the LF system fallback. It also documents the 20s
  `metricsCallTimeout` budget, which covers up to 2×20 sequential pages plus 429 backoff, so a very
  large project can 503 consistently.
