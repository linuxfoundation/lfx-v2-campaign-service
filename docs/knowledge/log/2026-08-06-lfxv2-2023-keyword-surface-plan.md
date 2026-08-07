# 2026-08-06 — Keyword surface plan, and the contract facts it got wrong (LFXV2-2023)

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

**A Goa design PR cannot land without its handlers.** The first breakdown split "design + codegen"
from "handlers" into two PRs. That split does not build: `make apigen` adds the new methods to the
generated `briefs.Service` interface, and `internal/service/brief.go:48` asserts
`var _ briefs.Service = (*BriefService)(nil)` at compile time, so the design-only PR turns `main`
red the moment it merges. This is a property of every service design in this repo, not of
keywords — adding a method to a design and implementing it are one PR.

**`cpc_bid_micros` and `effective_cpc_bid_micros` are not interchangeable.** The effective bid is
what the auction used and stays populated by INHERITING the ad group's bid, so selecting it makes
a criterion-level "max CPC" non-null for every keyword and erases the only distinction the field
carries: has its own bid vs. bids at group level. `ad_group_criterion.cpc_bid_micros` is the
criterion-level bid and is absent exactly when none is set.

**The bulk-mutation rule (`docs/api-catalog.md:19`) needed answering, not citing.** The reserved
`keyword-actions` row existing in the catalog does not by itself override the rule. Of the rule's
two stated reasons, the partial-success one is met by an itemized per-action result array, and the
permission-boundary one — the load-bearing reason — is met *only because* every criterion is
resolved against the one campaign before any operation is built. Without that authorization step
the rule's objection is simply correct. The exception has to be written into the catalog itself in
the implementing PR; an exception argued only in a plan reads later as an oversight.

**One mutate per action was the wrong reflex.** The draft sent a separate HTTP mutate for each
keyword action, justified as preserving per-action attribution because a batched mutate is atomic.
That justification holds only with partial failure OFF. `partialFailure: true` makes Google Ads
apply each operation independently and return a per-operation error at its own index — the same
attribution, one round trip. The serial version bought nothing and cost the documented scale: 100
actions is 100 sequential calls, which does not fit a 30s call budget and burns 100× the quota for
a single user gesture.

**A generated `Description` is API surface.** It is emitted verbatim into OpenAPI, so it has to
name the actions the method actually accepts. The draft's description said "pause, enable, or
change bid" while the Phase 1 enum was `pause`/`remove` — contradicting both the next sentence and
the request schema, and omitting a core operation. The same class of error put "USD" in the
micros field descriptions: Google Ads micros are denominated in the ad account's own currency
(`docs/api-catalog.md:318-321`) and this service does no FX conversion.

Two behavioural gaps found in the same pass, both now written into the plan: criterion IDs
arriving in a request body must be authorized against the campaign before any mutate operation is
built (otherwise a campaign-scoped route lets a caller mutate any campaign under the same
customer), and an aggregate CTR must guard its denominator — a non-empty keyword list can still
total zero impressions, and `x/0` in float arithmetic produces `+Inf`/`NaN`, which is not
encodable as a JSON number.

## Review pass — the outcome a boolean cannot express

One finding survived the earlier rounds, and it is the most consequential one in the plan:
the bulk-mutate failure branch marked every pending action `success: false` and called it
"none was applied."

That claim is not one the service can make. `MutateKeywordCriteria` chunks internally, so a
failure on the third chunk leaves the first two **definitely applied**; and a
`transportError` means the request left and the response did not return intact, so the
operation in flight may well have committed. Flattening all three states — confirmed,
unconfirmed, not attempted — into `false` tells the operator to retry the whole batch. For
pause/remove that is merely wrong; for Phase 2's change-bid it stacks a second adjustment on
top of one that already landed.

So the client now owes the caller a typed failure. `PartialMutateError` carries
`AppliedThrough` (the count whose outcome upstream confirmed) plus those confirmed results,
which lets the dispatcher split `pending` three ways instead of one: keep the confirmed
outcomes, mark the operation at the failure point UNCONFIRMED with a message that says to
check Google Ads before retrying, and mark the rest not-attempted and safe to retry.

This is the create-path discipline applied to a mutate: an ambiguous upstream outcome is
reported as ambiguous. Assuming failure is the more dangerous default precisely because it
reads as safe.

Left open for the owning team: whether `KeywordActionResult` should carry an explicit
three-state `outcome` field rather than encoding the distinction in the `message` prose. It
would be cleaner for the UI, but it changes a response shape the UI already consumes.
