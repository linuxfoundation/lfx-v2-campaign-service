---
type: "Go Package"
title: "internal/platform/hubspot"
description: "HubSpot API client (email channel): bearer auth, request layer with 429 retry, marketing-email + CRM-list + event-def operations."
resource: "internal/platform/hubspot"
tags:
  - platform-client
  - hubspot
  - email
  - go-package
timestamp: "2026-07-20T00:00:00Z"
---

# internal/platform/hubspot

Package hubspot is the HubSpot API client for the EMAIL channel (LFXV2-2770). It
drives HubSpot's email surface: marketing-email search/get/clone and draft-update
(subject + sender — email content-setting is deferred to LFXV2-2775), CRM
contact-list search/get/create/filter-update (no delete), and event-definition
lookups. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database. In production the bearer token is a field inside the
connection's ENCRYPTED credentials blob (there is no `private_app_token` column on
`hubspot_connections`); the connection layer decrypts it and injects it here.

## Auth (simplest of all the clients)

Unlike the ad-platform clients — Google Ads (OAuth2 refresh→access flow), X (OAuth
1.0a signing) — HubSpot authenticates with a **static private-app bearer token**.
There is no token-exchange endpoint: `doRequest` just attaches
`Authorization: Bearer <token>` from the injected `Credentials.PrivateAppToken`. A
missing token is a definite pre-send error.

## Request layer

`doRequest` applies the repo's standard discipline, mirroring the
googleads/reddit/meta/twitter clients:

- **No-follow redirects** enforced on whatever client is in use — including one
  supplied via `WithHTTPClient` — by rebuilding a FRESH `*http.Client` from the
  caller's reusable exported fields (Transport/Jar/Timeout) + `noFollow`, so the
  caller's client is never mutated. A 3xx is surfaced, not followed.
- **Bounded reads** (`maxResponseBody+1`, 10 MiB).
- **Typed body-free errors:** a non-2xx surfaces an `apiError` whose `Error()`
  renders only method/path/status — the response body is NOT retained at all (the
  request layer drains it for connection reuse but keeps no snapshot). Nothing in this
  client classifies on the body, and retaining it on an exported field could leak a
  HubSpot error envelope's request material via reflection/JSON of the error even though
  `Error()` omits it. A round-trip failure after the request was plausibly sent, or a 2xx
  whose body can't be read, is a `transportError`; it is ambiguous ONLY for a
  MUTATING call (`IsUnconfirmed` returns `transportError.Mutating`) — an idempotent
  read/search that failed in transit landed no mutation and is safely retryable. Its
  `Error()` peels
  every nested `*url.Error` layer (`safeCause`) so the request URL — which can carry
  query secrets — never leaks, while `Unwrap()` keeps the cause for `errors.Is/As`.
  A DNS/connect-time dial failure (`isPreSendDialError`) is a clean pre-send error
  (definitely not sent), rendered URL-free.
- **429 retry gated on an explicit `idempotent` flag:** a rate-limited idempotent
  call (a GET read) is retried up to `retryMax` with a bounded backoff honoring
  `Retry-After` (a server value over `maxRetryWait` aborts rather than sleeping
  pointlessly). A NON-idempotent call (a list/email create/clone) is NOT retried —
  HubSpot creates have no idempotency key, so a 429 whose first attempt may already
  have committed would double-create; it returns the 429 as an `apiError`
  immediately.

## Marketing-email operations (LFXV2-2779)

`email.go` builds on `doRequest`: `SearchEmails`/`GetEmail` (idempotent reads),
`CloneEmail` (`POST /marketing/v3/emails/clone`), `PatchEmailSettings`
(subject + the v3 `from` object's `fromName`/`replyTo`; preview/preheader text is
NOT a first-class v3 field, so it is deliberately not offered — see LFXV2-2775), and
`SetSendList`. Both `PatchEmailSettings` and `SetSendList` PATCH the DRAFT route
(`/marketing/v3/emails/{id}/draft`) — the base `/{id}` route mutates the LIVE email,
so draft edits must go through `/draft`. Creates/clones/PATCHes pass
`idempotent=false` (no idempotency key → a retried 429 could double-create). A clone
with a 2xx-no-id, and a clone/PATCH with an UNDECODABLE 2xx body, are surfaced as
UNCONFIRMED (a PATCH that returns a 2xx with no id just substitutes the requested id —
the update applied, so that is NOT unconfirmed). A mutating 429/3xx/5xx apiError is
flagged `Ambiguous` (see `IsUnconfirmed`), so the caller verifies rather than
blind-retrying. A GET (read) is never UNCONFIRMED — a malformed read is a plain error,
safely retryable.

**`SetSendList` recipients (ILS-only):** a HubSpot email's recipient list goes in
`contactIlsLists` (ILS list ids). HubSpot's ILS migration removed functional support
for the legacy `contactLists` recipient field after 2024-10-31 (it's silently
non-functional now), so this client NEVER emits `contactLists` — callers resolve an
ILS list id from the Lists v3 API. The client sends a COMPLETE `to` (clearing
`contactIds` so no clone-source contacts leak) with `contactIlsLists.include` = the
send list + `.exclude` = suppressions.

## CRM contact-list + event-definition operations (LFXV2-2780)

`lists.go`: `SearchLists` (`POST /crm/v3/lists/search` — constrains to contact lists
SERVER-SIDE via the `objectTypeId "0-1"` request field (a valid `ListSearchRequest`
field per HubSpot's v3 docs), with a per-hit `ObjectTypeID` check kept as
defense-in-depth; follows `offset`/`hasMore` pagination with a repeated-page guard;
`includeFilters` is a GET-single-list field, NOT a search field, and is not sent),
`GetList` (with `includeFilters=true`
so the filterBranch + processingType come back),
`CreateList` (`POST /crm/v3/lists` — canonical no-trailing-slash path, since the client
refuses redirects; DYNAMIC, contact objectTypeId `0-1`),
`UpdateListFilters` (`PUT …/update-list-filters`), and `ListEventDefinitions` (whose
human label is nested under `labels.singular`/`.plural`, not a top-level field, and
which does NOT request `includeProperties` — that payload is discarded). **List size
has TWO shapes** (`List.resolveSize` normalizes both): GET/CREATE
(`PublicObjectList`) carry a top-level integer `size`; SEARCH hits have no top-level
size and instead expose `hs_list_size` as a STRING under `additionalProperties`,
requested explicitly. `ListEventDefinitions` resolves `fullyQualifiedName` for
BEHAVIORAL_EVENT filters.

`filterBranch` is passed through as OPAQUE JSON — HubSpot's shape invariants (OR-root
with AND sub-branches, no nested ORs, `IN_LIST` not `LIST_MEMBERSHIP` in membership
branches) belong to the audience-builder (LFXV2-2774), not this transport client. A
create's 2xx-with-no-id is UNCONFIRMED. List/get responses are decoded from BOTH the
`{"list":{…}}` wrapper and the bare top-level shape HubSpot variously returns.

## Scope

Auth + request layer + the email/list/event-def operations above. Consumers: the
audience-building logic (LFXV2-2774, uses lists + event-defs) and the email staging
dispatcher (LFXV2-2777, uses the marketing-email ops), the latter blocked on PR #11.
