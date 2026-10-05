# 2026-10-05 — Microsoft config_snapshot no longer stores caller links verbatim

**Fix** — `campaignFromMicrosoft` passed the caller's raw `microsoftConfig` to
`applyCampaignConfig`, so whatever the caller wrote went into `campaigns.config_snapshot`,
which is stored UNENCRYPTED and indexed. The Reddit, Meta, Google Ads and X adapters already
scrub their snapshot copies; Microsoft was the one that did not.

Field inventory. `microsoftConfig` has no dedicated URL field — the ad's `FinalUrls` is the
brief's registration URL plus the client's `utm_*` params and is never part of the struct.
Two fields are caller free text the client forwards verbatim, and are now passed through the
shared `sanitizeSnapshotText` (scheme+host only; query, fragment, path and userinfo dropped)
in the snapshot copy:

- `keywords[].text` — up to 100 runes of arbitrary text; a pasted link is a shape it can take.
- `timeZone` — meant to be a Microsoft enum, but not validated by the client.

Kept verbatim because they cannot carry a URL: `budget`, `cpcBid`, `keywords[].matchType`
(only Exact/Phrase/Broad passes the client before a snapshot is written) and `geoTargets`
(ISO-2 codes, shape-checked by the client).

The new `microsoftSnapshotConfig` builds a copy and reallocates `Keywords` before rewriting
it, so the values sent to Microsoft are unchanged. No helper moved: `sanitizeSnapshotURL` and
`sanitizeSnapshotText` already live in `internal/dispatch/creds.go`, shared by every adapter.
The persisted `result` needed no change — `CampaignResult.Steps` interpolate only ids,
counts and geo codes, and `microsoftAdsUrl` is composed from the account id.

Tests (`internal/dispatch/microsoft_snapshot_test.go`): a dispatch whose keywords, time zone
and brief registration URL carry secret-bearing paths, queries and fragments persists none of
them in `config_snapshot` or `result`, while the `/Keywords`, `/Campaigns` and `/Ads` bodies
still carry the full values; and the snapshot helper does not mutate the dispatch config.
