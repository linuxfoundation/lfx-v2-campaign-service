# 2026-10-01 The create pre-check no longer records calls it never made

**Fix** — `Orchestrator.PreflightCreate` (added in `38a9ef1f`) recorded an upstream call under the
operation `preflight_create` for every platform whose dispatcher implements `CreatePreflighter`,
including when the dispatcher returned before contacting the platform. HubSpot does exactly
that: `HubSpotDispatcher.PreflightCreate` returns `nil` straight away unless the request asks for
an A/B test on a non-blank source email, and it also returns `nil` for a config it cannot parse.
Each such create added a near-zero `outcome="ok"` sample to `campaign_upstream_calls_total` and
`campaign_upstream_call_duration_seconds`, while the lookups that really reach HubSpot happen only
on A/B creates. The local no-ops therefore shared the operation with those lookups, pulling its
latency quantiles toward zero and diluting its error rate in proportion to how many creates did
not ask for an A/B test, so a slow or failing HubSpot email-type endpoint could hide behind
healthy-looking numbers. It also broke the stated contract of `recordUpstream`, that it measures
"actual network work rather than local refusals". A code review of the commit found it.

**The change.** A new sentinel, `domain.ErrPreflightNotApplicable` (re-exported by
`internal/service` like its siblings), means "I looked at the request, had nothing to check, and
made no platform call". `HubSpotDispatcher.PreflightCreate` returns it on the two no-call paths
(a config that does not parse, and A/B off or no source email id). `Orchestrator.PreflightCreate`
skips such a platform before recording anything: no error, no WARN, no `recordUpstream`. `nil` keeps
its meaning, "a check was made and found nothing wrong", and is still recorded, as are a refusal
(as a successful read) and an ordinary failure (as an error). The sentinel is internal to the
pre-check: it is never mapped to a status and never reaches a caller, and the 409 contract of
`38a9ef1f` is unchanged. A skip does not end the walk, so a later platform can still refuse.

The `CreatePreflighter` contract comment, the "A/B test pre-check" section of
[internal-dispatch](../code/internal-dispatch.md) and the "Create pre-check" section of
[internal-service](../code/internal-service.md) describe the second sentinel.

**Tests.** `TestOrchestrator_PreflightCreate_NotApplicableIsSkippedNotRecorded` asserts the
dispatcher is still asked, nothing is recorded, a wrapped sentinel is treated the same, and a
later platform's refusal is not masked. `TestHubSpot_PreflightCreateMakesNoCallUnlessAnABTestWasRequested`
now asserts the sentinel instead of `nil` for the eight no-call shapes. The existing
`TestUpstreamCallsAreInstrumented` case still covers a check that ran and was recorded.

**Not verified.** The effect on the real histogram was not observed against a running metrics
backend; it is established by the orchestrator tests reading the recorder. The pre-existing open
question from `38a9ef1f` stands: that the v3 single-email read returns a top-level `type` on a
live portal.
