# 2026-09-11 — LFXV2-2770: the audience builder's backend moves into this service

**Update** — the nine audience-builder endpoints now live here, as their own Goa service under
`/projects/{project_id}/audience-builder/…`, with the LFX One BFF reduced to a proxy. They had
been implemented end-to-end in that BFF, which put HubSpot list writes, a discovery classifier and
a pre-send QA rulebook in the layer whose job is to shape responses for one UI.

The split across packages follows the shape the work already had rather than the shape the
endpoints have. `internal/audience` gains a second, **exploratory** half beside the record half
that `plan.go` and `filters.go` make up: the classifier, the naming and quarter-ranking rules, the
name-search predicates, the QA checks, the failure vocabulary and the transport-neutral results.
All of it is pure — no HTTP, no credentials, no clock beyond an injected one — which is what lets
the rules be tested without a portal and read without a request in hand.

Orchestration therefore lands in `internal/dispatch` as `AudienceExplorer`, deliberately a
separate type from `AudienceBuilder` despite borrowing its credential resolution.
`AudienceBuilder` materialises the audience a BRIEF has committed to, and its failures fail the
brief. Exploration happens before anything commits, so almost every method degrades to a partial
answer with the gap named: a suppression term that resolved to nothing, a list id no longer in the
portal, a discovery run reported AS capped. `ComposeMaster` is the exception, because it writes.

`internal/service`'s handlers do three things and nothing else — refuse unauthenticated callers,
map neutral results onto generated types, and turn failures into the statuses the contract
declares. The last one is why the layer exists at all: a 404 on a list an operator typed is an
answer they act on, while the same failure reported as a 500 sends them looking for an outage that
does not exist.

Two things did not survive the move. There is no streaming `discover`: Goa v3 has no SSE encoding,
so the endpoint is a synchronous POST and the BFF unpacks one response into the frames its UI
already expects. And the model is used for exactly one thing, extracting the event's identity from
its page — a model that guesses which list to email is a model that can silently mail the wrong
ten thousand people, so classification is entirely deterministic.

The chart gap this exposed is recorded separately in
[[2026-09-11-LFXV2-2770-the-chart-had-no-route-for-it]]; the write path's partial-failure contract
in [[2026-09-11-LFXV2-2770-compose-partial-is-a-body-not-a-header]]; the new HubSpot reads in
[[2026-09-11-LFXV2-2770-membership-truncation-is-half-the-answer]].
