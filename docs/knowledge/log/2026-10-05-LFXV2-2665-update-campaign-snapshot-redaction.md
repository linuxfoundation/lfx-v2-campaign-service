# 2026-10-05 — Update-campaign config is redacted before config_snapshot

**Fix** — `BriefService.UpdateCampaign` persisted the caller's `config` (Goa `Any`) straight
into the UNENCRYPTED, indexed `campaigns.config_snapshot`, bypassing every dispatch adapter's
create-time scrubbing, so a token in a link's query, fragment, userinfo or path landed in the
clear.

- The pure snapshot string redactors (`sanitizeSnapshotURL` / `sanitizeSnapshotText` and their
  run patterns) moved unchanged to `pkg/redact/snapshot.go` as `SnapshotURL` / `SnapshotText`.
  `internal/dispatch` imports `internal/service`, so the service could not import dispatch
  without a cycle. Dispatch keeps one-line wrappers, so adapter behaviour is byte-identical and
  `creds_test.go` passes unchanged.
- `UpdateCampaign` now persists `redactedConfigSnapshot(config)`: the value is walked
  recursively and every string value AND every object key goes through `redact.SnapshotText`
  (keys are caller-typed too). Keys that collide after redaction are never merged: in sorted
  original-key order the first keeps the key and later ones get `#2`, `#3`, …. Numbers,
  booleans, null and structure are preserved; a nil config still leaves the snapshot as it was.
  The API contract is unchanged (any JSON value is still accepted).
- Other paths writing caller JSON into `config_snapshot` were checked: the only other writer is
  `applyCampaignConfig` on the dispatch create/adoption path, which snapshots each adapter's
  validated struct. The LinkedIn adapter still scrubs nothing there; that is owned elsewhere and
  not changed here.
- Rows written before this fix are NOT backfilled: a snapshot already holding an unredacted
  link keeps it until the campaign's config is next updated.
