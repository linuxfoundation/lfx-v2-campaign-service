# 2026-10-14 — #208: dispatch failures say why, without quoting the platform

**Fix** — An async create job's failed `PlatformResult.error` was always `"platform campaign
creation failed"`. The real cause went to the log and nowhere else, so a missing Reddit
conversion pixel and a dead OAuth refresh token produced the same polled result. Now
`DispatchFailureMessage` (`internal/service/dispatch_failure.go`) reports:

- service-authored pre-flight refusals **verbatim**. These are `domain.PreflightRefusal`, now
  marked across the Reddit, LinkedIn, Meta, X, Microsoft and HubSpot clients and dispatchers
  (see the `internal/dispatch` concept for the list);
- otherwise a fixed sentence per class: no connection, LF system connection missing or
  unusable, own connection unusable (with its `unusableConnectionReason` token), decrypt
  fault, service defect, token refresh failed (`domain.TokenRefreshFailure`), platform refused
  the create (HTTP status only, `domain.UpstreamStatusCarrier`), or **could not be confirmed**
  for any retained (possibly created) failure;
- the generic string as the last fallback.

No response body, URL, query or token can reach the result. Claim release/retention, the
retained row's status and the job aggregate are unchanged. Only the message changed.
`design/brief.go`'s `platform-result.error` description is regenerated, and
`docs/api-catalog.md` documents the classes. The api-catalog, design and bootstrap paragraphs
that called the reason token "log-only" were updated, and
`TestOrchestrator_PreCreateFailureLogsAClassifiedReason`, whose trip-wire required exactly
that, now asserts that the token appears and the error text does not. Google Ads clients are
not marked (owner: Shiv), so for Google only the shared classes apply.
