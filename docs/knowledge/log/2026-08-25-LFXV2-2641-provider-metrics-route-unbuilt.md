# 2026-08-25 — LFXV2-2641 the provider metrics route is documented but unbuilt

**Fix** — `docs/api-catalog.md` documented `GET /projects/{projectId}/{provider}/metrics` as a
live endpoint, with an FGA relation, a `days` parameter and a full `CampaignMonitorResponse`
body. It is declared in no `design/` file, appears in no `gen/` mux, and exists on no branch.
The row is now marked as a design sketch rather than deleted, so the intent survives without
reading as a contract.

**The ticket was closed on the reads that DID ship.** `aef68385` states verbatim: "Completes
LFXV2-2641. The ticket's metrics endpoints and status toggle already shipped; this adds the last
three." The metrics endpoints that had shipped are the brief- and campaign-scoped reads
(`GET .../briefs/{briefId}/metrics`, `GET .../campaigns/{id}/metrics`), not this project+provider
route. Nothing was lost — the claim was simply about a different pair of endpoints than the row
this note corrects.

**Two contradictions are recorded for whoever builds it, because both would have been inherited
silently.** The row specified `days`, default 14; every shipped metrics endpoint takes the closed
`metricsWindowEnum`, and per-platform defaults override it (`defaultMetricsWindowFor` caps X Ads
at 7 days, because X's API refuses wider ranges). And `CampaignMonitorResponse` exists in no Go
file: its `accountTotals` is coherent only because it is scoped to ONE provider and therefore one
account in one currency, which is exactly why `BriefMetrics` carries no cross-channel cost total —
this service performs no FX conversion. `BriefMetrics` rows also never zero-fill a campaign that
could not be read, each carrying its own `status`, so a total computed over rows without checking
status would silently understate.

**The chart already routes and authorizes the unbuilt paths.**
`templates/ruleset.yaml` grants `campaign_manager` on
`/projects/:projectId/<provider>/metrics` for all five ad providers, and the
HTTPRoute regex admits the same segment. A request reaching them is authorized
by Heimdall and forwarded to a service that serves no such route.
`parity_test.go` cannot catch this: it checks the RuleSet against the HTTPRoute
regex and reads neither `design/` nor `gen/`, so chart routes are never compared
against implemented endpoints. That gap is recorded here and in the catalog, and
deliberately NOT fixed in this change — implementing the route and withdrawing
the five chart entries are opposite decisions, and the choice is not a docs one.
