# 2026-11-13 — #582 LinkedIn objective readback trusts only adapter-written snapshots

**Fix** — the LinkedIn settings readback's recorded `objective_type` for an automatic row now
applies the absent-objective default (`WEBSITE_VISIT`) only to a config snapshot the adapter
provably wrote. `UpdateCampaign` replaces `config_snapshot` with caller JSON, and
`linkedInConfig.objective` is `omitempty`, so key absence alone could not prove authorship: a
`MAX_CONVERSION` campaign whose config was replaced read as recorded `WEBSITE_VISIT` (a false
`diverged`), and a caller could flip it.

- `linkedinSnapshotObjective` requires the non-`omitempty` `lifetimeBudget` key
  (`linkedinSnapshotAuthorshipKey`), which `applyCampaignConfig(…linkedinSnapshotConfig(cfg))`
  always marshals. An empty snapshot, a non-object, a snapshot without that key (including the
  `{"linkedInConfig":{…}}` envelope), an explicit-null or non-string `objective`, or a value
  outside `traffic`/`conversions` now reads `unknown`, each logged — the approach
  `googleAdsRecordedChannelType` takes for Google Ads' `channel`.
- `TestLinkedIn_ObjectiveRefusedByDispatchSendsNothing` is table-driven and pins Dispatch's own
  `checkObjectiveShape` with `"objective":null` and `""` cases.
