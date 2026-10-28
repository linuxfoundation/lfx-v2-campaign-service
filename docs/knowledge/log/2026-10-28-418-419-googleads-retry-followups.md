# 2026-10-28 — googleads: correct the counted-call-site count and test the transport branch of the budget-alignment gate

**Fix** — two review findings left unresolved when #404 and #406 merged, raised as issues
rather than hot-fixed (#418, #419).

**#418: five call sites, not six.** The `doRequestCounted` and `retriedUnconfirmed` doc
comments in `internal/platform/googleads/client.go` said "six" idempotent mutating call sites
use the counted form. Five do: ad group status (`adgroup_ad.go`), CPC bid (`bid_update.go`),
budget amount (`budget_update.go`), campaign status (`campaign.go`) and asset group status
(`pmax_creative.go`). The sixth idempotent mutate, `adGroupAds:mutate`, stays on
`doRequest` on purpose, because its failures already surface as `partialCascadeError`, which
reports `Unconfirmed()`. The comment now says five and names the exception. The concept file
already said "Five of the six idempotent mutating call sites", so it needed no change.

**#419: the transport branch is now exercised.**
`TestReuseBudgetAfterDuplicateName_AmbiguousAlignmentIsMarkedUnconfirmed` covered a 500 (marked)
and a 400 (unmarked). Neither subtest reached the `*transportError` branch of
`createOutcomeAmbiguous`, and the test's own doc comment names that branch as one the marker
exists for. A third subtest now hijacks and closes the connection on `campaignBudgets:mutate`.
It asserts three things: the error is a `transportError`, so the case cannot pass through
some other branch; it carries the unconfirmed-write marker; and `CreateOutcome` reports
`create_unconfirmed`. Disabling the `transportError` arm of `createOutcomeAmbiguous` makes
this subtest fail with `CreateOutcome = ("budget_name_taken", true)`, which is the regression
the review described. The server helper now takes a mutate handler instead of a status code,
so all three failure shapes share one fixture.
