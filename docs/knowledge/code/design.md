---
type: "Go Package"
title: "design"
description: "Package design contains the DSL for the campaign service Goa API generation."
resource: "design"
---

# design

Package design contains the DSL for the campaign service Goa API generation.

It defines four services: the health service (`readyz`/`livez`), the
connections service (per-provider singleton credential CRUD), the briefs
service, and the audiences service. The briefs service models the Project →
Brief → Campaigns hierarchy: brief CRUD (the funnel unit, carrying
`program_type`), asynchronous campaign creation (`POST .../campaigns` returns a
job to poll), campaign read/update, and the live reads that go straight to the
ad platform — `get-campaign-metrics` for performance and
`get-campaign-settings` for configuration divergence. Both are pure reads,
never persisted, and so declare no If-Match, unlike campaign updates, which
modify the stored row and require it. `get-campaign-settings` (LFXV2-3067)
answers what the metrics read structurally cannot: impressions, clicks, cost
and CTR do not describe a campaign's *configuration*, so divergence between
what a dispatch recorded and what the campaign now is upstream needs its own
method rather than another field on the metrics result. The audiences service (`design/audience.go`,
LFXV2-2773) models built campaign audiences nested under a brief
(`.../briefs/{briefId}/audiences`): create, get, list, and update-as-PATCH (a
load-then-merge where a nil field is unchanged; suppression lists are cleared via an
explicit `clear_suppression_lists` boolean, since an empty array can't round-trip through
the generated client's `omitempty` tag),
with optimistic concurrency via ETag/If-Match (`428` when missing, `412` on
mismatch). PATCH takes a dedicated `AudienceUpdateInput` (all fields optional, no
immutable `platform`) rather than the create-time `AudienceInput` (where `platform`
is required) — so a status-only or suppression-only patch is valid without resending
the immutable platform. Every method is gated on `campaign_manager` at the gateway via
`JWTAuth`, which can reject any request with an `Unauthorized` (401, carrying a
`WWW-Authenticate: Bearer` challenge) — so every brief, audience **and connection** method
declares `Unauthorized`, and still declares `BadRequest` for payload/path validation,
regardless of whether it accepts a body. The 401 arrived with LFXV2-3057; before it a
refused token and an invalid payload shared one status, which a client cannot act on
differently even though the remedies are opposite. The binding `platforms` selection is constrained to the known provider enum.

**A declared error is not documentation; it is the encoder.** Goa builds each method's
error encoder from that method's `Error(...)` list, so a typed error the method does not
declare has no `case` and falls through to the generic encoder — the caller gets **500**,
and the real status appears nowhere in OpenAPI. Nothing on the Go side shows it: the code
compiles, the handler returns the correct typed error, and only the wire status is wrong.
`JWTAuth`'s refusal is exactly such an error, which is why the declaration follows the
security scheme rather than the payload: a bodyless `GET` needs it as much as a create.
`TestEverySecuredMethodEncodesAuthErrors`
(`internal/service/connection_auth_encoder_test.go`) pins this by parsing the
**generated** encoders and requiring a `case` for BOTH `"BadRequest"` and `"Unauthorized"`
in every `Encode*Error` — it reads generated source rather than driving a list of encoders
because the case that must fail is a *newly added* provider method, which no
hand-maintained list would contain. It parses all three generated encoder files, because
briefs and audiences declare these errors through `commonBriefErrors()` (`design/brief.go`),
a separately maintained helper from connections' `authErrors()`. `Unauthorized` is the arm that matters for `JWTAuth`:
its refusal is returned on every method, including bodyless `GET`s that declare no
`BadRequest` payload at all.

The same file's `TestEveryConnectionUnauthorizedEncoderSetsTheChallenge` covers the second
half of the contract — that each generated `Unauthorized` arm emits the
`WWW-Authenticate: Bearer` challenge header, not merely the 401 status, since RFC 9110
requires the challenge on a 401 and only the generated encoder can put it on the wire.
`commonBriefErrors()`/`briefErrorResponses()` take no arguments for the same reason: an
earlier `withBadRequest bool` that had stopped gating anything was removed rather than kept
for call-site readability, since a parameter that could be passed `false` is a way back to
the defect.

**Adding a method to a service design is not a separable change.** `make apigen`
(`Makefile:63-68`) puts the new method on the generated `<service>.Service` interface,
and each implementation asserts satisfaction at compile time — `internal/service/brief.go:48`
declares `var _ briefs.Service = (*BriefService)(nil)`. A commit that lands DSL without the
corresponding handler therefore does not build; there is no "design-only" PR for a new method.
`make apigen` is also the only correct generator here: `goa gen` alone leaves the ko-embedded
OpenAPI copies under `cmd/campaign-service/kodata/gen/http/` stale, and `cmd/okfgen` regenerates
the knowledge bundle, not Goa output.

**An optional attribute generates a pointer field, and optional means ABSENT — not `null`.**
Marking an always-present response field optional does not merely mis-document it in OpenAPI: the
generated struct field is `*T` and a handler assigning a plain value does not compile. Only
values the service genuinely omits should stay optional. The same holds on the request side — an
optional request attribute is `*T`, so the domain type it converts into must be a pointer too, or
the "omitted" and "explicitly zero" cases collapse into one.

The distinction between *absent* and *null* is the part that reaches consumers. Goa generates the
HTTP field with `omitempty` (`gen/http/lfx_v2_campaign_service_briefs/server/types.go:225`), so a
nil pointer drops the property from the JSON object entirely; it never serialises as
`"field": null`, and Goa's DSL has no way to model "present with a null value". A UI contract
written as `field: T | null` is therefore a DIFFERENT shape from what an optional attribute
produces, and the gap has to be closed deliberately — normally by coalescing at the client
boundary (`value ?? null`), since `undefined ?? null` is already `null` and the rendered result
is identical. Say which one the endpoint means in its own documentation; do not leave the first
consumer to infer it from a missing key.

`event-details` is a NAMED type rather than `Any`, and the reason generalises: `Any`
renders as `{}` in the generated OpenAPI, so every generated client returns an untyped
value and no consumer can discover or validate the shape. `extracted_from` is its only
required attribute — a record that cannot say which strategy produced it is not worth
returning — and the rest are optional so an absent field arrives as a nil pointer rather
than `""`, which is the distinction a form being pre-filled actually needs.

`fetch-event-url` is `POST` despite creating nothing. The URL is the reason: as a query
parameter it lands verbatim in access logs, proxy logs and browser history at every hop;
as a body parameter it does not. That outweighs the idempotency `GET` would advertise.

`upload-creative-asset` (`POST .../briefs/{brief_id}/creative-assets`, LFXV2-3295) is the
briefs service's image-upload method, backing the Meta single-image creative. It is
SYNCHRONOUS — unlike `create-campaigns`, which returns a job — because it only validates and
stores bytes and touches no ad platform. The `bytes` attribute is a base64 **`String`**, not Goa's
`Bytes` type, and the reason is that the published contract has to describe the wire. Goa emits a
`Bytes` attribute as `type: string, format: binary`, which in OAS3 means RAW OCTETS — while an
`application/json` body can only carry base64 — so a strict generator built a client this server
cannot decode. That is unconditional in the generator (`goa v3.25.3`
`http/codegen/openapi/v3/types.go:179-180`), `Format()` is validated against a whitelist with no
byte/base64 member, and `openapi:extension:` meta cannot set `format`; so the field TYPE changes
rather than the generator. It publishes `type: string` with a description and a base64 example.
The media type is unchanged — still `application/json`, no multipart machinery.

`MinLength(1)`/`MaxLength(41943040)` put the accepted size in the OpenAPI document and the
generated validator applies them before the handler runs. They bound the ENCODED string in
CHARACTERS, which is both what OpenAPI `maxLength` means and what the validator now compares —
those were different quantities under `Bytes`, where goa applied the encoded figure to the
decoded slice. `MinLength(1)` rejects an empty upload.

The service decodes the string at the boundary (`base64.StdEncoding`, padded), and malformed
base64 is a **400**, never a panic or a 500. The 30-MiB DECODED ceiling at Meta's documented
single-image maximum is then enforced on `len(decoded)` in the handler (`maxCreativeStoredBytes`)
and, for writers that never reach the handler, as a table CHECK on `byte_size`
(migration `000029`). The encoded and decoded ceilings agree by construction:
`base64.EncodedLen(31457280) == 41943040`. `MaxLength` does not bound the wire either: the
validator sees the string only after the JSON decoder has read the entire body, so it alone
leaves the server buffering whatever a caller chooses to send. The inbound bound is
`constants.MaxRequestBodyBytes` (42 MiB), applied by `middleware.MaxBodyBytes` across every route
and sized from this ceiling: base64 expands by 4/3, so a maximum-size 30-MiB image is 40 MiB of
base64 exactly, plus the JSON envelope — which is why the cap is not 40 MiB, and why raising
`MaxLength` requires raising it in step. Every brief and connection method also declares a
`PayloadTooLarge` error mapped to `413`. No handler returns it — `middleware.MaxBodyBytes` sits
outside the mux and never reaches a Goa encoder — but declaring it is what gives the generated
CLIENT a decode case (without it an ordinary oversized upload surfaces as `ErrInvalidResponse`,
an unknown-status failure) and what puts the status in the OpenAPI documents. It is declared on
every method, not just the body-bearing ones, because the cap is global middleware.

`content_type` is an `Enum("image/png", "image/jpeg")`, but
the enum only constrains the DECLARED value — the handler re-sniffs the bytes and stores the
verified type (see [internal/service](internal-service.md)). It responds `201` when THIS request
stored the asset and `200` when an identical upload already existed and the stored row was
returned unchanged — the upload is idempotent on `(brief_id, checksum)`, and an unconditional
`201` told a retrying client it had created something when nothing was created. The two statuses
are selected by the `created` attribute via `Response(StatusCreated, Tag("created","true"))` plus
a default `Response(StatusOK)`; both share one body shape. The 201 arm also declares
`Body(CreativeAsset)` explicitly: since goa v3.31 an implicit response body drops the attribute its
`Tag` tests, which would silently remove `created` from the 201 body; an explicit `Body` is used as
declared. The `CreativeAsset` result type carries OPTIONAL `width`/`height` attributes, each
`Minimum(1)` and deliberately absent from `Required` (LFXV2-2665): an asset stored before the
service recorded dimensions has none, and `Required` would make those rows unserializable.
Absent means "dimensions unknown" — a published `0` would be read as a real measurement failing
every slot minimum, which is why `Minimum(1)` refuses it at the schema too. Neither carries an
ETag: creative
assets are insert-only and carry no version, so there is no optimistic-concurrency
handle to return (contrast the campaign/audience updates that require `If-Match`). `project_id`
uses the permissive UUID-or-slug attribute, not the slug-only one `create-campaigns` needs,
because the asset is bound to a campaign later by its own id and the id is never stamped into a
campaign name.

`list-creative-assets`, `get-creative-asset` and `delete-creative-asset` (LFXV2-2665) complete
that surface: before this, upload was the only creative-asset method, so a brief's library was
write-only — nothing could enumerate it, and nothing could free its bytes. A folder upload is
the use case that makes those gaps blocking, because it is a SEQUENCE of requests rather than
one.

- **The list result is a `CreativeAssetList`, not a bare array.** Alongside `assets` it carries
  `count`/`count_limit` and `bytes_used`/`bytes_limit`, and all five are REQUIRED. Publishing the
  ceilings beside the totals is what lets a client decide before it starts sending: with only the
  rows, a caller discovers a full brief by sending the file that gets refused, having already
  paid for every round trip before it. Making them required is what separates "the library is
  empty" from "the server declined to compute a total" — "0 of 100 used" is exactly the state a
  folder picker has to render correctly. The method takes no limit or cursor: a brief stored
  before the quota existed may hold more than `count_limit`, and its full set is precisely what
  an operator must see in order to delete back down to it.
- **`get-creative-asset` exists separately from a list filter** because it is the metadata read
  the signed-asset route does not provide — that route serves bytes, this one answers "what shape
  is this asset". `created` is absent here, as it is everywhere but upload: its only job is
  selecting 201 versus 200 on that one method.
- **Delete is a `204` with no body, and HARD.** Campaigns and briefs retire by a status flip;
  this row is removed, because reclaiming the storage against the quota is the entire reason the
  endpoint exists. It is refused `409` while any live campaign on the brief could still need the
  asset, which includes a campaign whose configuration has not been recorded yet (see
  [internal/infrastructure/postgres](internal-infrastructure-postgres.md) for why that arm is
  exact rather than a hedge).
- **No new error type.** Both refusals are the `Conflict`/`ConflictError` that
  `commonBriefErrors()` already declares, separated by the `reason` discriminator —
  `creative_asset_quota_exceeded` and `creative_asset_in_use` — the same mechanism
  `ab_test_unsupported_send_type` uses, and for the same reason: a folder upload sending many
  files has to branch on WHICH refusal it got (stop and prompt the user to free space, versus
  skip this one file), and the two are indistinguishable from prose.

Like upload, none of the three carries an ETag, for the reason given above.

`get-asset-plan` (LFXV2-2665) is the fifth method on that surface and the one that answers the
operator's actual question: not "what have I stored" but **"can this brief launch, and if not,
what is missing?"** It is `GET .../creative-assets/plan` — a PURE READ that stores nothing and
attaches nothing, so an operator may call it as often as they like while still deciding, and
the override they eventually send replaces a row in the plan rather than racing it.

- **`platforms` is a REPEATABLE query parameter**, not a comma-joined list: a platform
  identifier is opaque, and a separator inside one would need an escaping rule nothing else in
  this API has. Absent means every platform whose slot rules the service publishes, which is
  the answer "is my library ready" wants. A platform the service does not know is **ignored,
  not refused** — see [internal/service](internal-service.md) — and the echoed `platforms`
  field is what makes the drop visible.
- **The result is four lists and all of them are REQUIRED**: `assignments`, `gaps`,
  `unclassified` and `unused`. Empty is a real answer here — no gaps is precisely the state
  that means ready to launch — so each must serialize as `[]` rather than `null`.
- **`creative-slot-gap` makes `slot_key` OPTIONAL while `label` is required.** A requirement
  can span several slots (Google needs Demand Gen's landscape marketing image *or* its square
  one), and such a gap belongs to no single slot; the label names the requirement instead.
  Every other gap field — platform, need, reason — is required, because a gap with no stated
  remedy is not worth returning.
- **`requirement` travels with a FULL slot too**, not only a gap. The operator's next question
  after "what fits here" is "what else would", and re-deriving that sentence client-side would
  mean keeping a second copy of every platform's image geometry in the browser.
- **No ETag**, and for a different reason than upload's: the plan is DERIVED and holds no
  version of its own. It changes when the library changes, and the library's own methods are
  where a concurrency guard would belong.

## The audience-builder service (LFXV2-2770)

`audience_builder.go` declares a service of its own — thirteen methods under
`/projects/{project_id}/audience-builder/…`, every one carrying `bearerToken()` and the
project-id attribute, all behind `JWTAuth`. It is a separate Goa service rather than more methods
on the brief surface because nothing here is subordinate to a brief: an operator explores an
audience BEFORE any brief commits to one.

Three declarations are load-bearing:

- `search-audience-lists` requires `q` with `MinLength(1)` and `preview-audience-count` requires
  `list_ids` with `MinLength(1)`. An empty search term matches everything in the portal and an
  empty id set previews a count of nothing — both are caller mistakes worth a 400 rather than a
  plausible-looking answer.
- `get-audience-last-sent` caps `limit` at `Max(10)` with a default of 3. The endpoint fans out
  per email against a rate-limited API, so the ceiling is a budget, not a preference. The SEND
  DATE the rows are ordered by is projected onto the list rows the single sweep already reads
  (`publishDate` in `includedProperties`), but SELECTION does not depend on that projection
  arriving: the authoritative date is read for a shortlist of `limit + 12` rows (capped at 22), and only
  those survivors pay the expensive `listBriefs` fan-out, which stays at `limit`. Reading the date
  for `limit` rows only would have discarded the newest send on the strength of an edit timestamp,
  and a row already cut cannot be recovered by re-sorting. The sweep itself is now one walk rather
  than one per search term, so the search half of the budget HALVED, and an `event_name` with no
  searchable tokens is answered without a single request. See
  [internal/dispatch](internal-dispatch.md).
- `compose-audience-master` responds `201` and declares a second error, `ComposePartial`, mapped
  to `500`. Goa maps both it and `InternalServerError` to that status and discriminates with a
  `goa-error` header, so the ComposePartial body carries the created suppression list where the
  internal error carries only code and message — the body is what a proxying caller can actually
  read. See [internal/service](internal-service.md).

`audience-compose-master-input` carries an optional `brief_id`, and supplying it makes the same
call ALSO record the composed master as that brief's built audience — `recorded` (Required) and an
optional slim `audience` object report that half. `recorded` is explicit so no caller infers
attachment from an absent object, and the slim type deliberately omits `built_in_portal_id`: the
portal is provenance the service stamps, not a field a client has any use for. `ComposePartial`
gained a fifth shape, `master`, for the case where both lists exist and only the attachment failed.

`audience-compose-master-input` deliberately has NO `event_url`. Composition works from the lists
and names an operator has already reviewed; re-fetching the page at write time would let a page
edited since discovery change what gets created.

There is no streaming method: Goa v3 has no SSE encoding, so `discover-audience-lists` is a plain
synchronous POST and any progress reporting belongs to the caller.

## Account ids on connection configs carry a `Pattern` (LFXV2-2665)

`connection.go`'s per-provider config types declare a `Pattern` and a `MaxLength` on every id the
operator can set — Google Ads `account_id` and `login_customer_id` (`^([0-9]+)?$`), Reddit
`account_id` (`^[A-Za-z0-9_]+$`), Microsoft `account_id` (`^[1-9][0-9]{0,17}$`) and `customer_id`
(`^([1-9][0-9]{0,17})?$`, both `MaxLength(18)` — eighteen digits, not nineteen, so that every
value the pattern admits is a valid int64 and the design's rule is a subset of the runtime's
rather than overlapping it). `Required` alone checks only that the KEY is present, so
`{"account_id": ""}` was storable on an active connection, and Google's dashed UI form
`866-674-6580` — which no Google Ads API response can ever contain — was storable too.

Each pattern is the design-layer mirror of a rule the platform client already enforces at
runtime (`googleads.customerIDRE`, `reddit.accountIDRe`, `microsoft.ValidateAccountID` /
`ValidateCustomerID`), and the runtime checks STAY: Goa validates the HTTP transport, and
non-HTTP callers — bootstrap, migrations, rows written before these patterns landed — bypass it
entirely. Two deliberate asymmetries are worth knowing before "tightening" either side:

- An **empty** value is admitted where `""` is a supported runtime state (Google's
  credentials-first `account_not_selected`, Microsoft's "no customer scoped"), and refused where
  it is not. A pattern stricter than the runtime is the mirror image of the defect being fixed.
- Whitespace-padded and out-of-`int64`-range values diverge from `microsoft.ValidateCustomerID`,
  which trims and parses: the design layer is the stricter outer check for padding, and no regex
  can express the `int64` ceiling, which is why `ParseInt` must stay.
- **HubSpot's `account_id` carries neither**, and it is the exception that states the rule. Every
  bound above exists because that id is interpolated into a request path, query or header, so its
  shape is a transport concern first. HubSpot's is stored on the row and read by nothing — the
  campaign path takes its list id from `hubspotConfig`, and the connection probe authenticates the
  token and compares `portal_id`. With no request for a malformed value to reach, a bound would
  assert a shape this service has no way to know. It earns one when a caller puts it in a request.

`internal/apivalidation` tests the GENERATED validators from outside `gen/` and includes a drift
guard per platform that runs the same ids through the design validator and the platform
validator, asserting they agree except at the named asymmetries.

PUT is a full replace on every provider, and un-selecting an account is expressed by an ABSENT
`account_id` — which is why a strict pattern on an optional attribute does not break clearing.

Optional PROVIDER-CONFIG attributes differ (issue #300): omitting one whose stored value is
non-empty is refused with 400 on PUT, so clearing takes an explicit `""`. Their descriptions end
with the `keepOnUpdate` text, which is how the rule reaches the generated OpenAPI: Meta `app_id`,
Reddit `conversion_pixel_id`, X `as_user_id`, Microsoft `customer_id`, and HubSpot `portal_id`,
`sender_email`, `sender_name` and `brand_kit`. Because an explicit `""` is now the only way to
clear X's `as_user_id`, its Pattern is `^([0-9]+)?$`. Google Ads' `login_customer_id` does not
carry the text because Google Ads is not guarded yet. The update methods already declare
`BadRequest` (400) through `authErrors()`, so no new response was needed.

## What `ok` on a connection test promises (LFXV2-2665)

`TestResult.ok` is described as "the credential authenticated AND the configured account passed
that provider's own check", with an explicit note that **how deep that account check goes is
provider-specific** — including HubSpot, where there is no configured account to check at all
and the token's own portal is the whole of it.

The per-method description is provider-aware for the same reason, through
`testMethodDescription(key, title)`. Six providers get "credential and the configured account";
HubSpot gets "private-app token … no configured account is checked: the portal is the token's
own". A single shared sentence would publish, in `gen/**` and both embedded OpenAPI copies, a
promise `HubSpotDispatcher.ProbeConnection` does not keep — telling an integrator that a green
HubSpot test cleared an identifier nothing ever looked at. That dispatcher logs a `portal_id`
mismatch as a warning and deliberately keeps it out of the verdict, because `portal_id` is not
an account: nothing routes on it, and the portal a campaign lands in is the token's own. The earlier wording — "the configured account is usable" — promised more
than any probe delivers and more than several deliberately intend to: Microsoft and Meta test
membership in an enumeration and knowingly accept accounts the platform reports as suspended,
paused or draft, because those are recoverable states an operator fixes in the platform's UI and
not by re-saving a connection this service stored correctly. Google Ads does read the account's
manager and status fields; Reddit goes further in the other direction and fails a connection
naming no conversion pixel. A generated client cannot flatten that into one promise, so the
contract states the shape and points a caller needing lifecycle state at the account resource.

Narrowing the description rather than making every probe enforce lifecycle usability is the
deliberate half: the alternative changes six probes' behaviour to satisfy a sentence, and
"suspended account" is not something re-testing a connection repairs.

## `monitor-hubspot-account` (LFXV2-2665)

`HubSpotEmailMonitor`, `HubSpotEmailMonitorEmail` and `HubSpotEmailMonitorTotals` carry
TYPE-LEVEL examples built from shared helpers (`hubspotMonitorEmailExample` and friends) so the
row, totals and envelope examples describe the same send and the published action item is the
one the rules raise for it; the `emails` and `action_items` attributes carry their own examples
so Goa does not repeat an item. `TestPublishedHubSpotMonitorExamplesArePossible` walks all four
generated specs. No cost attribute exists on any of the three types.

See [design](../../../design).

## Meta audience examples are type-level (LFXV2-2665)

`MetaAdsAudienceBucket` and `MetaAdsAudience` (`design/brief.go`) carry TYPE-LEVEL `Example()`s,
built from the shared `metaAudienceAgeGenderExample` / `metaAudiencePlacementExample` maps. With
attribute examples alone Goa composed an impossible object: a bucket carrying age, gender AND
placement values at once, beside an envelope whose `bucket_count` (24) disagreed with its two-item
`buckets`. Each bucket example now carries only its own dimension's value fields, and the envelope's
`bucket_count` equals `len(buckets)`.

Type-level examples are not enough on their own: an ARRAY property gets its own generated
example, which Goa fills by repeating the element type's single example. So the `buckets`
attribute carries its own `Example()` too (#285): one age_gender bucket then one placement bucket.
Three identical placement buckets cannot occur, because rows are merged per segment and age_gender
buckets are listed first.

The same fix applies to `AudienceLastSentEmail` (`design/audience_builder.go`, #285). Its composed
example published populated `included_lists`/`suppression_lists` beside `lists_unavailable: true`,
which the type defines as "both arrays empty because unknown". It now has a type-level example
(a readable selection, `lists_unavailable: false`), and both list attributes carry resolved-list
examples (`missing: false`) via the shared `audienceIncludedListsExample` /
`audienceSuppressionListsExample`. This is an example-only change, with no behaviour change.

`AudienceListBrief` and `AudiencePreviewCount` themselves now carry TYPE-LEVEL examples too (#289
review): wherever they appear without an attribute-level example, Goa's synthesised one paired
`missing: true` with a populated `name`/`hubspot_url`, and `exact: false` with a `count` that was
neither `estimate` nor 0 — both impossible per their descriptions. The list brief uses the shared
resolved-list example; the preview count is `ExactPreviewCount`'s shape (`exact: true`, count ≤
estimate, empty reason). `internal/service/audience_builder_examples_spec_test.go` walks every
example in the four generated JSON specs and checks both invariants. Examples only; no behaviour
change.

`internal/service/meta_audience_wire_example_test.go` walks EVERY `example` in all four GENERATED
specs (v2 and v3, `gen/` and the kodata copy), where the defects lived. It checks the following:

- every Meta bucket carries only its own dimension's fields;
- every buckets array lists age_gender before placement and never repeats a segment;
- every envelope's `bucket_count` equals `len(buckets)`;
- no `lists_unavailable: true` appears beside a populated list.

It fails if the walk finds no such example, so a renamed field cannot pass it vacuously.

## Reddit audience read (LFXV2-2665)

`get-reddit-ads-audience` (`design/connection.go`) returns `RedditAdsAudience`
(`design/brief.go`): `window` (`redditAudienceWindowEnum` — the five windows the Reddit report client
maps), `buckets` of `RedditAdsAudienceBucket` (`dimension` gender | country | community, optional
`value` with the client's charset as `Pattern`, counters, `ctr`) and `bucket_count`. The bucket and
envelope carry TYPE-LEVEL examples built from shared maps, as the Meta and X types do, so no
published example is a response the endpoint cannot return.

## Reddit keyword performance read (LFXV2-2665)

`get-reddit-ads-keywords` returns `RedditAdsKeywords` (`window` on `redditAudienceWindowEnum`,
`rows` of `RedditAdsKeyword`, `row_count`, `truncated`). The row keeps the Google/Microsoft keyword
counters (`impressions`, `clicks`, `cost_micros`, `ctr`) and `campaign_id`, with optional `text`
(`MaxLength(200)`) and no criterion handles — Reddit reports none, so the row is read-only. Both
types carry type-level examples from one shared map.
