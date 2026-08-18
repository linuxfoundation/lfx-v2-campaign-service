---
type: "Kubernetes Resource"
title: "HTTPRoute"
description: "Kubernetes HTTPRoute manifest for the campaign service, defined in the Helm chart."
resource: "charts/lfx-v2-campaign-service/templates/httproute.yaml"
---

# HTTPRoute

Kubernetes HTTPRoute manifest for the campaign service, defined in the Helm chart.

See [charts/lfx-v2-campaign-service/templates/httproute.yaml](../../../charts/lfx-v2-campaign-service/templates/httproute.yaml).

## Routing

The service serves its API under `/projects/{projectId}/…` (the approved contract —
every endpoint is nested under a project and gated on that project's
`campaign_manager` relation). `project-service` owns `PathPrefix: /projects/`, and
the token that distinguishes a campaign-service path (`connection-*`, `briefs`,
`jobs`, the `{provider}/metrics` segment, `google-ads/keywords|audience`, `hubspot`)
sits *after* the variable `{projectId}` — which a `PathPrefix`/`Exact` match cannot
reach past.

**The `connection-*` family is spelled out as THREE alternation branches**, not one. All
seven providers share `/test` and `/set-credential`; what differs is the extra ruled
sub-path each carries:

- `connection-(google-ads|meta-ads|linkedin-ads|microsoft-ads|twitter-ads)` add
  **`/accounts`** — ad-account discovery (google-ads under LFXV2-2023, meta-ads under
  LFXV2-3062, linkedin-ads and microsoft-ads under LFXV2-3064, twitter-ads under
  LFXV2-3319). Reddit is absent because its client has no `ListAdAccounts` to expose, not
  because the route was skipped.
- `connection-hubspot` adds **`/emails`** — marketing-email search (LFXV2-3197). NOT
  `/accounts`: a HubSpot connection is already scoped to the portal its token
  authenticates against, so there is no account to discover. What the caller picks is
  which marketing email a campaign clones.
- Reddit carries neither. With it now the ONLY member of the shared branch, that branch is
  one ticket away from disappearing — but collapsing it early would admit `/accounts` for a
  provider the service does not serve.

Folding these together would admit `/accounts` for hubspot and `/emails` for google-ads,
neither of which is served — and a path the RuleSet does not rule is a route/rule parity
violation, which is what `parity_test` exists to catch. It has both positive and negative
rows for this reason: a widened alternation passes every positive test.

As each further provider gains a sub-path, add it to the branch that matches its SHAPE
rather than widening a shared one. Collapsing branches back together is correct only once
the providers in them carry the same sub-paths.

The route therefore uses a **`RegularExpression` path match** selecting
exactly this service's project-nested subpaths; `project-service`'s `/projects/`
routes are unaffected because Traefik resolves overlap by match specificity.

`RegularExpression` path matches require **Traefik >= v3.1.0**: its Gateway API
provider translates them into a native `PathRegexp(...)` rule (RE2/Go-regexp flavor)
— verified in Traefik's `buildPathRule` source for every v3.1.0+ tag. On v3.0.x the
match is rejected as "unsupported path match", and the feature is not listed in
Traefik's Gateway API conformance report even though the code implements it. So the
route works on the platform gateway (v3.1.0+), but the render alone doesn't prove it:
after deploy, verify the HTTPRoute's `Accepted` status condition is `True`.

A second rule routes the reserved `/campaigns`, `/campaigns/`, and `/_campaigns/`
placeholder prefixes (OpenAPI docs + not-yet-built endpoints).

## Heimdall parity

When `heimdall.enabled`, both rules attach the `heimdall-forward-body` middleware
(forwardAuth → Heimdall). **Invariant:** every path routed through that middleware
MUST have a matching rule in [ruleset.md](ruleset.md) — a routed request with no
matching Heimdall rule is rejected. The RuleSet's `project-api` rule covers every
routed project-nested family, and its `campaigns-placeholder` rule covers the
reserved prefixes, so chart↔route parity holds.

This parity is enforced by a Go test — `TestRouteRuleSetParity`
(`charts/lfx-v2-campaign-service/parity_test.go`). It renders both templates with
`helm template`, extracts the HTTPRoute's RE2 regex and the RuleSet's project-nested
path patterns (translating Traefik `:projectId`/`*`/`**` tokens to regexps), and
asserts a curated table of accepted/rejected paths matches IDENTICALLY in both
matchers. It also runs a WITNESS check (`TestRouteRuleSetParityWitnesses`) that
couples the assertion to the matchers' own content: it enumerates concrete example
paths from the route regex's AST (via `regexp/syntax`, one witness per alternation
leaf) and requires each to be authorized by a RuleSet entry, and builds a witness
from every RuleSet pattern and requires the route to forward it. This is what
catches a ONE-SIDED matcher edit — e.g. adding `tiktok-ads/metrics` to only the
route regex yields the witness `/projects/x/tiktok-ads/metrics`, which matches the
route but no rule, failing the build. Heimdall is default-deny (a request matching
no rule is REJECTED), so such drift makes the routed endpoint UNREACHABLE through
the gateway — not an unauthenticated bypass; the parity test catches it before it
ships either way. (The test skips when `helm` is absent but fails on a render error.)

Path extraction is SCOPED to the `project-api` rule block (not "any `/projects/` path
in the RuleSet"), and a separate `TestProjectAPIRuleEnforcesCampaignManager` asserts
that rule's authorizer is `openfga_check` with relation `campaign_manager` on object
`project:{projectId}`. This matters because the invariant is not merely "some rule
matches" but "the campaign_manager rule matches": a path moved into an `allow_all` /
`deny_all` / differently-scoped rule, or a downgrade of the rule's relation/object,
must FAIL the security regression test rather than silently satisfy path parity.
