# 2026-10-07 — LFXV2-2665 settings readback: #284 review follow-ups

**Fix** — Two review threads on the merged Microsoft/Meta/Reddit/X settings readback (#284):

- **Meta 100/33 is unverifiable, never absent.** `meta.Client.GetCampaignSettings` no longer
  turns Graph code 100 / subcode 33 on the campaign into absence after an account-scoped probe:
  an account that loads does not show that THIS campaign is gone rather than hidden from the
  token or refused for the operation. It is now an error (503) on every HTTP status, exactly as
  the adoption lookup treats it, and the probe (`proveAccountLoads`) is removed. Meta's readback
  therefore never answers 404 for the platform campaign; a `DELETED`/`ARCHIVED` campaign is still
  reported with its status. The only Meta 404s are a missing campaign row or no connection.
- **X budget comparison is gated on what X reports, created campaigns included.** The create
  path is NOT changed to send `budget_optimization: CAMPAIGN`: X's current reference lists
  `LINE_ITEM` as its only POST value (and default), while the v11 announcement says `CAMPAIGN` is
  the default, so an explicit `CAMPAIGN` is undocumented. The readback keeps comparing the budget
  only on a reported `CAMPAIGN`; a created campaign reporting it absent or `LINE_ITEM` reads
  `unknown`. New tests pin the readback for a created campaign under each answer, and
  `TestCreateSendsQueryParams` pins that the create sends no `budget_optimization`.

Docs: `docs/api-catalog.md` (`/settings` row), [internal/dispatch](../code/internal-dispatch.md),
[internal/platform/meta](../code/internal-platform-meta.md),
[internal/platform/twitter](../code/internal-platform-twitter.md).
