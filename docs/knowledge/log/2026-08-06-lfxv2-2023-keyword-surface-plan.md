# 2026-08-06 — Keyword surface plan, and four contract facts it got wrong (LFXV2-2023)

**Update** — `docs/plans/keyword-surface.md` records the design for roadmap item 4: two new
brief-service methods (`list-campaign-keywords`, `update-campaign-keywords`) behind a
`KeywordManager` optional dispatcher capability, phased so pause/remove can land without waiting
on PR #69.

The plan itself is uninteresting once implemented. What is worth keeping is the set of repo and
upstream contracts its first draft assumed wrongly, because each is a trap the next planning pass
would fall into identically.

**GAQL field paths are snake_case.** The draft wrote `adGroupCriterion.criterion.keyword` and
`metrics.ctrPercent`, copying the protobuf JSON representation. GAQL v23 rejects camelCase paths
outright, so the query fails before returning a row — and `metrics.ctr_percent` does not exist at
all; `metrics.ctr` is already a fraction. No unit test catches this unless the query text itself
is pinned, which is why one is.

**An ad-group criterion resource name is composite.** It is
`customers/{customer}/adGroupCriteria/{ad_group_id}~{criterion_id}` — the shape
`internal/platform/googleads/adgroup_ad.go` already validates. The customer ID is available
server-side but the ad group ID is not derivable from the criterion ID, so a contract carrying
only `criterion_id` cannot address a criterion. `ad_group_id` belongs on the action payload, not
just the list response.

**`okfgen` is not the Goa generator.** `go run ./cmd/okfgen` regenerates the `docs/knowledge`
bundle and overwrites hand-edited concept files. Goa output comes from `make apigen`
(`Makefile:63-68`), which runs `goa gen` and then re-copies the OpenAPI documents into
`cmd/campaign-service/kodata/gen/http/` — running `goa gen` alone leaves those ko-embedded copies
stale.

**Optional Goa attributes generate pointers.** Leaving an always-present response field optional
does not merely mis-document it in OpenAPI: the generated struct field is `*T`, and a handler
assigning a plain value does not compile. Only genuinely nullable values (a quality score Google
withholds, an unset max CPC) should stay optional. The mirror of this applies to request fields —
an optional `bid_micros` is `*int64` on the wire, so the domain model must be `*int64` too, and
flattening it at the boundary destroys the "omitted vs. explicit zero" distinction the validation
depends on.

Two behavioural gaps found in the same pass, both now written into the plan: criterion IDs
arriving in a request body must be authorized against the campaign before any mutate operation is
built (otherwise a campaign-scoped route lets a caller mutate any campaign under the same
customer), and an aggregate CTR must guard its denominator — a non-empty keyword list can still
total zero impressions, and `x/0` in float arithmetic produces `+Inf`/`NaN`, which is not
encodable as a JSON number.
