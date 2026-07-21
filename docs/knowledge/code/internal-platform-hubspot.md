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
drives HubSpot's email surface: marketing-email clone/patch/content-set, CRM
contact-list CRUD, and event-definition lookups. Credentials and account
configuration are injected via `NewClient`; the package never reads environment
variables or touches the database. In production the bearer token comes from a
decrypted stored `hubspot_connections` connection (`private_app_token`).

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
  renders only method/path/status — the response body is retained for internal
  classification but NEVER surfaced (a HubSpot error envelope can quote request
  material). A round-trip failure after the request was plausibly sent, or a 2xx
  whose body can't be read, is a `transportError` (AMBIGUOUS); its `Error()` peels
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
(subject/from/preheader), and `SetSendList`. Creates/clones/PATCHes pass
`idempotent=false` (no idempotency key → a retried 429 could double-create); a 2xx
with no id on a clone/get is surfaced as UNCONFIRMED so the caller verifies rather
than assuming success.

**`SetSendList` routing gotcha (load-bearing):** a HubSpot email's recipient list
goes in `contactIlsLists` when it is an ILS list (any CRM-v3 processingType) and
`contactLists` when legacy — and the two namespaces must NOT both appear in one
PATCH. Putting an ILS list id in `contactLists` (or including the opposite namespace)
makes HubSpot silently reject the ENTIRE `to` object, leaving the email with no
recipients. The client sends a COMPLETE `to` (clearing `contactIds` so no
clone-source contacts leak) with only the send-list's namespace populated, and only
same-namespace suppressions (HubSpot mirrors the exclude to the other namespace).
The ILS-vs-legacy decision is the caller's (from `GetList().ProcessingType`).

## CRM contact-list + event-definition operations (LFXV2-2780)

`lists.go`: `SearchLists`, `GetList` (with `includeFilters=true` so the filterBranch
+ processingType come back — the latter drives the send-list routing above),
`CreateList` (`POST /crm/v3/lists/` — DYNAMIC, contact objectTypeId `0-1`),
`UpdateListFilters` (`PUT …/update-list-filters`), and `ListEventDefinitions` (resolve
`fullyQualifiedName` for BEHAVIORAL_EVENT filters). `filterBranch` is passed through
as OPAQUE JSON — HubSpot's shape invariants (OR-root with AND sub-branches, no nested
ORs, `IN_LIST` not `LIST_MEMBERSHIP` in membership branches) belong to the
audience-builder (LFXV2-2774), not this transport client. A create's 2xx-with-no-id
is UNCONFIRMED. List/get responses are decoded from BOTH the `{"list":{…}}` wrapper
and the bare top-level shape HubSpot variously returns.

## Scope

Auth + request layer + the email/list/event-def operations above. Consumers: the
audience-building logic (LFXV2-2774, uses lists + event-defs) and the email staging
dispatcher (LFXV2-2777, uses the marketing-email ops), the latter blocked on PR #11.
