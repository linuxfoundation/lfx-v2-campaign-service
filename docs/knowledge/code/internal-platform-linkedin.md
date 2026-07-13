---
type: "Go Package"
title: "internal/platform/linkedin"
description: "LinkedIn Marketing API client: OAuth2 dark-post campaigns (Campaign Group -> Campaign -> Dark Post -> Creative) with targeting and up-front validation."
resource: "internal/platform/linkedin"
tags:
  - platform-client
  - linkedin
  - linkedin-ads
  - oauth2
  - go-package
timestamp: "2026-07-13T19:22:00Z"
---

# internal/platform/linkedin

Package linkedin provides a Go client for the LinkedIn Marketing API, ported
from the upstream TypeScript `linkedin-ads.service.ts` client. Credentials and
the full runtime config are injected via `NewClient`; the client never reads the
process environment or files.

Authentication is a Bearer access token; every request also sends the
`LinkedIn-Version` and `X-RestLi-Protocol-Version` headers required by the
Marketing API. `CreateCampaign` builds the full sponsored-content hierarchy in
one call — Campaign Group (ACTIVE) -> Campaign (PAUSED) -> Dark Post
(`feedDistribution: NONE`) -> Creative — with targeting assembled from the
runtime config's profile (skills/groups/job-functions) and resolved geo URNs.
Cross-tenant org/account pairing fails closed.

A deliberate divergence from the TS source is that geo resolution is a pure,
cache-only function (no network fallback). Beyond that, the Go port validates
strictly and fails BEFORE any permanent resource is created: budget minimums and
sub-cent/NaN/Inf; registration URL (absolute, http/https, real host); schedule
(malformed/past/reversed); event name and project (trimmed, length-bounded);
targeting facet URNs (numeric ids in the correct namespace); ad-account and org
ids (numeric); geo URNs; and the aliased `cloud-native` profile must exist for
`custom`. Find-or-create uses cursor pagination and propagates transient search errors
(rather than treating them as "not found") to reduce duplicates, but it is
best-effort and NOT atomic across calls: `CreateCampaign` re-POSTs every dark
post and creative on a repeat call, so this package does not itself guarantee
cross-call idempotency. True single-flight/idempotency is a planned caller-side
responsibility (the orchestrator's per-(brief, platform) claim), tracked
separately and not provided here. A 429 (idempotent methods only) is retried
with bounded backoff.

See [internal/platform/linkedin](../../../internal/platform/linkedin).
