# 2026-10-05 — Microsoft config_snapshot scrubs the unvalidated timeZone

**Fix** — `campaignFromMicrosoft` passed the caller's raw `microsoftConfig` to
`applyCampaignConfig`, so whatever the caller wrote went into `campaigns.config_snapshot`,
which is stored UNENCRYPTED and indexed. It now persists `microsoftSnapshotConfig(cfg)`.

What each adapter scrubs at this point: Reddit `PostURL`/`ImageURL`; Meta each variant's
`ImageURL`; Google Ads each sitelink's `finalUrl` (keyword and ad text kept verbatim by
design); X `tweetText`; HubSpot snapshots provenance fields only; Microsoft now `timeZone`.
LinkedIn still passes its raw config and scrubs nothing (variant `introText`/`headline` are
stored verbatim); it is owned by another engineer and not changed here.

Field inventory. `microsoftConfig` has no URL field — the ad's `FinalUrls` is the brief's
registration URL plus the client's `utm_*` params and is never part of the struct.

- `timeZone` — meant to be a Microsoft enum, but forwarded unvalidated, so effectively free
  text. Reduced with `sanitizeSnapshotText` (a link-shaped run becomes scheme+host); real
  enum values pass through unchanged.
- `keywords[].text` — kept VERBATIM, matching `googleAdsSnapshotConfig`. The first cut ran it
  through `sanitizeSnapshotText` too; pre-PR review found the prose redactor's path-only pass
  rewrote legitimate keywords (`k8s.io/docs tutorial` → `k8s.io tutorial`, `node.js/express`
  → `node.js`, `10.0.0.0/8` → `10.0.0.0`), so that was reverted before merge.
- `budget`, `cpcBid`, `keywords[].matchType` (only Exact/Phrase/Broad passes the client
  before a snapshot is written) and `geoTargets` (ISO-2 codes, shape-checked by the client)
  cannot carry a URL and are kept verbatim.

No helper moved: `sanitizeSnapshotURL` and `sanitizeSnapshotText` already live in
`internal/dispatch/creds.go`. The persisted `result` needed no change —
`CampaignResult.Steps` interpolate only ids, counts and geo codes, and `microsoftAdsUrl` is
composed from the account id.

Tests (`internal/dispatch/microsoft_snapshot_test.go`): a dispatch with a link-shaped
`timeZone` and a secret-bearing brief registration URL persists neither in `config_snapshot`
or `result`; `k8s.io/docs tutorial`, `node.js/express`, `kubernetes.io` and `c++ jobs` are
stored byte for byte; the `/Keywords`, `/Campaigns` and `/Ads` bodies still carry the full
values; and the snapshot helper does not mutate the dispatch config.

Known gap, not fixed here: `UpdateCampaign` in `internal/service/brief.go` writes
`ConfigSnapshot` from the caller's config with no adapter scrubbing, bypassing all of the
above on the update path.
