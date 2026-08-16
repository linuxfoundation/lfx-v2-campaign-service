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
   `AccountLister` — google-ads, meta-ads, linkedin-ads and microsoft-ads (ad-account
   discovery; the last two added under LFXV2-3064) — and
   `connection-hubspot/emails` (marketing-email search, LFXV2-3197). The HTTPRoute
   regex spells out THREE branches for the same reason — the `AccountLister` providers
   with `accounts`, hubspot with `emails`, and the providers with neither (reddit-ads
   and twitter-ads, whose clients have no `ListAdAccounts`) —
   because folding them into one alternation would rule `/accounts` for hubspot and
   `/emails` for google-ads, neither of which is served. `parity_test` fails if the
   RuleSet and the regex ever disagree, in either direction —
   `briefs` [+ nested campaigns], `jobs`, `{provider}/metrics` for the five ad
   providers, `google-ads/keywords|audience`, `hubspot`). Gated on the project
   `campaign_manager` relation (D2 — reads AND writes; no read-only audience),
   scoped to `project:{projectId}` captured from the URL. A single rule covers all
   families because they share the identical authorization.
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
