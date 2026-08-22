---
type: "Kubernetes Resource"
title: "RuleSet"
description: "Kubernetes RuleSet manifest for the campaign service, defined in the Helm chart."
resource: "charts/lfx-v2-campaign-service/templates/ruleset.yaml"
---

# RuleSet

Kubernetes RuleSet manifest for the campaign service, defined in the Helm chart.

See [charts/lfx-v2-campaign-service/templates/ruleset.yaml](../../../charts/lfx-v2-campaign-service/templates/ruleset.yaml).

## Rules

Rendered only when `heimdall.enabled`. Three rules, one per routed path group (the
chart↔route parity invariant — see [httproute.md](httproute.md)):

1. **`openapi:get`** — `/_campaigns/openapi.*` docs are publicly readable
   (`oidc` + `anonymous_authenticator` → `allow_all` → `create_jwt`).
2. **`project-api`** — every project-nested endpoint (`connection-*` — including two
   provider-specific sub-paths that are ruled by their own entries rather than by the
   shared `connection-*` family: `/accounts` on each provider whose dispatcher implements
   `AccountLister` — google-ads, meta-ads, linkedin-ads, microsoft-ads and twitter-ads
   (ad-account discovery; linkedin-ads and microsoft-ads added under LFXV2-3064,
   twitter-ads under LFXV2-3319) — and
   `connection-hubspot/emails` (marketing-email search, LFXV2-3197). The HTTPRoute
   regex spells out THREE branches for the same reason — the `AccountLister` providers
   with `accounts`, hubspot with `emails`, and the providers with neither (reddit-ads,
   whose client has no `ListAdAccounts`) —
   because folding them into one alternation would rule `/accounts` for hubspot and
   `/emails` for google-ads, neither of which is served. `parity_test` fails if the
   RuleSet and the regex ever disagree, in either direction —
   `briefs` [+ nested campaigns], `jobs`, `{provider}/metrics` for the five ad
   providers, `google-ads/keywords|audience`, `hubspot`). Gated on the project
   `campaign_manager` relation (D2 — reads AND writes; no read-only audience),
   scoped to `project:{uid}` where `{uid}` is resolved from the URL-captured
   `:projectId` slug (see "Slug-to-UID resolution" below). A single rule covers
   all families because they share the identical authorization.
3. **`campaigns-placeholder:deny`** — the reserved `/campaigns`, `/campaigns/*`,
   and non-openapi `/_campaigns/*` paths are routed through Heimdall but are not
   real endpoints yet, so they **fail closed** with `deny_all`.

## Authenticator pairing

The `project-api` rule pairs `oidc` with `anonymous_authenticator`: `oidc` alone
would reject a credential-less request *before* OpenFGA runs, and it is
`openfga_check` that actually rejects the anonymous subject (committee-service
pattern). The pairing also lets the `openfga.enabled=false` branch fall back to
`allow_all` for local dev. The `deny_all` placeholder rule intentionally omits
`anonymous_authenticator` (a valid token is required, then everything is rejected).

## Slug-to-UID resolution (LFXV2-3324)

`:projectId` on every `project-api` route is usually a project **slug** —
self-serve's `campaign-service.service.ts` sends slugs on this API, never
UIDs — but OpenFGA tuples for `campaign_manager`/`marketing_ops` are keyed on
project **UID**. A slug-keyed `openfga_check` object would never match a
UID-keyed tuple.

`design/connection.go`'s `projectIDAttr()` also intentionally accepts a raw
project **UUID** on this rule's get/update/delete/test/set-credential routes,
to keep historical UUID-keyed connection rows (migration 000003) reachable.
`project_slug_resolver_contextualizer`'s endpoint only performs slug lookup
and 404s on a UUID, so the rule must not run it unconditionally.

When `openfga.enabled`, the rule branches on whether `:projectId` matches a
UUID regex via Heimdall's `if:` CEL guard, evaluated against
`Request.URL.Captures.projectId`:

- **Not a UUID (slug):** runs `project_slug_resolver_contextualizer` (defined
  centrally in `lfx-v2-helm`'s `charts/lfx-platform/values.yaml`), which calls
  `lfx-v2-project-service`'s `GET /projects/slug-to-uid/{slug}` endpoint, then
  `openfga_check` reads the resolved `.Outputs.project_slug_resolver_contextualizer.uid`.
  The contextualizer's `continue_pipeline_on_error: false` means a failed
  resolution denies the request rather than letting `openfga_check` run
  against an unresolved/empty object.
- **UUID:** the resolver contextualizer is skipped entirely (its own `if:`
  guard is the negation of the branch condition), and a second, mutually
  exclusive `openfga_check` entry — gated by the positive UUID match — reads
  the raw `:projectId` capture directly as the object UID.

Two separate `openfga_check` entries (one per branch), rather than one entry
whose `object` template falls back to the raw capture when the resolver's
output is empty, because whether a skipped contextualizer's `Outputs` are
safely absent vs. an error is not documented Heimdall behavior — the
mutually-exclusive `if:` guards avoid relying on it.
