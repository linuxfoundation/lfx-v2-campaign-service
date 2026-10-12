# 2026-10-07 — Adopt existing Microsoft, Meta, Reddit and X campaigns

**Update** — `adopt-campaign` now works on `microsoft-ads`, `meta-ads`, `reddit-ads` and
`twitter-ads`, not only `google-ads`. Each dispatcher implements `service.CampaignAdopter`
(`internal/dispatch/{microsoft,meta,reddit,twitter}_adopt.go`, shared order in `adopt.go`), which is
what takes it off the type-assertion `ErrAdoptionUnsupported` path. Each validates the id format
before any connection work (400), resolves the project's OWN connection only
(`resolveOwnedForAdoption`, 409 without one), reads the campaign once by id — Microsoft
`GetCampaignsByIds`, Meta `GET /{id}?fields=…`, Reddit `GET /ad_accounts/{a}/campaigns/{id}`, X
`GET accounts/:a/campaigns/:id` — and proves provenance: account-scoped on Microsoft, the answer's
`account_id` on Meta, path-scoped plus a compare-when-reported on Reddit and X. A foreign-account
campaign is `ErrCampaignAccountMismatch`, which `AdoptCampaign` now answers 409. "No such campaign"
(and a deleted/archived one) is 404; transport, 5xx, retried-out 429, auth failures, unknown status
and any body `internal/platform/identityjson` refuses are 503 "could not be verified". Rows record
provenance only, so every toggle still refuses ACTIVATE (the four refusals now name adoption) and
allows PAUSE. Verified against published API documentation and httptest stubs only, not live
accounts. See [internal/dispatch](../code/internal-dispatch.md),
[internal/platform/identityjson](../code/internal-platform-identityjson.md) and the
[API catalog](../architecture/api-catalog.md).
