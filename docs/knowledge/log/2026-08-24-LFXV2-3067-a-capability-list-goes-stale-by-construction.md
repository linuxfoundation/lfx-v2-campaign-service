# 2026-08-24 — LFXV2-3067 a capability list goes stale by construction

**Docs** — the settings readback reached the code but not the prose that enumerates what a
campaign supports. Five separate hand-maintained lists named metrics, delete and pause and
stopped there, so a reader of any of them learned a capability list that the same branch had
already made wrong: `docs/api-catalog.md` in two places (the adoption motivation sentence and
the "What an adopted campaign supports" callout), `docs/knowledge/code/design.md` (the
briefs-service method rundown, which ended at the metrics read), the adopt-campaign Goa
`Description` in `design/brief.go`, and a fifth site in
`docs/knowledge/code/internal-service.md` that the review had not named.

The failure is structural, not an oversight: a list of capabilities is a copy of a fact that
lives elsewhere, and nothing links the copy to the source, so adding an endpoint silently
falsifies every copy. Four of the five sites are now written to describe the SHAPE instead —
an adopted row is an ordinary campaign row to every per-campaign endpoint, and activation is
the single exception — which stays true when a sixth endpoint is added, because it never
enumerated the endpoints in the first place. The exception is the Goa `Description`: it is
user-facing OpenAPI text where a reader wants the concrete list, so it keeps the enumeration
and was simply completed.

Changing that `Description` is not a docs-only edit. It regenerates the OpenAPI surface, so
`make apigen` must run and the four `cmd/campaign-service/kodata/gen/http/openapi*` copies —
what the deployed pod actually serves — must be confirmed byte-identical to `gen/http/`.
CI does not catch a divergence there.
