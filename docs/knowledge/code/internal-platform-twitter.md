---
type: "Go Package"
title: "internal/platform/twitter"
description: "X (Twitter) Ads v12 client: OAuth 1.0a signing and the campaign -> line_item -> promoted_tweet creation flow."
resource: "internal/platform/twitter"
---

# internal/platform/twitter

Package twitter is the X (Twitter) Ads API v12 platform client. It implements
OAuth 1.0a (HMAC-SHA1) request signing and drives the
campaign -> line_item -> promoted_tweet creation flow. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database.

`CreateCampaign` is only PARTIALLY idempotent: it reuses existing campaigns and
line items by name (paged cursor lookups via `findByName`) before creating new
ones, and a lookup that fails transiently propagates an error so the caller
aborts rather than creating a duplicate. The promoted-tweet association, however,
is always re-POSTed on a repeat call. A recognizable duplicate response
(`DUPLICATE_PROMOTABLE_ENTITY`) is NOT treated as idempotent success: X returns
that code even when the tweet is already promoted by a DIFFERENT line item, so it
is surfaced as a warning (on `PromotedTweetWarning` and in the step log) to be
verified manually rather than assumed to attach to this line item. A
lost/malformed first response likewise produces a warning. True cross-call
idempotency (idempotency keys) is explicitly deferred and tracked in LFXV2-2665. Only the campaign and line item are created with
`entity_status=PAUSED`; the promoted-tweet endpoint does not accept
`entity_status`, so the API creates that association `ACTIVE`. It cannot serve,
though, because the parent line item is paused — delivery is gated by the paused
line item, not by the association's own status.

Per the X Ads v12 contract, create endpoints take their parameters as URL query
parameters (not a JSON body); the client folds those params into the OAuth
signature base string. Flight dates (`start_time`/`end_time`, ISO8601 UTC) are
sent only on the line-item create, where they are required; the campaign
endpoint does not accept them in v12, so the campaign create omits them. Dates
are validated for both shape and real-calendar validity (`time.Parse`) before
any mutating call. The client paces sequential writes within a SINGLE dispatch
toward the 1-req/sec limit; it does NOT enforce the account-wide write limit
across concurrent dispatches or replicas (that needs cross-replica coordination,
tracked in LFXV2-2665), so operators must not rely on this stateless client for
cross-dispatch rate limiting. When the account limit is hit anyway, 429s are
retried with backoff bounded by `Retry-After` / `X-Rate-Limit-Reset`.

See [internal/platform/twitter](../../../internal/platform/twitter).
