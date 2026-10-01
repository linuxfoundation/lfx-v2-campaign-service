# 2026-10-01 A/B test on a time-zone email is refused up front

**Feature** — Ticking "A/B test" on an email cloned from a template that sends "based on recipients'
time zones" produced a single email and no explanation. HubSpot does not allow the two together:
`POST /marketing/v3/emails/ab-test/create-variation` answers HTTP 400 for a `LOCALTIME_EMAIL`. The
send mode is a property of the email and `CloneEmail` copies it, so the template picked as
`hubspotConfig.sourceEmailId` decides it. The variant stage in `Dispatch` is best-effort by design
and `doRequest` discards a non-2xx body, so the only trace was a generic WARN, and the dispatch
itself cannot report it better: it runs after the `202` and every dispatcher error becomes one fixed
job error on purpose.

The check therefore moved to the synchronous create seam. `BriefService.CreateCampaigns` now calls
`Orchestrator.PreflightCreate` before `Orchestrator.Start`. That walks each platform's optional
`CreatePreflighter` (new, `internal/service/orchestrator.go`); the HubSpot dispatcher
(`internal/dispatch/hubspot.go`) implements it by reading the source email's `type` with the new
`hubspot.Client.GetEmailType` and returning `domain.ErrABTestUnsupportedSendType` for
`LOCALTIME_EMAIL`. `mapBriefErr` turns that into a `409` `ConflictError` with
`reason="ab_test_unsupported_send_type"` and fixed, id-free text. Nothing is created for a refused
request.

Only that one sentinel refuses. Everything else — no A/B test requested, no source email, an
unresolvable connection, a failed or timed-out read, a response without a `type` — returns without
blocking: the orchestrator logs it and the create proceeds as before, so the pre-check is never an
availability dependency. A request that does not ask for an A/B test makes no HubSpot call. The
call is bounded at 10 seconds and recorded as the `preflight_create` upstream operation, with a
refusal counted as a successful read.

Not confirmed against a live portal: that the v3 single-email read returns a top-level `type`. The
pre-check logs the observed type on every A/B request and fails open if it is absent.
