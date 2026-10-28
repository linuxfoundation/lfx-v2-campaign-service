# 2026-10-27 — mark an ambiguous budget alignment so it is not reported as budget_name_taken

**Fix** — `reuseBudgetAfterDuplicateName` adopts an orphan budget after a `DUPLICATE_NAME`
refusal and aligns its amount through `UpdateCampaignBudget`. When that alignment failed
AMBIGUOUSLY — a 5xx, an exhausted 429, or a transport timeout on `campaignBudgets:mutate` —
the error reached `CreateOutcome` carrying no unconfirmed-write marker, and the create was
classified `budget_name_taken`. That token's operator sentence is "nothing new was created",
which in this case may be false: Google may have applied the new amount before the
connection dropped, so the EXISTING budget's amount has moved. Refs #384.

**Where the marker goes, and why not inside `UpdateCampaignBudget`.** The method marks only
its three 2xx exits — the ones where Google accepted the mutate and answered untrustworthily
(unusable response, untrustworthy resource kind, a different resource). Its ordinary
ambiguous failures deliberately return a plain wrapped error, because:

- ambiguity of that shape is what `createOutcomeAmbiguous` reads; and
- the method has a **second caller the issue did not mention** —
  `internal/dispatch/googleads_budget.go` — which already handles the ambiguity correctly by
  shape via `googleads.IsOutcomeUnconfirmed`, and says so in its own comment. Marking inside
  would be redundant there and would change the error type that path returns.

So the mark is applied at the call site in `budget_lookup.go`, gated on
`createOutcomeAmbiguous(err) && !carriesUnconfirmedWrite(err)`.

**Why the create path cannot just infer it by shape.** `CreateOutcome`'s `budget_name_taken`
arm gates on `carriesUnconfirmedWrite`, which reads the explicit `Unconfirmed()` marker ONLY
and is deliberately narrower than `IsOutcomeUnconfirmed`. The same chain usually also carries
a failed budget-lookup READ whose 5xx wrote nothing, and shape-inference there would suppress
a token that is correct. The write that may have landed therefore has to say so itself.

**What stays unmarked.** The gate leaves the DEFINITE failures alone: a 4xx Google rejected,
and the pre-send unsupported-budget-period refusal that never left the process. Nothing
changed in either case, so `budget_name_taken` remains true and the operator keeps the one
token that says exactly what to look for.

**The test starts at the HTTP handler, on purpose.** The existing classification case in
`create_outcome_test.go` hand-builds an `*unconfirmedBudgetMutateError`, so it exercises only
the predicate that reads the marker — it stays green whether or not anything in production
ever sets one. `TestReuseBudgetAfterDuplicateName_AmbiguousAlignmentIsMarkedUnconfirmed`
drives a real 500 on `campaignBudgets:mutate` through `reuseBudgetAfterDuplicateName` into
`CreateOutcome`, wrapping the two errors the way all five channels do (DUPLICATE_NAME first).
Reverted against the unfixed gate it reproduces the issue's exact symptom —
`CreateOutcome = ("budget_name_taken", true)`. Its 400 subtest is the counter-case the gate
must not catch.

**Sibling parity.** `UpdateAdGroupCPCBid` has the identical structure with
`unconfirmedBidMutateError` and the same unmarked ordinary exit, but its single caller
(`dispatch/googleads_bid.go`) is not on the create path and reads ambiguity by shape, so
there is no equivalent gap there. All five channels that call
`reuseBudgetAfterDuplicateName` — Search, Display, Performance Max, Demand Gen and Video —
already propagate the reuse error with `%w`, so the marker reaches `CreateOutcome` on every
one of them without a per-channel change.
