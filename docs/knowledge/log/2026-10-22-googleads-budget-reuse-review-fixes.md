# 2026-10-22 — keep the declined-reuse error classifiable, and the operator sentences true

**Fix** — the review-round fixes on top of the budget-name adoption change. Three of them
alter behaviour; the rest are truthfulness and test hygiene.

**The declined-reuse error now wraps both causes, DUPLICATE_NAME first.** All five creating
channels previously rendered the reuse verdict with `%v`, which flattened it to text and
dropped `*unconfirmedBudgetMutateError`'s `Unconfirmed()` out of the chain. Both are now
wrapped with `%w` — and the order matters: every predicate in `create_outcome.go` reaches
its target with `errors.As`, which stops at the FIRST `*apiError`, so the DUPLICATE_NAME
refusal is wrapped ahead of the reuse verdict. Wrapping the verdict first let an `apiError`
from the lookup READ shadow the duplicate code, costing the failure its `budget_name_taken`
token — caught by the new classification test, not by review.

**`budget_name_taken` is gated on `!carriesUnconfirmedWrite(err)`.** That token promises
nothing new was created. One reason a reuse declines is an amount alignment Google accepted
without a usable acknowledgement, where the budget create did fail but the existing budget's
amount may have moved; such a failure now classifies as `create_unconfirmed`. The new helper
consults only the explicit `Unconfirmed()` marker, deliberately not `IsOutcomeUnconfirmed`,
which also shape-infers from a 5xx or transport error — on this path usually the lookup read
failing, which wrote nothing.

**Operator-facing sentences brought into line with adoption.** Thirty strings across the
five channels said a budget "created" where it may have been reused ("in place", "created or
reused"). `dispatch_failure.go`'s `budget_name_taken` sentence had enumerated "or hold a
different amount" as a decline reason the code does not have — a mismatched amount is
aligned, never a refusal — and asserted the retry stays blocked until someone resolves it,
which is false for the transient case; it now names the real reasons and says a retry may
well succeed. `CreateOutcomeBudgetNameTaken`'s godoc was the fourth of four token comments
and had been missed when its three siblings were updated.

**Tests.** Channel-level DUPLICATE_NAME reuse proofs for Display, Demand Gen and Video
(`budget_reuse_channels_test.go`) assert the ADOPTED resource name reaches each channel's
`campaigns:mutate` — the arm is written out once per channel, so the Search and Performance
Max proofs said nothing about the other three. A two-case classification test pins the new
guard from both sides: an alignment mutate that may have landed must not report
`budget_name_taken`, and a failed lookup read must still get it. Handler captures in
`pmax_test.go` and `budget_lookup_test.go` are now mutex-guarded like
`newBudgetLookupServer`'s own; `go test -race` is clean.
