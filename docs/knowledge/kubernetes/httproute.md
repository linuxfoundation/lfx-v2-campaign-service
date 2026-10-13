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
`jobs`, `google-ads/keywords|audience|campaign-ref`, `microsoft-ads/keywords|audience|campaign-ref`, `(meta-ads|reddit-ads|twitter-ads)/audience`, `reddit-ads/keywords`, `(meta-ads|reddit-ads|twitter-ads)/campaign-ref`, `hubspot`,
`audience-builder`)
sits *after* the variable `{projectId}` — which a `PathPrefix`/`Exact` match cannot
reach past.

**There is no `{provider}/metrics` segment.** The regex used to admit
`(google-ads|linkedin-ads|meta-ads|reddit-ads|twitter-ads)/metrics`, and the RuleSet
authorized the same five paths, but no `design/` file has ever declared the route, so
those requests were forwarded to a service that 404s them. The alternative was removed
from the regex and the five RuleSet entries together (2026-10-13), and `parity_test.go`
pins all five as REJECTED by both matchers, so re-adding the segment on one side — or on
both without a handler — is a deliberate edit rather than drift. Metrics are served per
campaign and per brief (under `briefs/`) and per account (`connection-*/account-monitor`);
see the Monitoring section of `docs/api-catalog.md`. `parity_test` still cannot by itself
tell a served path from an unserved one — it compares the regex to the RuleSet and reads
neither `design/` nor `gen/` — so a new alternative must be checked against `design/` by hand.

**The `connection-*` family is spelled out as TWO alternation branches**, not one. All
seven providers share `/test` and `/set-credential`; what differs is the extra ruled
sub-path each carries:

- `connection-(google-ads|meta-ads|linkedin-ads|microsoft-ads|twitter-ads|reddit-ads)` add
  **`/accounts`** — ad-account discovery (google-ads under LFXV2-2023, meta-ads under
  LFXV2-3062, linkedin-ads and microsoft-ads under LFXV2-3064, twitter-ads under
  LFXV2-3319, reddit-ads under LFXV2-2665) — and **`/account-monitor`** — the per-account
  monitoring/pacing endpoint (LFXV2-2665). google-ads, meta-ads, linkedin-ads and
  reddit-ads implement it via the orchestrator's optional `AccountMetricsReader`
  capability; microsoft-ads and twitter-ads via the report-backed `AccountReportReader`,
  because their metrics come from asynchronous platform reports (Microsoft's Reporting
  service, X's stats jobs). twitter-ads joined this branch with its monitor; reddit-ads
  joined it when it gained discovery (LFXV2-2665) — until then it was a third branch
  carrying `/account-monitor` only.
- `connection-hubspot` adds **`/emails`** — marketing-email search (LFXV2-3197) —
  **`/campaigns`** — campaign UTM lookup and create (LFXV2-2641) — and
  **`/account-monitor`** — the email account monitor (LFXV2-2665), which shares the ad
  monitors' path but is project-scoped (the emails this service recorded, no `account_id`).
  NOT `/accounts`: a HubSpot connection is already scoped to the portal its token
  authenticates against, so there is no account to discover. What the caller picks is which
  marketing email a campaign clones, and which existing HubSpot campaign owns the UTM token.

Folding these together would admit `/accounts` for hubspot and `/emails` for
google-ads, neither of which is served — and a path the
RuleSet does not rule is a route/rule parity violation, which is what `parity_test` exists
to catch. It has both positive and negative rows for this reason: a widened alternation
passes every positive test.

As each further provider gains a sub-path, add it to the branch that matches its SHAPE
rather than widening a shared one. Collapsing branches back together is correct only once
the providers in them carry the same sub-paths.

**The `audience-builder` family is enumerated leaf by leaf**, not admitted with a free
`(/.*)?` tail (LFXV2-2770). It is a SIBLING of `briefs`, so it inherits nothing from the
`briefs(/.*)?` branch and needs its own alternation branch here plus its own RuleSet
entries. Nine leaves are spelled out — `capabilities`, `discover`, `lists/search`,
`suppression-lists`, `last-sent`, `existing-master-lists`, `preview-count`,
`compose-master`, `qa/run` — for two reasons. `compose-master` creates real contact lists
in a production HubSpot portal and is not idempotent, so routing a path the service does
not serve is worse here than for a read-only family: the base path serves nothing, and
`POST /signal-list` is specified in the plan but deliberately unimplemented. And two of
the leaves are two segments deep (`lists/search`, `qa/run`), so a single-segment
placeholder would not have covered the family anyway. Adding an endpoint to this service
therefore means editing this regex, `ruleset.yaml`, and `parity_test.go` in the same
commit.

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
placeholder prefixes (OpenAPI docs + not-yet-built endpoints). The `/_campaigns/` prefix also
forwards the public signed creative-asset route,
`/_campaigns/creative-assets/{projectId}/{briefId}/{assetId}` (LFXV2-2665), which Reddit fetches
anonymously; it needs no branch of its own, and the RuleSet's anonymous
`signed-creative-asset:get` rule is what admits it.

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
catches a ONE-SIDED matcher edit — e.g. adding `tiktok-ads/campaign-ref` to only the
route regex yields the witness `/projects/x/tiktok-ads/campaign-ref`, which matches the
route but no rule, failing the build. Heimdall is default-deny (a request matching
no rule is REJECTED), so such drift makes the routed endpoint UNREACHABLE through
the gateway — not an unauthenticated bypass; the parity test catches it before it
ships either way. (The test skips when `helm` is absent but fails on a render error.)

Path extraction is SCOPED to the `project-api` rule block (not "any `/projects/` path
in the RuleSet"), and a separate `TestProjectAPIRuleEnforcesCampaignManager` asserts
that rule has two `openfga_check` authorizers on relation `campaign_manager`, each
paired with the correct object: `project:{{- .Outputs.project_slug_resolver_contextualizer.uid -}}`
for the slug branch, `project:{{- .Request.URL.Captures.projectId -}}` for the UUID
branch. This matters because the invariant is not merely "some rule matches" but "the
campaign_manager rule matches, with each branch's object correctly paired to its
guard": a path moved into an `allow_all` / `deny_all` / differently-scoped rule, a
downgrade of the rule's relation, or a swapped guard/object pairing, must FAIL the
security regression test rather than silently satisfy path parity.

The Meta ad-set read and pause/resume (`campaigns/{id}/meta-ad-sets`, `campaigns/{id}/meta-ad-sets/{adSetId}/status`, LFXV2-2665) are routed and ruled by the `briefs` family with no entry of their own; `parity_test.go` pins both paths.
