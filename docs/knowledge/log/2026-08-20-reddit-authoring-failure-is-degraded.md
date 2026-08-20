# 2026-08-20 — Reddit: an authoring failure is a DEGRADED outcome, not a clean one

**Fix** — When `CreateCampaign` authored a promoted image post from `ImageURL`
(`Step 1.5`) and that authoring FAILED, `validatedPostID` stayed empty, the ad
step was skipped, and nothing set `CampaignResult.AdWarning`. The dispatch
adapter marks a campaign `created_degraded` only when `AdWarning` is non-empty,
so this path persisted a clean `created` for a campaign that owns no ad.

That is the expensive shape of the failure-as-success class: `created` is a
TERMINAL status, so the orchestrator's idempotency check (`isReusableCampaign`)
reuses the row and REFUSES to re-dispatch. The campaign stays permanently ad-less
while the row, the job result, and the API all report success. The missing ad was
visible only as a line of prose inside `Steps`.

The reasoning that produced it is worth naming: the original code commented that
an authoring failure "degrades to a no-ad campaign, matching no-PostURL". The two
states are alike in the RESULT (`AdCount == 0`) but opposite in INTENT. With no
`PostURL` and no `ImageURL` the caller never requested an ad, so a clean `created`
is correct. With `ImageURL` set, an ad WAS requested and does not exist. Matching
on the observable shape while ignoring what was asked for is what turned a
shortfall into a success.

`AdWarning` is now seeded from the authoring step (`adWarning := postWarning`), so
it means "an ad was REQUESTED and the result carries no confirmed one" across
both the authoring and ad-create paths. The no-`PostURL`/no-`ImageURL` path never
sets it, so its clean-`created` contract is unchanged.

**Re-dispatch was deliberately left blocked.** `created_degraded` is terminal in
`isReusableCampaign` exactly like `created`, so this fix makes the campaign
VISIBLE for reconciliation without making it retryable. That is the right
trade-off here: the paid campaign and ad group DO exist upstream, and a
re-dispatch would re-run the whole create and duplicate them — it cannot repair
just the missing ad, because the dispatcher has no resume-from-the-ad-step path.
Reopening the row for re-dispatch would trade a silent no-ad campaign for a
duplicated paid one. Making the ad step independently repairable is a separate
change to the dispatcher, not to this classification.

**Same commit, second fix: an ambiguous authoring failure was labelled pre-send.**
The failure arm branched only on `*apiError`; every other error fell to a step
reading "failed before the request reached Reddit". But this repo's own
classification treats `transportError` as AMBIGUOUS — the request may have landed
— and `createPromotedPost` returns exactly that for a 2xx carrying no `data.id`,
*because* the post may exist. In-flight timeouts and TLS errors take the same
path. So both were telling an operator to author the post manually when Reddit
might already hold it. Authoring now uses the same three-way split as the ad path:
`*apiError` → FAILED (definite), `createOutcomeAmbiguous` → UNCONFIRMED (verify
first), `default` → pre-send. The `default` arm is the pre-send arm, so any new
non-`apiError` shape must be checked against `createOutcomeAmbiguous` before it
lands there.

**The first version of that second fix ordered the arms wrongly, and reintroduced
the very harm it removed.** It read `case errors.As(err, &postAPIErr)` FIRST and
`case createOutcomeAmbiguous(err)` second. But the two predicates are NOT mutually
exclusive: `createOutcomeAmbiguous` returns true for an `*apiError` carrying a
**5xx** (Reddit received the POST and may have committed it before erroring) or a
**mutating 3xx** (redirects are force-disabled, so it reached a responder that may
have committed before redirecting). Both are `*apiError`s, so both matched the
first arm and were reported as *"Reddit rejected the post — author the post and add
the ad manually"*. That is the duplicate-post instruction the ambiguous
classification exists to prevent, arriving through arm ORDER instead of through the
original `else`.

`createOutcomeAmbiguous(err)` must therefore be evaluated **before** the
`*apiError` arm, and `errors.As` used only to EXTRACT the status for the message —
never to select the arm. The ad-create path already did exactly this
(`gotStatus := errors.As(...)` then `if createOutcomeAmbiguous(err)`), which is why
"match the existing path's ordering" was the right instruction and inventing a
switch-shaped variant of it was not. The `*apiError` arm now means what its message
claims: a definite rejection, i.e. a 4xx that is not a mutating 3xx.

The generalisable lesson: when two `case` predicates in a `switch` can both be true
for the same value, the arm order IS the classification. A reviewer reading the
arms in isolation sees three correct-looking branches; only asking "can predicate 2
be true when predicate 1 is?" exposes it. `errors.As` is a TYPE test and
`createOutcomeAmbiguous` is a SEMANTIC test over that same type — they were never
disjoint.

A wording assertion has the same trap as the prose assertion below: the 4xx arm and
the pre-send arm SHARE the token `FAILED`, so `Contains(res.AdWarning, "FAILED")`
passes on either branch and cannot discriminate. The 4xx test now asserts
`"FAILED (Reddit rejected the post)"` and that the text does NOT say "before it
reached Reddit".

**A test named for the behaviour asserted the bug.**
`TestCreateCampaign_AuthorPostFailureDegrades` passed identically before and after
the fix: it checked a `Steps` substring and `AdCount == 0`, never `AdWarning`. The
name claimed the degradation; the assertions only covered the prose. `Steps` is a
human-readable blob — the structural field the adapter actually reads is the one
that must be asserted. It now asserts `AdWarning` and its FAILED/UNCONFIRMED
wording.

Tests: `authorpost_test.go` gains `..._AuthorPostAmbiguousIsUnconfirmed` (2xx with
no id; an in-flight hangup), `..._AuthorPostPreSendIsDefinite` (a refused dial via
a transport that reroutes only `POST /profiles/.../posts` to a dead port), and
`..._AuthorPostAmbiguousAPIErrorIsUnconfirmed` (a 503 and a 302 — the two shapes
that are BOTH an `*apiError` and ambiguous, so they only classify correctly when
the ambiguous arm is tested first).
`internal/dispatch/reddit_test.go` gains
`TestReddit_AuthorPostFailurePersistsCreatedDegraded`, which asserts the PERSISTED
STATUS an operator reads (`created` before, `created_degraded` after) rather than
the internal flag. The pre-send test is load-bearing beyond its own arm: without
it, replacing `createOutcomeAmbiguous(err)` with a blanket `err != nil` survives,
since nothing else distinguishes ambiguous from pre-send.
