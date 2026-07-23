# Log

## 2026-07-21

**Update** — HubSpot deep-review pass (PR #35). Ran a 5-dimension parallel review
(context/concurrency, error-classification, test-completeness, API-contract/docs, security)
with adversarial verification of each finding. One REAL bug + polish:
(1) SECURITY: `transportError` and `preSendError` had EXPORTED `Err error` fields holding a
`*url.Error` whose exported `.URL` carries the full request URL incl. `?after=<cursor>`.
`Error()` strips it via safeCause, but JSON/reflection serialization of the error (a
structured logger, error middleware) walks the exported field and leaks the cursor — the
exact vector the package already eliminated for `apiError` (no Body field). Unexported both
to `err` (like `unconfirmedError`); `Unwrap()`/`errors.Is/As` and the URL-free `Error()`
are unaffected. Added json.Marshal leak-regression assertions to both leak tests.
(2) Test coverage: mutating-3xx-is-UNCONFIRMED, connection-refused-is-preSend-NOT-unconfirmed
(dial+ECONNREFUSED → definite pre-send), ListEventDefinitions malformed-body + stuck-cursor
guards. (3) Fixed two stale test comments (`properties`→`includedProperties`,
`nextPageToken`→paging `after` cursor).

**Update** — HubSpot ctx-cancel pre-send guard (PR #35 review, copilot — a REAL
correctness bug, not a nit). `doRequest` fired `httpClient.Do(req)` even when the caller's
context was ALREADY done before send. A ctx cancellation isn't an `isPreSendDialError`, so
the resulting `Do` error fell through to `transportError{Mutating: !idempotent}` →
`IsUnconfirmed == true` — wrongly telling a caller of a MUTATING request that the mutation
MIGHT have committed (verify-before-retry) when nothing was ever sent. Added a `ctx.Err()`
guard right before `Do` (inside the retry loop, so it also covers a ctx that expires during
a 429 backoff), returning a clean `preSendError` (definitely-not-sent → `IsUnconfirmed ==
false`). Mirrors the established ctx.Err() pre-send guard in the googleads/reddit/meta
clients. Added `TestDoRequest_AlreadyCancelledCtxIsPreSendNotUnconfirmed` (asserts
preSendError, NOT unconfirmed, wraps context.Canceled, and the server is never hit).

**Update** — HubSpot FINAL review pass (PR #35). Ran an exhaustive self-review of the
whole hubspot diff (correctness, test-strength, doc-drift, API-contract, security) and
closed the last three things an automated reviewer could flag — the code was already
functionally correct: (1) `TestDoRequest_Mutating429IsNotRetried` now asserts
`IsUnconfirmed(err)` (a mutating 429 is ambiguous/may-have-committed), closing the pending
Copilot suggestion and catching an `Ambiguous`-flag regression. (2) Fixed the
`DefaultBaseURL` const comment that still showed `/crm/v3/lists/` with a trailing slash.
(3) Added clarifying comments on GetEmail/CloneEmail explaining the value-decode already
covers a null body via the `e.ID == ""` check (patchEmail uses the *Email-pointer pattern;
these don't need to). Branch frozen at this head for merge.

**Update** — HubSpot round-22 (PR #35 review, copilot). (1) `patchEmail`: a 2xx JSON
`null` (or empty) body decodes into the Email struct WITHOUT error (zero-valued), so the
id-fallback would report a PHANTOM success for a malformed response. A PATCH is mutating,
so a null/empty body is now UNCONFIRMED (the update may have applied). Added
`TestPatchEmail_NullBodyIsUnconfirmed`. (2) Doc: corrected the round-20 log entry that
still said `properties=…` (the code uses repeated `includedProperties`), and added a
missing blank line before a `**Creation**` heading that was folding two log entries.

**Update** — HubSpot round-21 (PR #35 review, cursor). (1) Switched the field restriction
from a CRM-style comma-separated `properties` string to REPEATED `includedProperties`
entries — the marketing-emails LIST endpoint uses that shape, not the CRM `properties`
convention. (2) The malformed-2xx guard (Results==nil) now fires on ANY page, not just
page 0: on a later page a missing results array would otherwise silently TRUNCATE the walk
and return a partial. Applied to SearchEmails + ListEventDefinitions. (3) Added the same
guard to SearchLists (Lists==nil → error; an empty search returns `{"lists":[]}`, non-nil).
Tests added for the SearchLists malformed case. (Also merged main to pick up #33.)

**Update** — HubSpot round-20 batch (PR #35 review, copilot + dealako). (1) RESTORED
`sort=-updatedAt` on SearchEmails — verified against HubSpot's v3 docs that `sort` IS a
valid GET /marketing/v3/emails param (round 13 wrongly dropped it; another bot flip-flop,
like objectTypeId). Client-side parsed-instant sort stays as the guarantee. (2) Added a
repeated `includedProperties` values for name, subject, and updatedAt — the list
endpoint returns FULL email content by default, so at limit=100 rich templates could blow
the response cap. (3) SearchEmails
and ListEventDefinitions now ERROR on a malformed 2xx (`{}`/`null` → Results==nil, no
paging) instead of returning a clean empty success that hides a broken response (an empty
portal returns `{"results":[]}`, non-nil, still returns 0). (4) Removed the dead
`cloneEmailRequest.Language` field (never populated; omitting language IS the
preserve-source-locale behavior). (5) Doc fixes: `private_app_token` is NOT a
`hubspot_connections` column (the token lives inside the encrypted creds blob) — corrected
client.go + internal-platform-hubspot.md; fixed a stale `/crm/v3/lists/` createListRequest
comment. (6) Tests: sort/properties assertions, malformed-body, and the id-tiebreak.

**Update** — HubSpot error-path query-strip + comment/test cleanup (PR #35 review round 18,
copilot). (1) `doRequest` now strips the query string from `path` before it reaches any
error (the full path is already in `u` for the URL) — a paginated request carries
`?after=<cursor>`, and the cursor (or any future query secret) must not leak through the
URL-free error contract. Added `TestDoRequest_ErrorPathStripsQueryString`. (2) Corrected
the stale `Email.UpdatedAt` field comment (it still claimed lexical sorting; the code
parses). (3) `TestSearchEmails_SortsByParsedInstantNotLexical` checks `len(got)` before
indexing so a pagination/filter regression reports the failure instead of panicking.

**Update** — HubSpot sort-by-instant + drop error-body snapshot (PR #35 review round 15,
copilot). (1) `sortEmailsByUpdatedDesc` now PARSES `updatedAt` as an RFC3339 instant
before comparing — a raw lexical compare mis-orders equivalent instants with different
offsets/fractional seconds (`2026-01-01T00:30:00+01:00` is OLDER than
`2026-01-01T00:00:00Z` but sorts lexically after). Missing/malformed → zero time (sorts
last); id tiebreak. Added `TestSearchEmails_SortsByParsedInstantNotLexical`. (2) Removed
`apiError.Body` + `readErrorSnapshot` entirely: nothing in this package classifies on the
body, and an EXPORTED Body field could leak upstream request material via reflection/JSON
serialization of the error even though `Error()` omits it. Non-2xx responses are now just
drained for connection reuse (googleads keeps a snapshot only because it parses error
codes from it).

**Update** — HubSpot remove dead Label field + doc fix (PR #35 review round 14, copilot).
(1) Removed the unused `AccountConfig.Label` — it was documented as "surfaced on results"
but this client's operations return raw Email/List objects with no result envelope to
carry it, and nothing read `c.account.Label` (the meta/reddit clients DO surface it on
their campaign-result types; hubspot has none yet). A no-op config field misleads callers,
so it's gone until there's a result type that reads it. (2) Doc: `CreateList` endpoint in
internal-platform-hubspot.md now shows the canonical no-trailing-slash `/crm/v3/lists`
(matching the round-13 code fix).

**Update** — HubSpot endpoint-contract fixes (PR #35 review round 13, copilot).
(1) `SearchEmails` — [CORRECTED in round 20: `sort` IS a valid GET /marketing/v3/emails
param per HubSpot's docs; this round wrongly dropped it. Round 20 restored `sort=-updatedAt`
as a server hint.] The most-recently-updated-first order is guaranteed CLIENT-SIDE
regardless via `Email.UpdatedAt` + `sortEmailsByUpdatedDesc` (parsed-instant compare, id
tiebreak) applied to the aggregated matches. (2) `CreateList` now
POSTs to the canonical `/crm/v3/lists` (no trailing slash) — HubSpot canonicalizes a
trailing slash via redirect and this client refuses redirects, so `/crm/v3/lists/` could
have produced a failed/ambiguous create. Updated the test path assertion.

**Update** — HubSpot cursor decode preserves `+` (PR #35 review round 12, cursor/copilot).
The round-10 `decodeCursor` used `url.QueryUnescape`, which converts a literal `+` to a
space — but base64 paging cursors legitimately contain `+`, so a token like `A+B/C=`
would be sent as `A B/C=` and break pagination. Switched to `url.PathUnescape`, which
decodes `%XX` while preserving `+`. Added `TestSearchEmails_PreservesPlusInCursor`. Also
fixed a stale `List.ObjectTypeID` field comment that still carried the (wrong) round-6
"objectTypeId is response-only" claim.

**Update** — HubSpot constructor input normalization (PR #35 review round 11, copilot).
`NewClient` now trims the injected `PrivateAppToken` and `PortalID` (mirrors meta/twitter):
a whitespace-only token is treated as missing (rather than sent as `Bearer   `), and a
padded portal id can't build invalid app URLs. Added `TestNewClient_NormalizesTokenAndPortalID`.

**Update** — HubSpot cursor decode + dedup clarity (PR #35 review round 10, copilot/cursor).
(1) HubSpot returns `paging.next.after` already percent-encoded (e.g. `MjA%3D`); feeding
it straight back through `url.Values.Encode` double-encoded the `%` (→ `MjA%253D`),
corrupting page-2 of `SearchEmails`/`ListEventDefinitions`. Added `decodeCursor`
(QueryUnescape-once, unchanged on non-encoded tokens, falls back to the raw token on
error) and use it in both cursor paginators. Added `TestSearchEmails_DecodesEncodedCursor`.
(2) Clarified `SearchLists` loop-detection: `seen`/`newThisPage` intentionally track the
RAW server rows independently of the contact filter (progress ≠ what we keep). (3) Fixed a
dangling doc-comment in `client_test.go`.

**Update** — HubSpot defensive filter tolerates omitted objectTypeId (PR #35 review
round 9, cursor). The round-8 client-side check dropped any hit whose `ObjectTypeID` !=
"0-1" — but a HubSpot response can OMIT `objectTypeId`, leaving it empty, which would
drop valid contact lists (the server-side filter already guaranteed they're contacts).
Now the defensive check drops a hit only if its type is EXPLICITLY non-contact (`ot != ""
&& ot != "0-1"`); an empty/omitted type is trusted. Test updated with an omitted-type
fixture row that must be kept.

**Update** — HubSpot contact-list filter RESTORED server-side (PR #35 review round 8,
copilot — REVERSES round 6). VERIFIED against HubSpot's official v3 docs:
`objectTypeId` IS a valid `ListSearchRequest` body field — the docs give the exact
example `{"query":"HubSpot","processingTypes":["MANUAL"],"objectTypeId":"0-1"}`. Round 6
had claimed the opposite (that it's a response-only property) and the server-side filter
was dropped in favor of client-side only; that was based on a wrong API claim from a
self-contradicting bot review. `SearchLists` now sends `objectTypeId: "0-1"` in the
request again (server-side filter), KEEPING the per-hit `ObjectTypeID` check as
defense-in-depth, and restored the body assertion (`objectTypeId == "0-1"`). The
`TestSearchLists_FiltersToContactListsClientSide` defensive test stays. NOTE for future
reviewers: this is settled against the HubSpot docs — do not remove the server-side
`objectTypeId` again.

**Update** — HubSpot dedup + cap coverage (PR #35 review round 7, cursor/copilot).
`SearchLists` (offset paginator) now tracks seen list ids and errors when a non-empty
page adds no NEW ids (server repeating a page), matching the cursor paginators'
stuck-cursor guard — previously it could return duplicate rows. Added a boundary test
for the 10 MiB response cap: a body AT the limit succeeds, limit+1 is a `transportError`,
and an over-cap MUTATING call stays `IsUnconfirmed`.

**Update** — HubSpot contact-list filtering (PR #35 review round 6, copilot). [SUPERSEDED
by round 8 — see the top entry.] This round removed the server-side `objectTypeId` filter
on a bot claim that it wasn't a valid `ListSearchRequest` field. That claim was WRONG
(HubSpot's docs document the field), so round 8 restored the server-side filter. What
survives from this round: `ObjectTypeID` was added to the `List` struct and a per-hit
client-side check + `TestSearchLists_FiltersToContactListsClientSide` were added — both
KEPT as defense-in-depth alongside the server-side filter.

**Update** — HubSpot input-normalization (PR #35 review round 4, cursor).
`SearchEmails`/`SearchLists` trim the query before matching/forwarding (a padded term
no longer silently returns no results), and `CloneEmail` trims `cloneName` and rejects
an empty-after-trim name (consistent with `CreateList`), so a padded name can't produce
a misnamed draft.

**Update** — HubSpot paginator hardening (PR #35 review round 3, cursor).
`SearchEmails` and `ListEventDefinitions` now error on a non-advancing cursor (a
repeated `paging.next.after` token) instead of re-fetching the same page until the cap
and duplicating results — matching the offset guard `SearchLists` already had.
`CreateList` trims its name before posting (padding no longer becomes part of the list
name).

**Update** — HubSpot client hardening (PR #35 review round 2, copilot/cursor).
(1) All id entry points trim-and-reassign before use (`GetEmail`,
`PatchEmailSettings`, `SetSendList`, `CloneEmail`, `GetList`, `UpdateListFilters`) —
a whitespace-padded id sent raw yields a 404/rejection that silently fails staging.
(2) `SearchLists` now errors on an empty page while `hasMore=true` instead of
returning a silent partial (a truncated audience list under-targets); the cap-exceeded
paths deliberately keep returning an error (all-or-error contract, never a silent
partial). (3) Corrected the `transportError` doc: it is ambiguous ONLY for a MUTATING
call (`IsUnconfirmed` returns `transportError.Mutating`); an idempotent read/search
that failed in transit is safely retryable.

**Update** — HubSpot client v3-contract fixes (PR #35 review, copilot; verified
against HubSpot's OpenAPI specs). (1) `PatchEmailSettings`/`SetSendList` now PATCH the
DRAFT route `/marketing/v3/emails/{id}/draft` — the base `/{id}` route mutates the
LIVE email, so draft edits were hitting the wrong endpoint. (2) `SetSendList` is now
ILS-ONLY: HubSpot's ILS migration removed functional support for the legacy
`contactLists` recipient field after 2024-10-31, so the client never emits it (dropped
the `isILS` param + the legacy numeric-id handling; callers resolve an ILS list id from
the Lists API). (3) `SearchLists` constrains results to
contact lists via the `objectTypeId "0-1"` request field (a valid `ListSearchRequest`
field — see the round-8 entry; a round-6 detour briefly moved this client-side before it
was restored server-side). It also drops the invalid
`includeFilters` search-body field, and reads
membership size from `hs_list_size` (a STRING under `additionalProperties`, requested
explicitly) — there is no top-level `size`, so `List.Size` was always 0. (4) A mutating
429/3xx/5xx `apiError` is now flagged `Ambiguous`; new `IsUnconfirmed(err)` lets callers
distinguish a may-have-committed outcome from a definite 4xx. (5) 429/error response
bodies are drained (bounded) before close so the keep-alive connection is reused on
retry. (6) Added multi-page pagination tests (cursor + offset forwarding, aggregation,
termination) for all three list-walkers.

**Update** — GA budget-name reconcile guidance qualified (PR #33 review, copilot). The
`campaignNamePartial` comment + `internal-platform-googleads.md` claimed the budget and
campaign names always DIFFER, so `CampaignBudgetName` is the budget reconcile key. That's
true only PRE-attachment: a non-shared (`explicitlyShared=false`) budget's name
SYNCHRONIZES to the campaign name once the campaign attaches, so at a campaign-stage
ambiguous failure the budget's current name is unknown (may be `campaignName`). The code
already handles this — the budget-stage partial (`budgetPartial`) carries
`CampaignBudgetID`, so past attachment reconciliation is by ID, not name — this just
corrects the comment/doc to say so (no behavior change).

**Update** — GA error-body snapshot no longer pins the full response (PR #33 review,
copilot). `doRequest` built `apiError.Body` as `string(raw)[:maxErrorBodyChars]` — the
400-char substring shared the up-to-`maxResponseBytes` backing array, so every retained
apiError pinned the whole body. Now the raw BYTES are sliced to the cap first and only
the bounded slice is converted to string (a fresh allocation), so the snapshot retains at
most `maxErrorBodyChars`. Error-code parsing still runs against the FULL raw body first,
so duplicate/field-error classification is unchanged. Added
`TestDoRequest_ErrorBodySnapshotIsBounded`.

**Update** — GA CampaignInput gains EventSlug (PR #33 review, dealako). Added a plumbed
`EventSlug` field to `googleads.CampaignInput` for struct parity with the meta/twitter/
reddit clients (which build UTM click-through params from it). GA's CreateCampaign builds
only a PAUSED shell today (no ad/final URL), so the field is accepted but not yet
consumed; GA-3+ ad creation will use it. Reserved now so the platform-agnostic input
shape stays stable.
**Update** — Made the create-brief + create-campaigns `project_id` SLUG-ONLY in the published Goa contract (PR #36 review, copilot). The handlers already reject a UUID at runtime (validateProjectSlug), but both create methods still declared `projectIDAttr` ("UUID or slug"), so generated/OpenAPI clients accepted UUIDs the handlers then 400'd. Added `projectSlugAttr()` (Pattern `^[a-z0-9]+(-[a-z0-9]+)*$` + MaxLength(35)) to those two methods and regenerated the API; read/update/delete stay `projectIDAttr` (UUID-or-slug; migration 000003 preserved historical UUID rows). Also tightened `projectSlugRe` to reject consecutive hyphens (`foo--bar`) so it matches the "single internal hyphens" contract; added foo--bar/cncf- to the rejection test. **Update** (same PR, later review): extended the SAME slug-only contract to ALL SEVEN connection-CREATE endpoints (`create-{provider}` via `connectionMethods`) — a connection is stored keyed by `project_id`, the exact-match key for the dispatch lookup, so a UUID-keyed connection could never join a dispatched campaign. `validateConnectionProjectSlug` guards each `Create*` service method (connections-flavored 400); the generated decoder validates the pattern too; get/update/delete/set-credential/test stay permissive for historical UUID rows. Compatibility-impacting: a UUID connection-create payload now 400s where it previously succeeded.

**Update** — PR #40 review (round 11): two fixes. (1) Archived-brief lifecycle
inconsistency (cursor): `ListAudiences` 404s on an archived parent brief, but
`GetAudience`/`UpdateAudience` only matched the audience row and never re-checked the
brief was active — so after archiving, list failed while get/patch still succeeded on
the same nested resource. Added an `EXISTS(active brief)` predicate to `GetAudience`'s
query (Update loads via Get, so the patch path is covered too), consistent with List +
Create. (2) Doc drift: `internal-infrastructure-postgres.md` still showed the old
`btrim(...) <> ''` 000006 constraint; updated it to the `~ '[^[:space:]]'` expression.

**Update** — PR #40 review (copilot, round 10, after David's approval): two fixes.
(1) UpdateAudience checked If-Match only via the repo's atomic write, AFTER the merge +
built-invariant Validate() — so a patch valid against the client's fetched version but
content-invalid once merged onto a NEWER stored version returned 400 instead of 412
(stale ETag). Added an explicit `cur.Version != version → 412` check right after
GetAudience (before merge/validate); the repo's atomic check still catches a read→write
race. Added a regression test (`TestAudienceService_Update_StaleIfMatchIs412NotContent400`).
(2) The built-invariant CHECK (000006) used `btrim(x) <> ''`, which strips only ordinary
spaces — a tab/newline-only master-list id passed the DB CHECK but `Validate()`
(strings.TrimSpace) rejects it. Switched to `platform_master_list_id ~ '[^[:space:]]'`
(requires a non-whitespace char), matching the app.

**Update** — PR #40 review (copilot, round 9): two fixes. (1) Cross-tenant integrity gap:
`campaign_audiences.brief_id` referenced only `campaign_briefs(id)`, so the copied
`project_id` was unchecked — a worker/backfill/direct write could persist an audience
whose `project_id` differed from its brief's, and `GetAudience` (trusts the stored
`project_id` for tenant scoping) could expose it under the wrong tenant. Added migration
000007: a composite FK `(brief_id, project_id) → campaign_briefs(id, project_id)` (plus
the `UNIQUE (id, project_id)` on campaign_briefs the composite FK requires). The API
create path already guarded this via `INSERT … WHERE EXISTS` an active project-scoped
brief; the FK makes the DB the source of truth for all writers. (2) Doc drift: updated
`cmd-campaign-service.md` to say `buildMux` mounts health/campaign, connection, brief,
AND audience servers (it said only health + connection).

**Update** — PR #40 human review (David CHANGES_REQUESTED + Rashad). Fixed the one
blocking defect: `CreateAudience` stored `created_by` as the JSONB literal `null` for an
unattributed row — `actorFromCtx` returns a typed-nil `*model.Actor` that slips past
`marshalAny(any)`'s `v == nil` guard (a typed nil boxed in an interface is not `== nil`)
and JSON-marshals to `"null"`. Added a `marshalActor(*model.Actor)` helper that checks
the concrete pointer, so no actor → SQL NULL. Also (agreeing with both reviewers) added a
DB CHECK `campaign_audiences_platform_valid` (`platform IN ('hubspot')`) to migration
000006 so the platform enum is datastore-enforced like `status`, not only at request
time. Clarified `audienceFromInput` status handling to an explicit if/else (behaviorally
identical — `StatusOrDefault()` was already a no-op when set — but a reviewer misread the
unconditional call as an overwrite; the false positive is now un-misreadable). Dropped
the dead `id` parameter from `audienceFromInput`. Added tests: nil-actor→NULL created_by,
and explicit-status-preserved-on-create.

**Update** — PR #40 follow-up review: two fixes. (1) The "explicit empty list clears
suppressions" contract couldn't round-trip: `suppression_list_ids` is an optional array,
so the generated client encodes it `json:"...,omitempty"` and a non-nil `[]string{}` is
dropped on the wire — the clear silently didn't work. Replaced the empty-slice signal with
an explicit `clear_suppression_lists` boolean in `AudienceUpdateInput` (always encodes;
takes precedence over a supplied list), regenerated `gen/`, updated `applyAudiencePatch`/
`hasAudiencePatch`, and added a service test for replace/clear/precedence. (2) `mapAudienceErr`
mapped `ErrNotFound` → "the audience was not found", but on create/list that error comes
from a missing/cross-project/archived PARENT BRIEF — made the shared message
resource-neutral ("the audience or its parent brief was not found").

**Update** — Route + authz for campaign_audiences (LFXV2-2783). Verified the audiences
endpoints need NO new gateway wiring: they nest under `/briefs/{briefId}/audiences`, so
the HTTPRoute `briefs(/.*)?` regex already forwards them and the single Heimdall
`project-api` rule (`/projects/:projectId/briefs/**`) already authorizes them on
`campaign_manager` (confirmed by running the RE2 regex against real audiences paths).
Added explicit audiences rows to the route/rule PARITY test (parity_test.go accepted
table) so a future narrowing of the briefs match/rule can't silently unroute or
de-authorize them, and documented the inheritance in api-catalog.md. No chart change.

**Update** — PR #40 follow-up review: two fixes. (1) `AudienceRepo.UpdateAudience` did
`UPDATE` then a SEPARATE `GetAudience` re-read to return the row — a race where a
concurrent version N+1 could land between the two statements and hand the first caller
the other writer's row + ETag. Switched to `UPDATE … RETURNING audienceCols` scanned
atomically, so the caller always gets the state its OWN write produced; the re-read
survives only on the no-row path to classify 404 vs 412 (it never becomes the returned
row, so it can't race). (2) Tightened the migration-000006 CHECK to reject blank/
whitespace master-list ids (`btrim(...) <> ''`), not just NULL — via the API empties are
written as NULL, but a direct/build-worker write could persist `''`, and the DB is meant
to be the source of truth for all writers.

**Update** — PR #40 review: updated `internal-container.md` to include the audiences
service in the no-DB and cold-start-503/late-binding mode enumerations (it was still
listing only connection + brief). The container wires `AudienceService` in all four
paths and late-binds it via `AudienceService.SetBackend` (same RWMutex/`ready()` pattern
as the brief service), so the OKF concept now matches the container behavior.

**Update** — PR #40 follow-up review: enforce the built-audience invariant. `AudienceBuilt`
is DEFINED as "the platform master list exists", but `status:"built"` was accepted with no
`platform_master_list_id` — persisting a row that claims a list its pointer is NULL. Added
`CampaignAudience.Validate()` (built ⇒ non-empty master-list id, evaluated on the EFFECTIVE
status) and call it before persisting on BOTH create AND update-after-merge, so no path (a
create with built+no-id, a status-only patch to built on an id-less row, or clearing the id
on an already-built row) can leave "built" meaning nothing — each is now a 400. Model +
service tests cover all three. Backed the app-level 400 with a DB CHECK constraint
(migration 000006: `status <> 'built' OR platform_master_list_id IS NOT NULL`) so the
platform build worker and direct writes can't violate it either — the datastore is the
source of truth, the API 400 a friendly early reject. (Reviewer-sim follow-ups: fixed a
godoc regression where `audienceValidationErr`'s doc comment detached `mapAudienceErr`'s;
documented the deliberate content-400-before-concurrency-412 precedence in UpdateAudience.)

**Update** — PR #40 follow-up review (two rounds): fixed the campaign_audiences PATCH
contract. (1) The update method reused `AudienceInput`, where `platform` is Required —
so the generated validator rejected a status-only/suppression-only patch unless the
caller also resent the immutable `platform`, defeating the "only supplied fields change"
contract. Added a dedicated `AudienceUpdateInput` (all mutable fields optional, no
`platform`), pointed `update-audience` at it, regenerated `gen/`, retyped
`applyAudiencePatch`. (2) But then every field being optional meant `{"audience":{}}`
passed the validator as a no-op that still bumps version/updated_at → invalidates other
clients' ETags → spurious 412s. Added a service-level `hasAudiencePatch` guard rejecting
an all-omitted patch as a 400 (with a test asserting the version is NOT bumped). Updated
the service tests to send platform-free patches and fixed the `AudienceInput` doc comment
(it is the CREATE payload; updates use `AudienceUpdateInput`). design.md notes the split.

**Update** — PR #40 review: extended the container startup tests to cover the new
audiences service (typed-503 in both no-DB and cold-start-503 modes + successful
`SetBackend` late-binding), and updated the architecture index for accuracy —
`design.md` now says four services and describes the audiences service, and
`api-catalog.md` gained a Campaign Audiences section listing the four nested routes.

**Creation** — Added the campaign_audiences Goa API (LFXV2-2782, epic LFXV2-2770) on
top of the existing DB layer (migration 000005 + model.CampaignAudience +
AudienceRepository + repo). `design/audience.go` defines the audiences service
(create/get/list/update) nested under a brief
(`/projects/{project_id}/briefs/{brief_id}/audiences[/{audience_id}]`), reusing the
shared design helpers (bearerToken/projectIDAttr/briefIDAttr/ifMatchAttr, JWTAuth,
the standard error set). Regenerated gen/ via goa. `internal/service/audience.go`
implements the handlers: maps payloads ↔ model, optimistic-concurrency update gated on
If-Match (same strong-validator parsing as briefs), ETag = version, typed error
mapping, and RWMutex `SetBackend` late-binding + typed-503 mode mirroring the brief
service. Wired into the container (no-db / 503-boot / live / cold-start-retry paths)
and mounted in the server (`buildMux` + a route-mount test asserting
`GET …/audiences` resolves non-404 + a nil-endpoints fail-loud case). Service-layer
tests cover create/defaults/If-Match(428/412/success)/404/late-binding. Full gate green.
**Update** — Made `page_id` + `account_id` REQUIRED and format-VALIDATED in
`MetaAdsConnectionConfig` (design/connection.go, PR #38 review, consolidated over three
rounds): an active Meta connection with an unusable id would always fail dispatch (the
Meta client rejects any `account_id` not matching `act_<digits>` and any non-numeric
`page_id` before a mutating call), so beyond `Required` we validate `page_id` with a
digits-only `Pattern` and `account_id` with `Pattern(^act_[0-9]+$)` — `Required`/
`MinLength(1)` alone would let `{"page_id":""}` or `account_id:"foo"` through. This
surfaces the error as a 4xx at connection creation instead of a silent runtime failure.
Added table-driven API-level tests exercising the GENERATED request-body validators
(missing/empty/non-numeric page_id and non-`act_` account_id rejected on both create and
update; valid numeric ids pass) — in a NON-generated package `internal/apivalidation`
that imports the exported validators, NOT under `gen/` (DO-NOT-EDIT boundary). Also fixed
a vacuous placement assertion in the meta dispatch happy-path test: it used lowercase
`facebook`/`instagram` JSON keys, but `meta.Placement` has no json tags so those were
silently ignored and the client applied its both-feeds default — switched to the correct
`FacebookFeed`/`InstagramFeed` keys and now assert instagram is ABSENT from targeting
(proving the `InstagramFeed:false` override is honored). Gave `CampaignCreateInput.
platforms` a deterministic UNIQUE `Example` ([reddit-ads, meta-ads] — two providers with
a registered dispatcher on this branch, so a consumer copying it doesn't hit "no
dispatcher registered") — Goa's auto-example otherwise repeated the first enum value
(duplicate `reddit-ads`), which the handler rejects. Regenerated the Goa API, dropped the now-non-pointer `cfg.PageID` deref in the
connection service, updated internal-dispatch.md, and strengthened the meta happy-path
dispatch test to assert the full mapping contract (objective→OUTCOME_SALES, lifetime
budget in minor units, geo countries, pixel + page promoted objects, per-variant
creative/ad fan-out).

**Update** — Added the linkedin and meta PlatformDispatcher adapters to
`internal/dispatch` (LFXV2-2638 / 2640), following the reddit template from the
Creation entry below. Each reuses the shared `credsSource` (Get → Decrypt) and does
its own per-platform interpretation: linkedin unmarshals a single OAuth2 accessToken +
builds RuntimeConfig from the connection's AccountID + numeric `org_id`; meta uses an
accessToken + AccountID (`act_...`) + `page_id`, budget in the account's currency (no
FX). Both are registered in `container.registerDispatchers` (fast path + cold-start
retry) alongside reddit — three of the paid providers. The twitter adapter (OAuth1
4-tuple, LFXV2-2642) is planned on a later branch and not yet registered. Each has
pre-create/NoUpstreamCreate tests + a happy-path through the real client against an
httptest server. Google Ads follows once its client (PR #33) lands; email/HubSpot is
LFXV2-2777.

**Creation** — Added `internal/dispatch` — the per-platform PlatformDispatcher
adapters that wire the orchestrator to the ad-platform clients (LFXV2-2639, Reddit
first). Until now the orchestrator's `dispatchers` map was empty, so campaign creation
recorded jobs that dispatched to nothing. The package has: a SHARED `credsSource`
doing the one mechanical step common to every platform (ConnectionReader.Get →
Encryptor.Decrypt, returning the raw plaintext + AccountID/ProviderConfig/Status) —
deliberately NOT interpreting the blob, since credential shapes differ per platform;
and a PER-PLATFORM `RedditDispatcher` that unmarshals its own `redditCreds` (OAuth2),
maps the brief's event fields + the per-platform `config` onto `reddit.CampaignInput`,
calls the client, and maps the result → `model.Campaign`. Claim contract: pre-create
failures (missing/invalid connection, config/credential errors, or a client `(nil,
err)`) are wrapped `notCreated` → a `preCreateError` implementing
`NoUpstreamCreate()`, so the orchestrator RELEASES the claim; ANY non-nil client
result + error (ambiguous create — the decision keys on result!=nil, NOT on a
populated id, since an ambiguous create returns a name-only partial whose id may be
empty) is handed back so the claim is RETAINED and the orphan recorded. Registered in
`internal/container`
(`registerDispatchers`, called from both the fast path and the cold-start retry path);
`logMissingDispatchers` warns for ad providers still without an adapter. Concept doc +
index added; dispatch/container/service tests green (-race).

## 2026-07-20

**Update** — Fixed "briefs stay broken after a cold-start DB retry" (PR #28 review,
cursor High, surfaced after #11 merged into #28). After #11 added the brief service +
orchestrator to the container, the 503-mode background retry only late-bound the
CONNECTION service + readiness — it never re-wired the BRIEF service, so brief/job
routes returned 503 for the whole pod lifetime while `/readyz` flipped to healthy
(readiness OK but routes 503 — worse than "unavailable"). Fixed: (1) gave
`BriefService` a `SetBackend(briefs, campaigns, jobs, orch)` late-binding setter
guarded by an RWMutex, with handlers now snapshotting collaborators via `ready()`
(so a mid-request swap can't race); (2) the retry goroutine now fully re-wires — brief
`SetBackend` + orchestrator + `FailStuckJobs` + `StartRecoverySweeper` — and flips
readiness LAST so `/readyz` never reports OK while brief routes still 503; (3) 503-mode
boot now wires a nil-repo brief service (routes mounted → typed 503, not a nil panic).
Added `TestBriefService_SetBackend_LateBinding` + a container 503-mode assertion.
Race-clean.

**Update** — Documented the Traefik `RegularExpression` HTTPRoute version requirement
(PR #28 review, copilot). Copilot claimed Traefik's Gateway API provider doesn't
support `RegularExpression` path matches (only Exact/PathPrefix) → the project-nested
route would be silently unrouted. VERIFIED WRONG against Traefik's source
(`buildPathRule`, every v3.1.0+ tag): a `RegularExpression` match is translated to a
native `PathRegexp(...)` rule (RE2/Go-regexp), GA, not gated. BUT two real nuances:
(1) **v3.0.x does NOT support it** (returns "unsupported path match"), so it requires
Traefik >= v3.1.0 — now stated in the template comment + concept doc; (2) the feature
is NOT in Traefik's Gateway API conformance report even though the code implements
it, so the render alone doesn't prove routing — added a note to verify the deployed
HTTPRoute's `Accepted` status condition is True. Replaced the vague "custom
conformance" wording. No route change (works on the platform's v3.1.0+ gateway).
NOTE: no other LFX service uses RegularExpression HTTPRoute (query-service uses
PathPrefix/Exact) because they route on their own top-level prefix; campaign-service
can't (project-service owns /projects/), hence the regex.

**Update** — Corrected the "re-run after a partial migration is harmless" doc claim
(PR #28 review, copilot). The container concept doc and the `Migrate` doc comment
said migrations are idempotent so a re-run after a partial is harmless — but that's
wrong for a PARTIAL (dirty) migration: golang-migrate marks the schema dirty
precisely because partial migration SQL is not assumed idempotent, and a re-run then
hits `ErrDirty` (needs manual `force`, exactly the permanent-failure path documented
above). Reworded both to scope the "skipped/harmless" claim to a CLEAN schema and
describe partial failure as the dirty/manual-recovery state.

**Update** — Fail fast on a PERMANENT migration failure instead of 503-looping
forever (PR #28 review, copilot + cursor). The 503-mode retry loop retried
`initDatabase` on ANY error — so a dirty schema (`migrate.ErrDirty`, set when a prior
migration failed partway) would loop forever behind a 503, with no fail-fast signal.
A dirty schema can't clear by re-running Migrate; it needs an operator to force the
version. Added `postgres.IsPermanentMigrationErr` (classifies a wrapped
`migrate.ErrDirty`); the synchronous fast path now returns an error (process exits
loud) and the background retry loop logs ERROR + stops looping on it. Connectivity /
lock / deadline failures are deliberately still transient (they retry). Note: the
overlapping-migration half of these findings was already fixed earlier (migrateMu +
pool-first-then-Migrate); these older bot comments predate that. Test added.

**Update** — Made the pgx DSN-parse errors DSN-free (PR #28 review, copilot). Both
`NewPool` and `ValidateMigrationDSN` wrapped `pgxpool.ParseConfig`'s error with `%w`;
NewContainer propagates it and main logs it, so a malformed credential-bearing
DATABASE_URL risked logging the connection string. VERIFIED that pgx's
`ParseConfigError` already redacts the password (`redactPW`) across every malformed
DSN shape I probed (bad port, space-in-host→url.Parse-fails-falls-to-keyword-regex,
bad connect_timeout/sslmode, keyword form) — so the finding's literal "leaks the
password" claim is not currently true. BUT we shouldn't depend on a dependency's
best-effort redaction for a secret, so wrapped both sites in a `dsnParseError` whose
Error() renders a STATIC DSN-free message and whose Unwrap() keeps the pgx cause for
errors.Is/As + diagnostics. Test asserts a password/DSN never reaches Error() while
the cause stays unwrappable.

**Update** — Added the route/rule PARITY test (PR #28 review, copilot). The PR
described an RE2 route/RuleSet parity regression guard, but none was committed — the
HTTPRoute regex and the Heimdall RuleSet path list are two hand-maintained matchers
with nothing coupling them, so a drift (a forwarded-but-unruled path) would skip the
campaign_manager FGA check unnoticed. Added `TestRouteRuleSetParity`
(`charts/lfx-v2-campaign-service/parity_test.go`): renders both templates via `helm
template`, extracts the RE2 regex + the RuleSet's project-nested patterns (translating
Traefik `:projectId`/`*`/`**`), and asserts a curated accepted/rejected path table
matches identically in both matchers (skips if helm absent; fails on render error).
Verified non-vacuous by flipping an expectation. httproute concept doc updated.

**Update** — Scoped the parity test to the campaign_manager rule (PR #28 review,
copilot). `extractRulePatterns` treated ANY `/projects/` path anywhere in the RuleSet
as "authorized", so a path moved into an allow_all/deny_all/differently-scoped rule
would still satisfy parity — but the actual invariant is campaign_manager on
project:{projectId}, not just "some rule matches". Now extraction is scoped to the
`project-api` rule BLOCK (isolated from its `- id:` to the next), and a new
`TestProjectAPIRuleEnforcesCampaignManager` (also called from both parity tests)
asserts that rule's authorizer is openfga_check with relation campaign_manager +
object project:{projectId}. A rule downgrade/re-scope now fails the security test.

**Update** — Strengthened the parity test to couple to matcher CONTENT (PR #28
review, copilot). The curated table only sampled fixed paths, so a one-sided
matcher edit that no case exercised (copilot's example: adding `tiktok-ads/metrics`
to the route regex only) would still pass. Added `TestRouteRuleSetParityWitnesses`:
it enumerates concrete example paths from the route regex's AST (`regexp/syntax`
walker — one witness per alternation leaf, `[^/]+`/`.*` collapsed to literals) and
requires each to be RULED, and builds a witness from every RuleSet pattern and
requires the route to FORWARD it. A route-only new branch now yields an unruled
witness → fail; a RuleSet-only entry yields an unforwarded witness → fail. Verified
against copilot's exact scenario (`/projects/x/tiktok-ads/metrics` is caught).

**Update** — Bounded the migration step with the startup deadline (PR #28 follow-up
review, cursor Medium). After the earlier pool-first fix, `initDatabase` still ran
`postgres.Migrate` (no context) synchronously with no time bound, so a reachable
but slow/lock-blocked migration could block `NewContainer` indefinitely. Now
Migrate runs in a goroutine under a package `migrateMu` (serializes runs so a retry
never starts a second migration while a prior deadline-abandoned one is finishing)
and the caller returns on the startup deadline. Also cleaned a union-merge artifact
in this log (duplicated oversized-body line).

**Update** — Hardened the #28 503-mode cold-start fix after review (cursor HIGH +
copilot). (1) `initDatabase` started `postgres.Migrate` (uncancellable Up()) in a
goroutine and returned on the 15s deadline WITHOUT waiting — so the retry loop
launched another migration while the previous was still blocked, leaking goroutines
and racing concurrent migrations. Reworked to open the pool FIRST (NewPool does a
context-bounded Ping) and run Migrate only after a reachable ping, so Migrate never
blocks against a down DB and retries never overlap. (2) A malformed DATABASE_URL
(keyword DSN) is deterministic, so `NewContainer` now fails fast via
`postgres.ValidateMigrationDSN` instead of 503-looping forever. (3) Corrected the
service.go comments/doc that claimed a NIL readiness dep makes /readyz not-ready —
a nil dep is treated as READY (no-DB mode); cold-start uses the non-nil notReady{}
checker. (4) The connection 503 message "not configured" → "unavailable" (during
cold start the DB is configured, just unavailable). Tests + concept doc updated.

**Update** — Made the DB cold-start startupProbe budget real (PR #28 review,
LFXV2-2558). `NewContainer` capped migration+pool init at 15s and `main` exited
on failure, so an unreachable DB at boot crash-looped the pod and the ~90s
startupProbe budget never applied. Now a *transient* DB-init failure boots the
services in 503 mode (a `notReady` health dep so `/readyz` returns 503, distinct
from no-DB mode; connection service nil-repo) and a background goroutine retries
migration/pool, swapping the live pool/repo in via `SetReadinessDep`/`SetBackend`
(mutex-guarded against concurrent request reads) once it opens. Config errors
(invalid DB settings, bad encryption key) still fail fast. `Close` cancels the
retry goroutine. Updated the container + deployment concept docs and the
startupProbe comment.
**Creation** — Added the `campaign_audiences` resource — DB layer (LFXV2-2773 subtask
2781, email epic LFXV2-2770). Migration `000005` creates `campaign_audiences` (a built
audience subordinate to a brief: `brief_id` FK to `campaign_briefs`, columns store a
POINTER + provenance — `platform_master_list_id`, `suppression_list_ids`,
`inclusion_summary`, `status` building/built/failed, `version` — NOT the audience
contents, which stay in HubSpot). This is the "B2" decision: a built audience is a
first-class, inspectable, reusable, versioned LFX resource. Added `model.CampaignAudience`
(+ AudienceStatus, StatusOrDefault), `domain.AudienceRepository` interface, and
`postgres.AudienceRepo` (create/get/list/update; project-scoped; optimistic-concurrency
update gated on version → ErrPreconditionFailed, matching ReplaceCampaign). Indexed on
brief_id + project_id (no natural uniqueness — a brief may have many audiences). The
Goa API/handlers + route/rule wiring are the sibling subtasks (2782/2783); the repo
isn't consumed until the service exists. Model unit test added; per repo convention
(no DB unit tests here — repos are covered via service-layer fakes) the migration is
validated on boot. Whole-module build/vet/test green; concept doc + log updated.

**Update** — Idempotency-lookup errors no longer silently fall through to dispatch
(PR #11 review, cursor Medium). In `dispatchPlatform`, the fast path treated ANY
non-nil error from `GetCampaignByPlatform` like "no row" and fell through to
claim/dispatch — so a transient/real DB failure that hid an existing campaign could
trigger a duplicate upstream create, with no log/signal. Now the outcomes are
distinguished: existing-with-upstream-id → reuse; `ErrNotFound` → fall through to the
claim; any OTHER error → surface as a platform failure (logged ERROR), not a blind
dispatch. Corrected the concept doc, which had documented the old swallow-the-error
behavior as intentional. Test added (`TestOrchestrator_IdempotencyLookupErrorIsFailure`).

**Update** — Addressed dealako's 4 [minor] review items on PR #11 (LFXV2-2626).
(1) `GetCampaignByPlatform` was the one campaign_repo method not scoped by
project_id — added a `projectID` param + `AND project_id=$3` (matching
GetCampaign/ClaimCampaignDispatch) for tenant-isolation defense-in-depth; updated
the domain interface + the orchestrator call site. (2) The rare double-fault in
`ClaimCampaignDispatch` (post-insert read AND rollback both fail) orphans a
`status='pending'` row that permanently blocks the (brief,platform) pair — now
logs at ERROR with project_id/brief_id/platform/job_id for alerting/manual
reconcile. (3) Added `TestClaimCampaignDispatch_ConcurrentSingleWinner` — N
goroutines racing the claim path, asserting exactly one wins and losers cleanly
no-op (the prior claim tests were single-threaded). (4) `design/brief.go`: `Brief`
now `Reference()`s `BriefInput` for the 8 shared attributes instead of
duplicating them — this also fixed a latent drift the manual copy had already
caused (Brief's `program_type` was missing BriefInput's Enum, so the generated
OpenAPI had no enum + gibberish examples on the Brief response; regenerated).
**Update** — Closed the second half of the X/Twitter URL leak (PR #31 review,
copilot). The transportError fix covered the AMBIGUOUS branch, but the PRE-SEND
branch (`isPreSendDialError` → DNS/connect-refused) still did a raw
`fmt.Errorf("... %w", err)` of the `*url.Error`, so a DNS/refused failure on a
create still rendered the request URL (X puts create params in the query string)
into persisted Steps. Added a `preSendError` type mirroring transportError's
URL-free `Error()` (via `safeTransportCause`) but semantically DEFINITE (request
never sent → not applied, unlike ambiguous transportError); `Unwrap()` retains the
cause so `isPreSendDialError`/`errors.Is` still match. Test added
(`TestPreSendError_DoesNotLeakURL`). NOTE: reddit/meta (merged) have the SAME raw
`%w` pre-send render — same follow-up as the transportError leak applies there.

**Update** — Fixed a URL leak + stale docs on the X/Twitter client (PR #31 review,
cursor Medium + dealako + copilot). (1) `transportError.Error()` rendered `%v` of
the wrapped `httpClient.Do` error — typically a `*url.Error` embedding the full
request URL (which can carry request material / a destination's secret query) —
and that string was copied into `PromotedTweetWarning` + persisted `Steps`. Added
`safeTransportCause` which unwraps a `*url.Error` to its underlying cause
(timeout/EOF/reset) with no URL; `Error()` now renders method/path + that. Test
added. NOTE: reddit/meta (merged) have the same `%v` transportError render —
follow-up to apply the same URL-suppression there. (2) Corrected the stale
`createOutcomeAmbiguous` header comment that still claimed "NOT gated on the HTTP
method" after the 3xx gate was re-added. (3) Documented CreateCampaign's
non-standard `(non-nil result, non-nil error)` contract so callers inspect the
result on error (for reconcile) instead of discarding it.

**Creation** — Added the `internal/platform/hubspot` Go package (email-channel
scaffold, LFXV2-2778 under epic LFXV2-2770). HubSpot's auth is the simplest of any
client — a STATIC private-app bearer token (no OAuth token-exchange flow), attached
directly by `doRequest`. The request layer mirrors the googleads/reddit/meta/twitter
discipline: no-follow redirects (fresh-client rebuild so a `WithHTTPClient` caller
isn't mutated), bounded 10 MiB reads, typed `apiError` (method/path/status only,
body never surfaced) + `transportError` (URL-free via `safeCause`, cause retained
via Unwrap), `isPreSendDialError` pre-send classification, and 429 retry gated on an
explicit `idempotent` flag (a non-idempotent create is never retried — no idempotency
key → double-create risk). Concept doc + code index added.

**Creation** — Added the HubSpot marketing-email ops (LFXV2-2779) + CRM-list/event-def
ops (LFXV2-2780) on the client. `email.go`: SearchEmails/GetEmail (idempotent),
CloneEmail, PatchEmailSettings, SetSendList. `lists.go`: SearchLists, GetList
(includeFilters=true → filterBranch + processingType), CreateList (DYNAMIC,
objectTypeId 0-1, opaque filterBranch), UpdateListFilters (PUT …/update-list-filters),
ListEventDefinitions. Creates/clones are non-idempotent; a 2xx-with-no-id is
UNCONFIRMED (a resource may exist → verify, don't blind-retry). SetSendList sets
recipients via `contactIlsLists` (ILS list ids) ONLY — HubSpot removed the legacy
`contactLists` recipient field after 2024-10-31 (see the 2026-07-21 ILS-only update),
so the client never emits it. Sends a complete `to` (contactIds cleared) with the ILS
send list + its suppressions. filterBranch shape invariants stay with the
audience-builder (LFXV2-2774), not this client. Full gate green.

**Creation** — Added the `internal/platform/snowflake` Go package (email channel,
LFXV2-2772 under epic LFXV2-2770): a READ-ONLY Snowflake client that resolves
past-edition EVENT_NAME/EVENT_ID from `ANALYTICS.PLATINUM_LFX_ONE.event_registrations`
for HubSpot BEHAVIORAL_EVENT filters. Read-only BY CONSTRUCTION — no arbitrary-SQL
entry point (unlike the reference app's `snowflake_query(sql)`); the one method
`ResolvePastEventNames` builds a fixed, fully-parameterized SELECT DISTINCT (terms
bind as ILIKE ?/NOT ILIKE ?, never interpolated; identifiers are constants guarded by
`ident`; LIMIT-capped). Source is PLATINUM (not the reference's Silver_Segment).
Fail-closed on error/empty (callers must NOT substitute guessed names). Key-pair (JWT)
auth via injected PKCS8 PEM, with `.env`-mangling tolerance (quotes/`\n`/CRLF); pool
opens lazily; DSN never quoted into errors. Tested with a hand-rolled in-process
database/sql driver fake (no new test dep) — 9 cases asserting query shape,
injection-safety, fail-closed, and key parsing. **DEPENDENCY:** adds
`github.com/snowflakedb/gosnowflake` v1.19.1 (the only official Go Snowflake driver;
no shared Go Snowflake service exists — the LFX One UI's Snowflake service is
TypeScript). Concept doc + code index added; `go mod tidy` run.
**Update** — Two more GA-2 partial/pre-send fixes (PR #33 review, copilot). (1) The
ambiguous/duplicate BUDGET partial exposed only `CampaignName`, but the resource that
may exist is a budget created under a DIFFERENT name (`LFX | Budget | …`) — with no id
yet, a caller couldn't reconcile it. Added `CampaignBudgetName` to `CampaignResult`
and populated it in every partial. (2) A pre-send contract hole: with a CACHED OAuth
token, an already-cancelled context reached `httpClient.Do`, got wrapped as a
`transportError`, and was reported UNCONFIRMED — but nothing was sent, so it's a clean
failure. Added an explicit `ctx.Err()` check immediately before the first mutate →
`(nil, err)`. (Without a cached token the token fetch surfaced the ctx error pre-send
anyway; the cached-token path reaches Do directly, hence the explicit guard.) Tests
added for both (the pre-send test warms the token cache first).

**Update** — Added `networkSettings` to the GA-2 SEARCH campaign create (PR #33
review, copilot — verified against v23 docs before applying). A SEARCH campaign that
targets NO network is rejected with
`CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK`, and an omitted
`networkSettings` resolves to exactly that (proto3 bools default false) — Google
documents no protective default and every official create sample sets it. The
rejection lands on `campaigns:mutate` AFTER the budget commits, so it would orphan the
budget. Now sends `networkSettings{targetGoogleSearch: true, targetSearchNetwork:
false, targetContentNetwork: false}` — Google Search only (conservative for a PAUSED
broker shell; targetSearchNetwork=true would require targetGoogleSearch AND opt into
Search Partners). Happy-path test now asserts the networkSettings block. Concept doc
updated.

**Update** — Corrected the GA-2 name-length limits after re-verifying the v23 docs
(PR #33 review round 3, copilot — TWO contradictory claims: one said 255, one said
128; BOTH wrong for Campaign). Authoritative from the v23 System Limits table + RPC
field refs: `Campaign.name` = up to **256 CHARACTERS** (`StringLengthError.TOO_LONG`);
`CampaignBudget.name` = **1..255 UTF-8 BYTES** (trimmed). Different number AND unit.
My earlier "128 chars" campaign cap was simply wrong (over-strict, rejecting valid
names). Fixed: `maxCampaignNameRunes=256` (validated via `utf8.RuneCountInString`),
`maxBudgetNameBytes=255` (validated via `len`); `validateEntityName` now takes the
measured length + unit label so each name is measured in its correct unit (a
multibyte name would otherwise slip past the budget's byte ceiling). Also confirmed
v23 forbids NUL/LF/CR in `Campaign.name` — already handled by the control-char
stripping in `sanitizeNamePart`. Replaced the 128-overflow test with a byte-limit
preflight test + a units (bytes-vs-runes) test. LESSON: when two AI reviewers give
contradictory numbers, verify against the primary source before implementing either.

**Update** — Fixed several GA-2 correctness bugs from PR #33 review (copilot +
cursor), verified against the v23 docs: (1) campaign create now sets the REQUIRED
`containsEuPoliticalAdvertising: DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` —
omitting it fails every create with FieldError.REQUIRED (and since 2026-04-01 an
undeclared account has ALL mutates rejected), which would have orphaned the budget.
(2) The campaign duplicate check used `DUPLICATE_NAME` (the BUDGET code); campaigns
use `CampaignError.DUPLICATE_CAMPAIGN_NAME` — split into isDuplicateBudgetNameErr /
isDuplicateCampaignNameErr so the campaign branch actually fires. (3) A mutating
429 is now UNCONFIRMED (doRequest suppresses its retry precisely because it may
have committed — was mis-classified as a clean failure → double-create risk). (4)
Error codes are now parsed from the FULL body in doRequest and retained on
`apiError.ErrorCodes`; hasErrorCode reads that field instead of re-parsing the
truncated `Body` (a real error JSON exceeds maxErrorBodyChars, so the old on-demand
parse of the truncated snapshot silently dropped codes, breaking all duplicate
detection). (5) A ctx check between the budget and campaign mutates skips the
campaign create on a done context, returning the budget as a reconcilable partial.
(6) Clarified docs: a campaign-create 4xx doesn't mean nothing was created (the
budget exists); the non-shared-budget name-reuse-on-retry corollary is undocumented
so retry-safety relies on a stable NameSuffix. Concept doc + index updated (GA-1→GA-2).

**Update** — Second GA-2 review round on PR #33 (5 fixes): (1) split the name-length
limit into `maxBudgetNameLen=255` / `maxCampaignNameLen=128` and validate each name
against its own limit — v23 permits a 255-char budget name but only 128 for a
campaign, so the collapsed single limit let a 129–255-char campaign name pass
preflight and get rejected by the paid campaigns:mutate AFTER the budget was
created (avoidable orphan). (2) Require BOTH Project AND EventName independently (was
either-or): Project is the attribution key the pipeline parses from the name, so a
one-segment name is mis-attributed. (3) Added `sanitizeNamePart` to strip the `|`
delimiter from caller segments before composing — a raw `|` would inject extra
pipe-fields and break name-based reconciliation/attribution. (4) `firstResourceName`
now returns (resourceName, id) and errors on a present-but-MALFORMED resourceName
(no id segment, e.g. `customers/1/campaigns/`) → UNCONFIRMED, instead of continuing
with an empty unreconcilable id. (5) Fixed the RejectsBadInput test (its budget
cases now set Project+EventName so they exercise the budget checks, not the new
attribution checks that run first) + added tests for the 128-overflow, pipe-strip,
malformed-resourceName, and firstResourceName cases. Concept doc updated.

**Update** — GA-2 PR #33 follow-up (copilot): renamed `CampaignInput.BudgetUSD` →
`Budget` (and `maxBudgetUSD` → `maxBudget`). Google applies `amountMicros` in the ad
account's OWN currency and this client does no FX conversion, so the `USD` suffix
was a false promise — 50 on a EUR account is 50 EUR/day, not ~54. Field comment now
states it's account-currency (NOT USD), and the budget-created step no longer
hardcodes a `$` sign. Mirrors the meta client, which renamed the same field for the
same reason. No behavior change (the value was already sent as-is).

**Update** — GA-2 PR #33 follow-up (cursor Bugbot): the both-fields-required check
validated the RAW input (`strings.TrimSpace`), but composeName only includes a
segment when its `sanitizeNamePart` is non-empty — so a delimiter-only value like
`"|||"` passed validation yet sanitized to nothing, dropping the Project segment
while still creating a paid budget/campaign. Fixed by validating the SANITIZED
value (`sanitizeNamePart(in.Project/EventName) == ""`) so validation and
composition stay consistent; added pipe-only test cases.

**Creation** — Added Google Ads campaign creation (GA-2, LFXV2-2637) in
`internal/platform/googleads/campaign.go`: `CreateCampaign` creates a PAUSED SEARCH
campaign as two sequential `:mutate` calls — a non-shared STANDARD `campaignBudget`
(amountMicros = budget×1e6) then a `campaign` referencing it with a `manualCpc {}`
bidding strategy. Both resource ids surfaced. Because `:mutate` has no idempotency
key, added `createOutcomeAmbiguous` (5xx/transport ambiguous always; 3xx only on a
mutating method) + `isDuplicateNameErr` (4xx DUPLICATE_NAME → already-exists) +
machine-readable error-code parsing (`error.details[GoogleAdsFailure].errors[].errorCode`,
body never surfaced, codes bounded): an ambiguous or 2xx-no-resourceName outcome →
UNCONFIRMED + reconcilable partial (carries the budget id once created); a definite
4xx → clean failure. Deterministic composed names so a retry collides on
DUPLICATE_NAME rather than double-creating. Table-driven httptest coverage for
every branch. Concept doc updated.
**Update** — Extended the Meta ad-set ambiguity to the 2xx-no-id case (LFXV2-2641,
PR #30 review by Copilot). The ad-set create's error path already routed through
`createOutcomeAmbiguous`, but a 2xx response with an empty `id` fell through to a
definite "returned no ad set ID" — the same duplicate-create risk as the campaign
and twitter no-id paths. Now surfaces UNCONFIRMED (verify before retrying). Test
added. Also fixed a CI `check-fmt` failure (gofmt comment alignment in the meta
test).

**Update** — Extended the X/Twitter create-outcome ambiguity to the INITIAL
CAMPAIGN create (LFXV2-2642, PR #31 review by Cursor + Copilot) — the last
uncovered create step. The campaign POST returned a bare `(nil, err)` on an
ambiguous 3xx/5xx/transport failure and a plain error on a 2xx-no-id, discarding
the deterministic campaign name; X may have committed the PAUSED campaign, so a
caller got no reconcile signal and could retry into a duplicate. Now returns a
name-carrying partial result + UNCONFIRMED (verify before retrying) for both cases
(a definite 4xx/pre-send error still returns plain `(nil, err)`), mirroring the
meta/reddit clients' name-only partial for the first create step. The whole
twitter flow (campaign → line item → promoted tweet) now classifies every create
outcome consistently. Tests added.

**Update** — Extended the X/Twitter create-outcome ambiguity to the LINE-ITEM
create (LFXV2-2642, PR #31 review by Cursor). The line-item POST always returned a
definite "line item creation failed" (even on a 5xx/mutating-3xx/transport error
where X may have committed it) and a definite "returned no line item ID" on a
2xx-no-id — the same blind-retry/duplicate risk already fixed for the campaign,
promoted-tweet, and meta ad-set paths. Both now surface UNCONFIRMED (verify before
retrying) when ambiguous; a definite 4xx/pre-send error still reads "failed".
Also updated the `PromotedTweetWarning` field contract (it told consumers the
promoted tweet "may need to be added manually", which for an UNCONFIRMED outcome is
the duplicate risk this exists to prevent — now it requires verifying before adding
or retrying) and corrected the twitter concept doc's "shallow copy" wording to the
fresh-client construction.

## 2026-07-19

**Update** — Fixed an http.Client copy-after-use in the Meta client's no-follow
enforcement (LFXV2-2641, PR #30 review by Copilot). `NewClient` value-copied a
`WithHTTPClient`-supplied client (`hc := *c.httpClient`) to override CheckRedirect
— but an `http.Client` must not be copied after first use (the copy duplicates its
internal mutex while sharing the request-cancellation map, so concurrent use of
the caller's client and the copy can race). Now builds a FRESH `*http.Client`
carrying only the exported reusable fields (Transport, Jar, Timeout) with
`CheckRedirect: noFollow`. The no-follow test asserts Transport/Timeout are
preserved and the fresh client is a distinct pointer. Also made the campaign
UNCONFIRMED step reason-neutral ("ambiguous response — timeout, server error, or
an unfollowed redirect") since a 3xx now routes there too. NOTE: the reddit client
(merged) has the same value-copy pattern — follow-up tracked to apply the same
fresh-client fix there. The twitter client gets the same fix on PR #31.

**Update** — Closed two more Meta ambiguity gaps (LFXV2-2641, PR #30 review by
Copilot). (1) `doRequest` returned a plain error when a NON-2xx response body
failed to read, stripping the HTTP status — so a mutating 3xx/5xx with an
unreadable body (the create may have committed) was mis-seen as a definite failure
by `createOutcomeAmbiguous` (which keys on the `*APIError` status). It now returns
an `*APIError` preserving the status on a non-2xx read failure (2xx read failures
stay `transportError`). (2) The ad-set create returned its error directly without
the ambiguity check the campaign and ad/creative creates use, so a surfaced 3xx/5xx
read as a definite "ad set creation failed" — risking a duplicate ad set on retry.
It now routes through `createOutcomeAmbiguous`: ambiguous → UNCONFIRMED (verify
before retrying), definite 4xx → "failed". Tests added for both. (3) The same
status-stripping existed in the OVERSIZED-body branch (> maxResponseBody, 10 MiB), which returned a
plain error before recording the status — a mutating 3xx/5xx over the cap was still
mis-classified as a definite failure. Now the oversized-body branch preserves the
status the same way (2xx → transportError, non-2xx → *APIError), with a regression
test. Updated the meta concept doc to describe the fresh-client + status-preservation.

**Update** — Gated the Meta client's 3xx create-outcome ambiguity on a mutating
method (LFXV2-2641, PR #30 review by Cursor Bugbot). `createOutcomeAmbiguous`
treated EVERY 3xx as UNCONFIRMED without checking the method, diverging from the
reddit client (which gates 3xx on `isMutatingMethod`) despite claiming to mirror
it. All call sites pass POST today so behavior was unchanged, but the helper's
contract was wrong for any future GET caller — a GET redirect is not a create.
Added `isMutatingMethod` to the meta client and gated the 3xx branch (5xx and
transport errors stay ambiguous regardless of method); extended the ambiguity test
with GET/POST/DELETE method cases. Now genuinely identical to reddit.

**Update** — Fixed the http.Client copy-after-use in the X/Twitter client's
no-follow enforcement (LFXV2-2642, PR #31), matching the meta fix (PR #30):
`NewClient` now builds a fresh `*http.Client` (Transport/Jar/Timeout + noFollow)
instead of value-copying the caller's; the no-follow test asserts Transport/Timeout
preservation and a distinct pointer.

**Update** — Gated the X/Twitter client's 3xx create-outcome ambiguity on a
mutating method (LFXV2-2642, PR #31), matching the same fix applied to the meta
client (PR #30, Cursor review) and the reddit client. `createOutcomeAmbiguous`
had treated every 3xx as UNCONFIRMED regardless of method; now a 3xx is ambiguous
only on a mutating method (a GET redirect is not a create), while 5xx and
transport errors stay ambiguous regardless of method. Added `isMutatingMethod`
and GET/POST/DELETE test cases. All three clients (reddit/meta/twitter) now share
an identical method-gated contract.

## 2026-07-18

**Creation** — Added the `internal/platform/googleads` Go package (GA-1 scaffold,
LFXV2-2636): a Google Ads REST client (not gRPC) with OAuth2 refresh-token auth
(single-flight leader/follower, secret-safe errors), a request layer (no-follow
redirects, bounded reads, pre-send/ambiguous/definite classification, 429 retry
gated on an explicit idempotent flag since GAQL search is POST-but-read-only), and
cursor-paginated GAQL search with page/row caps. customer_id validated digits-only.
GAQL gotcha documented: v23 replaced campaign.start_date/end_date with
campaign.start_date_time/end_date_time. Concept doc + code index updated. Campaign
creation (:mutate), metrics/keywords/audience, and keyword actions follow in
GA-2..GA-5.

**Update** — Routed the project-nested campaign API through the gateway and gave it
real authz (PR #28, LFXV2-2558). The chart previously routed only `/campaigns`, so
the actual contract paths (`/projects/{projectId}/…`) were unreachable. httproute
now uses a `RegularExpression` match selecting this service's project-nested
subpaths (`connection-*`, `briefs`, `jobs`, `{provider}/metrics`,
`google-ads/keywords|audience`, `hubspot`), leaving `project-service`'s `/projects/`
routes untouched. ruleset replaces the `/campaigns` `deny_all` placeholders with a
single `project-api` rule gating every routed family on the project
`campaign_manager` relation (`openfga_check` scoped to `project:{projectId}`, D2),
with `oidc` + `anonymous_authenticator` paired (openfga_check is what rejects the
anonymous subject) and an `allow_all` fallback when OpenFGA is disabled (local dev).
A separate `campaigns-placeholder` rule keeps the still-routed `/campaigns` /
`/_campaigns/*` prefixes fail-closed (`deny_all`), preserving the chart↔route parity
invariant (every heimdall-routed path has a matching rule). deployment readiness
`failureThreshold` relaxed 1→3 for CloudNativePG cold start. Concepts updated:
`httproute`, `ruleset`.
**Update** — Also strengthened the no-follow regression tests (meta + twitter):
they injected a nil-`CheckRedirect` client, which couldn't prove the override is
UNCONDITIONAL (a "fill only nil callbacks" impl would pass). Now they inject a
caller client carrying a SENTINEL `CheckRedirect` and assert the client the code
uses returns `http.ErrUseLastResponse` despite it, while the caller's original
still returns the sentinel (shallow copy, not mutation). (PR #30 review by Copilot.)

**Update** — Typed the X/Twitter Ads client's errors and added outcome
classification (LFXV2-2642). doRequest previously returned a bare fmt.Errorf for
every non-2xx AND echoed the response body into the error string (which can carry
signed URLs / destination secrets and gets persisted into Steps). Added a typed
`apiError` (status/method/path + X's machine-readable error codes, NO body),
`transportError` (ambiguous), `isPreSendDialError`, and `createOutcomeAmbiguous`
(a 5xx apiError or a transportError → UNCONFIRMED regardless of method; a 3xx →
UNCONFIRMED only on a mutating method, since a GET redirect is not a create; a
definite 4xx or a pre-send error → not ambiguous). `isDuplicatePromotedTweetErr`
now matches the typed error code
(DUPLICATE_PROMOTABLE_ENTITY, gated to a 4xx) instead of the no-longer-surfaced
body. Brings X to parity with the reddit/meta/googleads clients. Concept doc updated.

**Update** — Extended the X/Twitter create-outcome classification to the 2xx
edge (LFXV2-2642, PR #31 review by Copilot): a promoted_tweets POST returning a
2xx with no `data.id` was warning "add it manually" — but a 2xx means the POST
succeeded and X MAY have created the association, so a manual re-add risks the
duplicate the classifier prevents. Now that case is surfaced as UNCONFIRMED
(verify before retrying), same wording as the ambiguous-error branch;
`TestPromotedTweetMissingIDWarns` updated to assert the distinction.

**Update** — Gated the X/Twitter duplicate classification to a 4xx (LFXV2-2642,
PR #31 review): `isDuplicatePromotedTweetErr` matched `DUPLICATE_PROMOTABLE_ENTITY`
on any status and ran before `createOutcomeAmbiguous`, so a mutating 3xx/5xx
carrying that code was reported as a known duplicate instead of UNCONFIRMED (the
create may have committed on a 5xx). Now requires a definite 4xx; 3xx/5xx falls
through to ambiguous. Also reworded an UNCONFIRMED warning from "reached X" to
"may have reached X" (a transportError is only plausibly sent), and corrected the
`createOutcomeAmbiguous` log description (status/type-based + caller-scoped, NOT
"any GET failure → clean").

**Update** — Closed a no-body-leak regression in that same X/Twitter `apiError`
(LFXV2-2642, PR #31 review by Copilot): `Error()` was rendering the retained
`ErrorCodes` from the untrusted response body, re-opening the leak channel into
persisted Steps (an untrusted body can place secrets even inside `errors[].code`).
Now `Error()` renders method/path/status only; codes are kept solely for
`hasErrorCode` classification, and `parseErrorCodes` drops over-long values and
caps the count. Mirrors the reddit client's Body-for-classification-only pattern.

**Update** — Disabled HTTP redirect following on the Meta and X/Twitter Ads
clients (LFXV2-2641), closing a duplicate-create gap: both built their
`*http.Client` (and accepted `WithHTTPClient` clients) with no `CheckRedirect`, so
the stdlib could follow a 3xx on a mutating POST after the create was committed and
muddy outcome classification (for X, a followed redirect also resends an OAuth-1.0a
request signed for the original URL). Added a shared `noFollow`
(`http.ErrUseLastResponse`) policy set on the default client and enforced
unconditionally after options via a shallow copy (so a caller's client isn't
mutated) — matching the reddit/linkedin/googleads clients. Regression tests added.
**Update** — Reddit no-follow enforcement now builds a fresh `*http.Client` for a
`WithHTTPClient`-supplied client instead of value-copying it (LFXV2-2641).
`NewClient` did `hc := *c.httpClient; hc.CheckRedirect = noFollow`. The rebuild
carries over only the caller's documented exported fields (Transport, Jar, Timeout)
and sets `CheckRedirect: noFollow`, so it depends on the type's public API rather
than the struct's internal shape (layout-independent) and won't silently carry any
future unexported field. NOTE: this is NOT a race fix — on the repo's Go target
`http.Client` is just those four exported fields with no internal synchronization
state, so the old value copy was also correct (`go vet` copylocks does not flag
it). It's a defensive/clarity change. Strengthened the no-follow test to assert
Jar preservation (in addition to Transport/Timeout) and the caller-not-mutated
guarantee. Scope: reddit only — reddit is the sole client on main enforcing
no-follow on a caller-supplied client (merged via PR #27). The separately-proposed
PRs #30 (meta) and #31 (twitter), still open against main, ADD no-follow to those
clients and construct the client the same way.

## 2026-07-15

**Update** — Hardened the Reddit Ads client's ambiguous-outcome classification
(PR #27): `isPreSendDialError` now proves pre-send ONLY for DNS resolution and
connect-time dial failures (ECONNREFUSED/EHOSTUNREACH/ENETUNREACH). NO TLS error
is treated as pre-send, matching the merged Meta client — a TLS error is not a
reliable pre-send proof for an arbitrary caller-supplied transport (renegotiation,
or a wrapping RoundTripper surfacing a cert/record error while reading a response
after forwarding the POST), so both `*tls.CertificateVerificationError` and
`tls.RecordHeaderError` flow to the UNCONFIRMED path — the safe classification.
Redirect following is still force-disabled on every client used, including one
supplied via `WithHTTPClient` (`CheckRedirect` overridden to
`http.ErrUseLastResponse` UNCONDITIONALLY on a shallow copy, so the caller's
client is not mutated), which keeps 3xx handling well-defined. A 3xx on a MUTATING
request is classified UNCONFIRMED (it reached a responder and may have committed
before redirecting); a 3xx on a GET is not a create. A context error surfaced
from an IN-FLIGHT `Do` stays UNCONFIRMED (the per-attempt ctx wraps the whole
round trip, so it can fire after the POST reached Reddit) — but a cancellation
returned while waiting for token refresh is a proven pre-POST failure
(`refreshToken` returns `ctx.Err()` directly) and remains non-ambiguous.
5xx/mid-flight transport failures also stay UNCONFIRMED. Reworded the
manual-fallback UTM step to SET/REPLACE the utm_* params (matching
`buildRedditUTMURL`'s `url.Values.Set`), keeping all other query params and
dropping a trailing path slash.

## 2026-07-13

**Creation** — Added OKF concept doc for internal/platform/meta (Meta Ads Graph
API client) with `tags`/`timestamp` frontmatter (queryable fields per OKF v0.1
§4.1), listed in the code index.

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/reddit concept doc (queryable fields per OKF v0.1 §4.1).

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/linkedin concept doc (queryable fields per OKF v0.1 §4.1).

## 2026-07-10

**Update** — Addressed Copilot review on the X/Twitter Ads client (PR #19):
create calls now send params as URL query parameters (not a JSON body) per the
X Ads v12 contract, use `entity_status=PAUSED`, and line items carry the
required `start_time`/`end_time` with `bid_strategy` (not `bid_type`); dates are
strictly parsed to reject impossible calendar values; name lookups propagate
errors instead of masking them as not-found. Added the
`internal/platform/twitter` code concept and index entry.

**Update** — Mount connection routes in the HTTP server (LFXV2-2556): the
`cmd/campaign-service` concept now notes that every container-wired service
must also be mounted in `server.go`, or its routes 404 despite compiling.

**Creation** — Added the `internal/platform/reddit` concept doc for the new
Reddit Ads API v3 client (OAuth2 token refresh + Campaign -> Ad Group -> Ad
creation) and listed it in the code index.
**Update** — Hardened claim-based dispatch: resolve the dispatcher and reuse an
already-completed campaign BEFORE claiming (so a no-dispatcher platform never
leaves a permanent pending claim), release the pending claim if dispatch fails
before the upstream campaign is created, and bound concurrent provider calls with
a process-wide semaphore (previously the per-job errgroup limit let N concurrent
jobs each get maxParallelDispatch slots). Shutdown cancels in-flight runs on
drain timeout.

**Update** — Reworked LFXV2-2665 single-flight from a held-connection advisory
lock to an atomic claim row (INSERT ON CONFLICT DO NOTHING of a `pending`
campaign), removing the pool-exhaustion/blocking hazards of holding a connection
across the HTTP dispatch. The pending row is also the recovery signal for an
upstream-create-then-crash. Recovery scan uses a staleness cutoff so a rolling
deploy can't fail a job the old replica is still dispatching.

**Update** — Durable campaign dispatch (LFXV2-2665): per-platform single-flight
via an atomic claim row (ClaimCampaignDispatch — INSERT ON CONFLICT DO NOTHING of
a 'pending' campaign; see the later hardening entries above for the final shape,
which superseded an initial advisory-lock attempt), so concurrent
create-campaigns can't double-create upstream; the orchestrator drains in-flight
runs on graceful shutdown before the pool closes; and startup fails-forward jobs
left non-terminal by a restart. Added CampaignRepository.ClaimCampaignDispatch /
DeleteDispatchClaim and JobRepository.FailStuckJobs.

**Update** — PR #11 review round 3: validate brief_id/campaign_id/job_id path
params as UUIDs (400 instead of a PostgreSQL cast 500); make brief approval
version-gated via If-Match (rejects approving stale content, 412/428); type the
job-poll result (PlatformResult array, replacing Any); and stop applying
debug.LogPayloads to the connection/brief/health endpoints so DEBUG can't leak
BearerTokens or plaintext provider credentials into logs (debug.HTTP header/status
logging is retained). Reconciled api-catalog (PlatformResult; CampaignCreateResult
marked as the future richer shape).

**Update** — Brief + campaign API and async orchestrator (LFXV2-2626):
updated `design`, `internal/service`, and `internal/container` concepts for
the Project → Brief → Campaigns hierarchy, async job dispatch, and idempotent
per-platform creation. Behavior hardened per review: brief content replace
resets status to `draft` and persists `event_slug`; duplicate platform sets are
rejected; dispatch reuses an existing upstream campaign instead of re-creating;
brief responses carry `event_details`/`copy`/`keywords`/`targeting`; the
`(project_id, event_slug)` archived-aware partial unique index moved to a new
migration `000003` (never edit an applied migration in place); `platforms` is
enum-constrained and every brief method declares `BadRequest` (JWTAuth can 400).

**Creation** — Added OKF concept doc for internal/platform/linkedin (LinkedIn
Marketing API client), listed in the code index.

**Update** — Dropped the Goa CLI path allowlist; twitter-api-secret FP is
fingerprint-only in `.gitleaksignore`. Clarified `.grype.yaml` rationale
(Engine fixes exist; Go module path not yet upgradeable via migrate/dktest).

**Update** — Absorbed PR #18 grype fixes into the MegaLinter secrets work:
added `.grype.yaml` (ignore five transitive test-only `docker/docker`
CVEs) and `REPOSITORY_GRYPE_ARGUMENTS` in `.mega-linter.yml`. Kept the
narrower gitleaks allowlists from PR #24 (not #18's broad `^gen/`).

**Update** — Documented local MegaLinter/Docker workflow and tightened
`.gitleaks.toml` allowlists (narrow Goa CLI path + `.gitleaksignore`
fingerprint for twitter-api-secret false positive; sample AES key limited
to docs + `values.local.example.yaml`). Added architecture concept
`megalinter-secrets.md`.

## 2026-07-09

**Update** — Wired `CREDENTIAL_ENCRYPTION_KEY` into the Helm chart and local docs (required whenever a DB URL is configured so `/readyz` can start). Documented a non-production local sample key.

**Update** — Documented PostgreSQL readiness on `/readyz` (LFXV2-2559): updated service/config/container/constants concepts, added `internal/infrastructure/postgres` concept, noted PG* secret injection on Deployment, and added the `002-db-conn-check` feature-spec subtree.

**Creation** — initial OKF knowledge bundle generated from existing docs, Helm charts, Go packages, and speckit specs.
