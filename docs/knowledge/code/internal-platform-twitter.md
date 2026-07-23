---
type: "Go Package"
title: "internal/platform/twitter"
description: "X (Twitter) Ads v12 client: OAuth 1.0a signing and the campaign -> line_item -> promoted_tweet creation flow."
resource: "internal/platform/twitter"
tags:
  - platform-client
  - twitter
  - x-ads
  - oauth1
  - go-package
timestamp: "2026-07-13T19:18:36Z"
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
aborts rather than creating a duplicate. Reuse is NOT a silent no-op: when a
campaign or line item is reused, the client does NOT re-apply this request's
budget/config/flight-dates to it, so the reused resource may be serving under a
DIFFERENT budget/config or an already-ENABLED line item with different dates. This
is signalled two ways for reconciliation — a warning step in the result, and the
structured `CampaignResult.Reused` flag (set on both the success and partial-result
paths). Consumers that need to know whether the returned campaign matches the
request MUST inspect `Reused` (the dispatch adapter maps a reused result to the
`created_degraded` status, and an authoritative reconcile is the orchestrator's
job, LFXV2-2665). The promoted-tweet association, however,
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
are validated for shape, real-calendar validity (`time.Parse`), ORDER (end after
start), AND for a future start: the start's emitted midnight-UTC instant must be at
least `minStartLead` (5m) ahead of now, so today (or a start only moments ahead) is
rejected before any mutating call — otherwise the multi-request create flow could cross
the start time and X would reject the now-past line-item start, leaving an orphan.
Budget is likewise validated pre-create (positive, ≤ 1e9, rounds to ≥ 1 micro-unit). The client paces sequential writes within a SINGLE dispatch
toward the 1-req/sec limit; it does NOT enforce the account-wide write limit
across concurrent dispatches or replicas (that needs cross-replica coordination,
tracked in LFXV2-2665), so operators must not rely on this stateless client for
cross-dispatch rate limiting. When the account limit is hit anyway, 429s are
retried with backoff bounded by `Retry-After` / `X-Rate-Limit-Reset`. Redirect
following is force-disabled (a shared `noFollow` `CheckRedirect` policy). For a
`WithHTTPClient`-supplied client, `NewClient` builds a FRESH `*http.Client`
carrying the caller's reusable exported fields (`Transport`, `Jar`, `Timeout`) with
`CheckRedirect: noFollow`, rather than value-copying the caller's client (an
`http.Client` must not be copied after first use). So a 3xx is surfaced rather than
followed — important with OAuth 1.0a, where a followed redirect would resend a
request signed for the original URL to a different one.
A non-2xx surfaces a typed `apiError`. Its `Error()` renders only method/path/
status — the raw body is NOT echoed, and neither are X's machine-readable error
codes, so a signed URL / destination secret (which an untrusted body could place
even inside `errors[].code`) can't leak into a persisted Step. The codes are
retained on the struct solely for internal classification via `hasErrorCode`
(e.g. matching `DUPLICATE_PROMOTABLE_ENTITY`), and `parseErrorCodes` bounds what
it keeps (drops over-long values, caps the count). This mirrors the reddit
client, whose `apiError` likewise retains `Body` for classification but never
surfaces it. An ambiguous
transport/read/decode failure surfaces a `transportError`, and a pre-connect dial
failure surfaces a `preSendError`. BOTH render URL-free: `httpClient.Do` returns a
`*url.Error` whose `%v`/`String()` embeds the full request URL (and X puts create
parameters in the query string), so a naive `%w`/`%v` of that error would leak the
URL into the copied `PromotedTweetWarning` and persisted Steps. Each type's
`Error()` runs the cause through `safeTransportCause`, which peels EVERY nested
`*url.Error` layer down to the URL-free underlying cause (timeout/EOF/ECONNREFUSED);
`Unwrap()` retains the real cause so `errors.Is`/`errors.As` (incl.
`isPreSendDialError`) still match. `preSendError` is DEFINITE (request never sent →
not applied), distinct from the ambiguous `transportError`.
`createOutcomeAmbiguous` treats a mutating 3xx/5xx (and transport error) as
UNCONFIRMED so a create that may have committed is not blind-retried into a
duplicate; a `preSendError` is neither, so it stays a definite "not applied".

See [internal/platform/twitter](../../../internal/platform/twitter).
