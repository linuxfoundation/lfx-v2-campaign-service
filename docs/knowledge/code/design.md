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
job to poll), and campaign read/update. The audiences service (`design/audience.go`,
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
`JWTAuth`, which can reject any request with a `BadRequest` (400) — so every brief
and audience method declares `BadRequest` regardless of whether it accepts a body.
The binding `platforms` selection is constrained to the known provider enum.

**Adding a method to a service design is not a separable change.** `make apigen`
(`Makefile:63-68`) puts the new method on the generated `<service>.Service` interface,
and each implementation asserts satisfaction at compile time — `internal/service/brief.go:48`
declares `var _ briefs.Service = (*BriefService)(nil)`. A commit that lands DSL without the
corresponding handler therefore does not build; there is no "design-only" PR for a new method.
`make apigen` is also the only correct generator here: `goa gen` alone leaves the ko-embedded
OpenAPI copies under `cmd/campaign-service/kodata/gen/http/` stale, and `cmd/okfgen` regenerates
the knowledge bundle, not Goa output.

**An optional attribute generates a pointer field.** Marking an always-present response field
optional does not merely mis-document it in OpenAPI: the generated struct field is `*T` and a
handler assigning a plain value does not compile. Only genuinely nullable values should stay
optional. The same holds on the request side — an optional request attribute is `*T`, so the
domain type it converts into must be a pointer too, or the "omitted" and "explicitly zero"
cases collapse into one.

See [design](../../../design).
