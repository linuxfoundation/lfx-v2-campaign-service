# 2026-11-25 — A HubSpot draft deleted upstream can be staged again

**Fix** — an email brief could stage exactly one HubSpot draft, ever. A HubSpot slot takes no new
version (`model.ProviderSupportsSlotVersions`), so `dispatchPlatform` reused the completed row for
every later create; once someone deleted that draft in HubSpot, every Stage answered `reused` with
an id HubSpot no longer knows. Found in an end-to-end run against the LF portal: a draft staged on
2026-10-22, since deleted, came back from Stage as the result, and its link opened "Email not found".

- `service.UpstreamPresenceChecker`, an optional dispatcher capability, says whether the object a
  completed row points at is gone upstream. `HubSpotDispatcher.UpstreamGone` reads the email, in
  the portal it was created in (the `ReadMetrics` provenance guard), and answers "gone" only on a
  definite 404.
- Before the fast-path reuse, `Orchestrator.retireIfGoneUpstream` asks it, for platforms without
  slot versions only. On "gone" it soft-deletes the row through `CampaignRepository.DeleteCampaign`
  (the delete-campaign endpoint's write: the upstream id stays for the audit trail, and a `deleted`
  index message co-commits) and claims the same slot version afresh, so the brief stages a new draft.
- Every doubt keeps the reuse, as before: no capability, a failed or inconclusive check, or a
  retirement that fails. A duplicate draft is the failure the reuse exists to prevent.

Concepts updated: `code/internal-service.md`, `code/internal-dispatch.md`. Tests:
`internal/service/orchestrator_upstream_gone_test.go`, `internal/dispatch/hubspot_upstream_gone_test.go`.
