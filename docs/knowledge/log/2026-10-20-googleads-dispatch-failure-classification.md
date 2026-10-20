# 2026-10-20 — google-ads: classify what a failed create left behind

**Fix** — A Performance Max create failed in production and the operator was told
`google-ads campaign creation could not be confirmed; something may have been created — verify on
the platform before retrying`. That sentence was the least the service could say, and it was being
said where the service in fact knew much more. Read-only verification in the live account
(`8666746580`, change history, All time) showed exactly one row for the attempt: a budget created
at 2026-10-19 4:08:35 PM Pacific, `$500.00`, campaign column EMPTY — the budget step had
SUCCEEDED and `campaigns:mutate` had failed. Because that budget is non-shared its name
synchronizes to the campaign's once a campaign attaches, so a budget still carrying its composed
name is provably unattached. The next retry on the same event would therefore hit `DUPLICATE_NAME`
at the budget step and the operator would be told, again, that something may have been created.

**Two root causes, both of them a message problem rather than a dispatch problem.**

1. **No status reached the message.** `domain.UpstreamStatusCarrier` is implemented by the
   reddit, linkedin, meta, twitter, microsoft and hubspot HTTP error types; the google-ads
   `*apiError` was the one that did not, even though `StatusCode` sat on the struct. The status
   arm of `DispatchFailureMessage` was unreachable for google-ads whatever the platform answered,
   so a definite 400 refusal and an ambiguous 503 rendered identically. Fixed by adding
   `UpstreamStatus() int` to `*apiError` — the status only. `*apiError` is an unexported TYPE,
   so nothing outside the package can name it in an `errors.As` to reach `Method`, `Path` or
   `Body`; those stay exported struct fields, so the carrier widens no leak but closes none
   either. What keeps the body away from the operator is unchanged: `Error()` never prints it,
   and `DispatchFailureMessage` returns fixed sentences rather than reporting from the error
   (#208).

2. **Five outcomes collapsed into one sentence.** Every retained google-ads failure — budget name
   already taken (nothing created at all), budget created then campaign definitely refused,
   budget created and campaign genuinely unknown, campaign name already taken, campaign created
   but its cascade unfinished — reached the operator as the same UNCONFIRMED wording. Those are
   not variations of one message; "nothing new was created" and "the budget exists and the
   campaign does not" are opposite instructions.

**The carrier.** `domain.UpstreamCreateOutcomeCarrier` (`UpstreamCreateOutcome() string`) is the
POST-request counterpart of `PreflightRefusal`, and deliberately a separate type:
`PreflightRefusal`'s contract is that it wraps only a refusal raised BEFORE the first upstream
request, so reusing it here would make "nothing was created" claims about requests that were in
fact sent. Its vocabulary is six service-authored constants — `budget_name_taken`,
`campaign_name_taken`, `budget_created_campaign_refused`, `budget_created_campaign_unconfirmed`,
`campaign_created_setup_incomplete`, `create_unconfirmed` — platform-neutral because every
platform here builds a campaign as the same budget-then-campaign-then-setup cascade. Each token
answers "what is in the ad account now?", not "which code path failed", so two arms that leave the
same state share a token.

**Classified once, not fifty times.** The classifier is ONE function —
`googleads.CreateOutcome(result, err)` — called at the single wrap in `internal/dispatch/googleads.go`
that every channel's partial failure already passes through, not an annotation on each of the
create paths' ~50 partial-returning arms. That works because all five create entrypoints build
their partials to the same escalating contract: a pre-budget partial carries names only, a
post-budget partial adds `CampaignBudgetID`, a post-campaign partial adds `CampaignID`. The ids a
partial carries ARE the account state, so reading them covers all five channels at once and cannot
drift one arm at a time.

Two things the local review round corrected here, both of which made the service state something
FALSE where the sentence it replaced had been merely weak — the failure mode this whole change
exists to avoid, reproduced inside the fix for it.

First, each duplicate-name refusal (`isDuplicateBudgetNameErr` / `isDuplicateCampaignNameErr`) is
now gated by the id state of its own step. `parseErrorCodes` keeps the bare enum VALUE and
discards the error family, so a `DUPLICATE_NAME` raised by any later mutate in the cascade — PMax
reaches `assetGroups:mutate` well after the campaign exists — matched the budget arm and told the
operator that nothing had been created while a billable campaign was live. The ids are the better
evidence everywhere except at the budget step itself, which is the one place they cannot separate
a duplicate-name refusal from an unconfirmed budget create.

Second, the budget-created case claims a REFUSAL only on positive evidence of one: a definite 4xx
`*apiError` in the chain. It previously claimed refusal whenever `createOutcomeAmbiguous` was
false — but that predicate answers false for any error that is neither `*apiError` nor
`*transportError`, and ten arms across the five channels return a budget partial with a plain
`fmt.Errorf`: the post-2xx resource-name parse and validate failures, past which `campaigns:mutate`
answered 2xx and the campaign DOES exist, and the `ctx.Err()` arms, where no campaign request was
ever sent. The operator was told to delete the budget and retry — the one instruction that
duplicates a paid campaign. Everything without that positive evidence is now reported as
unconfirmed, which is what those arms' own error text had said all along.

`internal/dispatch`'s `withCreateOutcome` attaches the token. `Error()` is the wrapped text
unchanged and `Unwrap` keeps the chain, so it ADDS a question to the error and replaces nothing:
the `*apiError` and its new status, and a `context.Canceled` from the ctx arms between budget and
campaign, all still resolve through it. A nil error or an empty token adds no carrier at all, so a
classifier that declined to classify is indistinguishable from no classifier.

**#208 still holds.** `DispatchFailureMessage` **switches** on the token and returns one of five
FIXED sentences; it never formats the token into a message. So an unrecognised token — one a later
version invents — can only fall through to the existing status/unconfirmed wording, never put
words in the operator's mouth, and no upstream text can reach the operator whatever the carrier
holds. `create_unconfirmed` deliberately gets no sentence of its own: it means exactly what the
fallback already says, and a second copy would be one more sentence to keep in step.
`TestDispatchFailureMessage_UnclassifiedOutcomeIsUnchanged` pins that a missing carrier and that
token produce the identical pre-existing message, which is the regression guard for every provider
that does not classify. The service tests assert against a stand-in carrier whose `Error()` is
deliberately hostile (`... -> 400 SECRET must not be shown`), so every arm's assertion doubles as a
leak check.

**What the operator now reads for the case above.** The retry hits the budget-name collision and is
told that nothing new was created, that the retry stays blocked until that budget is removed or
renamed, and that — because an unattached non-shared budget does not appear in the shared budget
library — it is found in the platform's change history. That is the state the account was actually
in, and the only read-only UI path that shows it.

**Scope.** Message only. The claim verdict is the orchestrator's `preCreate`, computed once and
taken here, never recomputed: claim release/retention, the retained row's status and the job's
aggregate are exactly what they were before. No other provider's wording changes, and the
pre-create branch is unreachable for the carrier by construction
(`TestDispatchFailureMessage_OutcomeDoesNotReachThePreCreateBranch`).
