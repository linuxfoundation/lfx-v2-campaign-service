# 2026-08-07 — LFXV2-2023 GA-3b: drop the stale "GA-3c not wired" claim; split log fragments

**Update** — Closed the suppressed Copilot findings on PR #67.

- **The 409 refusing ACTIVATE named a reason that is no longer true.** It said the campaign
  cannot be activated "because the dispatcher cascade (GA-3c) is not wired and targeting
  (GA-4) is not provisioned" — but the PAUSE cascade is wired a dozen lines below the guard
  that raises it. Missing GA-4 targeting is now the ONLY stated reason, both in the error an
  API consumer sees and in the four comments that repeated the claim
  (`internal/dispatch/googleads.go`'s `ToggleStatus` doc, its guard comment, and its
  unreachable-tail comment; `internal/platform/googleads/adgroup_ad.go`'s
  `UpdateAdGroupAndAdStatus` doc; `campaign.go`'s `UpdateCampaignStatus` doc). The
  unreachable-tail comment also cited a hard-coded line number, which drifts on every edit —
  it now names the guard instead.
- **`campaign.go`'s `EventSlug` doc pointed at the wrong file.** `buildAdFinalURL` lives in
  `ad_copy.go:246`, not `adgroup_ad.go`.
- **Three log fragments carried more than one entry.** `CLAUDE.md` requires one file per
  entry. `2026-08-04-ga3b-adgroup-ad-cascade.md` held three, `2026-08-05-ga3c-status-toggle-cascade.md`
  three, and `2026-08-05-ga3b-review-fixes.md` two (the second under a `**Fix**` marker
  rather than `**Update**`). Each trailing entry moved to its own dated, ticket-slugged
  fragment; text is unchanged except for one back-reference that now names the fragment it
  used to sit beneath, and the `**Fix**` marker normalized to `**Update**`.

Dealako's five review findings (2026-08-07T00:09) were all closed by commits `355a2280`
through `d42b1aee`, which landed after that review was submitted: the partial-cascade
"ad-group succeeds, ad fails" case is covered by `TestUpdateAdGroupAndAdStatus`'s
`IsOutcomeUnconfirmed` + `partialCascadeError{stage: "ad"}` assertions; the dispatcher test
now decodes the `adGroupAds:mutate` body and asserts `Headlines`/`Descriptions` land in
their own asset lists (and NOT in each other's) plus `RegistrationURL`/`EventSlug` in the
final URL; `TestCreateAdGroupAndAd_AdCreationFails` captures the partial `CampaignResult`
via `assertPartialAdGroupResult`; and the weighted (not plain-rune) headline/description
limits are documented in `internal-platform-googleads.md`. The `ToggleStatus` stale-shell
claim he flagged is the same one fixed above.
