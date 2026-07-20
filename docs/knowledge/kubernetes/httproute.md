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
reach past. The route therefore uses a **`RegularExpression` path match** (Traefik
Gateway API "custom" conformance) selecting exactly this service's project-nested
subpaths; `project-service`'s `/projects/` routes are unaffected because Traefik
resolves overlap by match specificity.

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
matchers — so a future edit to one matcher that isn't mirrored in the other fails
the build rather than silently opening an unauthenticated bypass. (The test skips
when `helm` is absent but fails on a render error.)
