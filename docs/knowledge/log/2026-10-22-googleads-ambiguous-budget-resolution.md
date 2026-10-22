# 2026-10-22 — settle an ambiguous budget create against the account (#385)

**Fix** — when `campaignBudgets:mutate` failed in a way that does not say whether the
write landed (a 5xx, an exhausted 429, a transport error after the request was on the
wire), every one of the five creating channels returned a partial carrying no budget id
and never read the account back. A budget that HAD been committed therefore became an
orphan nobody was told about: non-shared and unattached, so it appears in no Google Ads
UI surface, and it silently blocked the next retry of the same brief by name one call
later — failing differently, a call further on. That is the shape of the 2026-10-16 and
2026-10-20 Performance Max orphans.

**The account may be read here, and more strongly than in the DUPLICATE_NAME arm.** This
arm is Step 1: `campaigns:mutate` has not been issued, on this attempt or on any attempt
that reached it. A non-shared budget's name synchronises to its campaign's name on
attach, so no campaign can have taken this name and renamed its budget — a live budget
found under the composed name is this client's own, and so is an absence. Unlike the
DUPLICATE_NAME arm, Google refused nothing, so nothing can contradict the answer.

**`findReusableBudget`'s contract is split.** `findBudgetByName` is now the shared
search-and-gates core with three exits — the budget, a clean `(nil, false, nil)` for zero
rows, and an error for everything else. `findReusableBudget` is a thin wrapper that owns
the one judgement belonging to the DUPLICATE_NAME caller alone: zero rows there is a
contradiction, so the wrapper turns the clean absence into an error. Every other gate
failure stays an error inside the core — a budget that exists but is shared, attached,
wrong-shaped or unreadable is never reported as an absence — which is what makes the
clean absence safe for the new caller: absence means the account was read and held no
such budget, never that a gate was skipped.

**`ambiguousBudgetCreateOutcome` is the WHOLE of the arm for all five channels** —
resolution, partial and sentence — so they cannot drift on a failure none of them can
exercise against the live account. Three outcomes: budget exists → the caller's partial
plus `CampaignBudgetID` and a sentence saying it WAS created, unattached, and that a
retry will reuse it; proved absent → `(nil, err)` saying nothing was created, which is
the pre-create failure contract (no outcome token, the orchestrator releases the claim,
the retry is clean); account unreadable → the partial and the original UNCONFIRMED
sentence, nothing settled and nothing claimed. The create error is wrapped FIRST in all
three, because `errors.As` stops at the first `*apiError` and a read error must not
shadow the create's.

`CreateOutcome` needed no new annotation: it already classifies from the partial's ids,
so setting `CampaignBudgetID` is what turns the generic unconfirmed failure into
`CreateOutcomeBudgetCreatedCampaignUnconfirmed`, and a nil result into no token at all.
The proven-absent `(nil, err)` is the only behaviour change visible outside the package.

**Cost.** One `googleAds:search` on the ambiguous path, two when a budget is found
(`assertBudgetUnattached` asks the campaign side independently) — paid only on a failure
that was previously unresolvable.

**Tests.** Unit proofs that `findBudgetByName` reports zero rows as a clean absence
without the second query and that a gate failure is never an absence; a three-subtest
proof of `resolveBudgetAfterAmbiguousCreate`'s outcomes; and channel-level proofs of all
three arms on Search plus the created/absent arms on Performance Max. The Display and
Video `AmbiguousBudgetKeepsTheClaim` tests now permit the resolution READ and forbid only
further mutates, which is the invariant they were always about.

Out of scope and worth a follow-up: the adjacent "2xx with an unparseable resource name"
arm, where the same lookup would recover a lost budget id.
