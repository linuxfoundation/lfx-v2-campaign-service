# 2026-10-27 — A budget create that answers 2xx without a usable resource name is recovered by name (#387)

**Fix** — `campaignBudgets:mutate` can answer **success** and still carry no
`results[0].resourceName`, or one with no id segment. All five Google channels
failed on the spot when that happened. The budget was committed — Google said
so — so the failure left exactly the orphan class the DUPLICATE_NAME arm exists
to close, reached from the opposite direction: non-shared and unattached, so it
appears in no Google Ads UI surface, while holding the composed name that blocks
every later retry of the same brief.

## What the arm now does

`budgetFromCreateResponse` is the whole of every channel's "read the budget out
of a 2xx" step. On a parseable response it is `firstResourceName` and nothing
else — no search is issued, and a test pins that by failing on any search at
all. Only a parse failure calls `recoverBudgetAfterUnreadableCreate`, which
looks the budget up by the name this client composed — this client's own
idempotency key — and hands back the canonical resource name `reusableBudget`
rebuilds from the customer id and the budget id, never the string the unreadable
response happened to carry.

A recovered id is reported to the operator in its own `Steps` sentence rather
than folded into the channel's "Campaign budget created" line, which the five
channels word differently.

## Why it is `findReusableBudget`'s shape and not `ambiguousBudgetCreateOutcome`'s

An absence here is a **contradiction**, not an answer. On the ambiguous path
(#385) Google said nothing, so a zero-row account is evidence that nothing was
created and the arm reports it as such. Here Google said 2xx, so a budget of
that name provably exists; an account showing none has returned two answers that
cannot both be true, and the recovery refuses rather than classifying.

Three consequences:

- **The re-reads matter more here, not less.** The commit is confirmed, so a
  first read that misses can only be replica lag. The schedule — 4 attempts,
  250ms→1s, under a 20s ceiling — was extracted out of
  `resolveBudgetAfterAmbiguousCreate` into `retryFindBudgetByName` and is now
  shared by both callers. The **detachment** was not: `context.WithoutCancel`
  stays in the ambiguous arm alone, where a cancelled caller context is itself
  one of the shapes that reach it. On this path nothing arrives that way, and
  the caller's deadline is a legitimate bound on a readback that is holding up a
  campaign mutate.
- **The gates are still checked**, though "Google committed this moments ago"
  predicts they will pass. The argument is an argument; the row is the evidence.
  A shared or already-attached budget is refused by `findBudgetByName` exactly
  as on the DUPLICATE_NAME path. **The amount is one of those gates here**, and
  it refuses rather than aligns: the create that just succeeded set the amount,
  so a row carrying a different one is not this create's budget. The shape that
  makes this more than theory is a same-named budget removed shortly before,
  with a lagging replica still serving it as ENABLED and unattached — every
  other gate passes on it, and the attachment check reads the campaign side,
  which lags with it. On the DUPLICATE_NAME path the opposite is right: there
  the budget pre-dates the brief, so a different amount is an old budget to
  bring into line.
- **Nothing downstream changed on failure.** Both errors are wrapped with the
  parse failure first, which is the order they happened in and the order the
  sibling arms read in. That ordering is **not** an `errors.As` guard here, as
  it is on the DUPLICATE_NAME arm: `firstResourceName` returns only a
  `json.Unmarshal` wrap or a plain `fmt.Errorf`, never an `*apiError`, so the
  chain's first `apiError` is the lookup's either way. What is new is that this
  arm admits the lookup READ's status into a chain that previously carried
  none, so `DispatchFailureMessage` can now say "the platform answered HTTP N"
  about a create that answered 200 — accepted because the read's status is the
  only upstream signal this path has, and the sentence it decorates already
  names which call succeeded and which did not. The partial still carries no
  `CampaignBudgetID`, `CreateOutcome` is still `CreateOutcomeUnconfirmed`, and
  the campaign is still never attempted.

## One sentence that was false and is gone

The old refusal read "budget creation UNCONFIRMED (`<name>` may exist — verify
in Google Ads before retrying)". A 2xx **is** Google confirming the create: the
budget's existence is settled and only its identity is not. The sentence now
says it **WAS** created, because telling an operator it may exist sends them
looking for something that is definitely there. No *production* classification
was coupled to the wording: `CreateOutcome` reads ids off the partial rather
than parsing error text.

Two existing tests did assert the old `UNCONFIRMED` token — the empty-results
and malformed-id variants in `campaign_test.go`. Both now assert the new
sentence and the behaviour that did not change: no campaign attempted, and the
parse failure surviving into the refusal.

## Deliberately out of scope

The campaign-side arms where a 2xx **campaign** resource name fails to parse
(`pmax.go` and siblings, plus `validateCampaignResource`). A billable campaign
is already running by then, and recovering its identity is a harder problem than
recovering a budget's.
