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
job to poll), campaign read/update, and live campaign metrics reads (a pure
read from the ad platform, never persisted — unlike campaign updates, which
modify the stored row and require If-Match). The audiences service (`design/audience.go`,
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
stores bytes and touches no ad platform. The `bytes` attribute is Goa's `Bytes` type (`[]byte`
in Go, a base64 string in the JSON body): the transport choice is Goa-native with no multipart
machinery, and `MinLength(1)`/`MaxLength(41943040)` put the accepted size in the OpenAPI
document and the generated validator applies them before the handler runs — `MinLength(1)`
rejects an empty upload and `MaxLength` is the ENCODED ceiling, `base64.EncodedLen` of the 30 MiB
stored-file limit, because OpenAPI `maxLength` counts characters of the JSON string. The 30-MiB
decoded ceiling at Meta's documented single-image maximum is enforced in the handler
(`maxCreativeStoredBytes`) and, for writers that never reach the handler, as a table CHECK on
`byte_size` (migration `000029`). `MaxLength` does not bound the wire either: the validator sees the
decoded slice only after the JSON decoder has read the entire body and base64-decoded it, so it
alone leaves the server buffering whatever a caller chooses to send. The inbound bound is
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
a default `Response(StatusOK)`; both share one body shape. Neither carries an ETag: creative
assets are insert-only and carry no version, so there is no optimistic-concurrency
handle to return (contrast the campaign/audience updates that require `If-Match`). `project_id`
uses the permissive UUID-or-slug attribute, not the slug-only one `create-campaigns` needs,
because the asset is bound to a campaign later by its own id and the id is never stamped into a
campaign name.

See [design](../../../design).
