# 2026-08-18 — LFXV2-3279 Microsoft refuses duplicate keywords, so the skip was wrong

**Fix** — the reuse-path keyword SKIP added earlier today
(`2026-08-18-LFXV2-3279-reused-adgroup-keyword-duplication.md`) rested on a factual claim about
Microsoft that turns out to be false, and the skip it justified created a state no automated
path could leave. Both are corrected here. That earlier entry stands as written; this one
supersedes its conclusion.

## The claim, and what Microsoft actually documents

The skip's stated justification was that re-posting a keyword to an ad group that already has
it creates a SECOND COPY — "two criteria bidding on the same term, so the reused campaign pays
twice for the traffic the operator approved once". Checked against Microsoft's own
documentation, that does not happen. `AddKeywords` **rejects** an already-present keyword
rather than duplicating it:

* `CampaignServiceDuplicateKeyword` (**1517**) — "An attempt was made to create a duplicate of
  a keyword that already exists."
* `CampaignServiceKeywordAndMatchTypeCombinationAlreadyExists` (**1542**) — "A keyword with the
  specified match type already exists."

Comparison is by **normalized** form, not literal text: keyword normalization folds case,
whitespace, accents, number/date formats and punctuation before comparing, so `Car`, `car` and
`car.` are one keyword to Microsoft. The reasoning that produced the skip was sound given its
premise; the premise was simply never checked against the vendor's documentation.

A separate reviewer claim — that duplicates are permitted but harmless because only the
highest-bid copy serves — is **also** false, and in the opposite direction. Microsoft does not
admit the duplicate at all. Both readings were wrong; the documented behaviour is refusal.

## Why the skip had to go, not merely be narrowed

The skip was gated on `adGroupExisted` alone, which treats "the ad group exists" as proof that
"its keywords exist". They are different facts and they come apart on the FIRST run:

1. Run 1 creates the ad group, then fails before the keywords land — an ad failure, an
   `UNCONFIRMED` keyword step, a 429, any of them.
2. Run 2 finds the ad group, hits the skip, and returns **success with no keywords posted**.
3. The row persists with empty `KeywordIDs`.
4. `MicrosoftDispatcher.ToggleStatus` then refuses ACTIVATE **forever**:
   `len(microsoftKeywordIDs(campaign)) == 0` → `ErrCampaignNotProvisioned`.

With no keyword READ in v13 there is nothing that can repair it from here. A campaign whose ad
group landed but whose keywords never did could never be activated by any automated path.

The narrower fix considered first — skip only when the ad group existed AND keywords were
previously recorded — was rejected because the persisted ids **cannot reach this code**.
`Dispatch` receives `(brief, platform, config)` and no campaign row; the prior `KeywordIDs`
live on the persisted result blob, which the platform client never sees. Threading them in
would mean widening the dispatcher interface for every provider. And once duplicates are known
to be refused, the gate buys nothing: posting is already safe.

## The shape

The skip is removed. The keyword batch is posted on the reuse path, and the duplicate
rejections are classified as the success they are:

* `errDuplicateKeywords` wraps `errPartialFailure`, so existing `errors.Is(…, errPartialFailure)`
  classification still matches, but the case is distinguishable.
* `isDuplicateKeywordPartial` recognizes all four spellings — both codes under either the
  symbolic `ErrorCode` enum or the numeric `Code`.
* The caller's duplicate arm sits **above** the `errPartialFailure` arm. Because
  `errDuplicateKeywords` wraps it, the broad arm would otherwise win and report a rejection for
  an ad group whose targeting is already correct.
* The classification requires **every** actual error to be a duplicate, not just one. A batch
  mixing a duplicate with a genuine rejection (editorial, bad bid, over-length) stays on the
  rejection path — an ANY test would report success and claim the editorially-rejected keyword
  "already existed", which is a keyword that does not exist and never will.
* Ids are never invented for pre-existing keywords. A duplicate entry returns a null id slot
  and v13 has no read to resolve it, so `KeywordIDs` carries only what THIS run created.

The consequence is that the case that mattered now resolves itself: an ad group with no
keywords gets keyworded on the next run and ACTIVATE succeeds. The residual case — every
keyword already attached, so no ids are learned — still refuses ACTIVATE, which remains the
honest answer, since enabling a Paused keyword requires its id. LFXV2-2665 is what closes it.

## What the steps say now

The operator-facing text changed with the reason, rather than outliving it. It no longer claims
the keywords were withheld to avoid duplicating spend, and no longer tells the operator to
attach a newly-added keyword by hand — that keyword is now attached automatically. It states
that the already-present keywords were left unchanged, and that Microsoft refuses a duplicate
rather than creating a second copy, so nothing was duplicated and no bid doubled.

## Verification note

The mutation that survived its first pass was narrowing `isDuplicateKeywordPartial` to the
numeric `1517` alone. Every duplicate test supplied the numeric `Code` and the symbolic
`ErrorCode` together, so no test distinguished the four spellings — and v13 emits either.
`TestCreateCampaign_DuplicateKeywordCodeSpellings` now sends each spelling in isolation, and
kills it. Worth recording because the gap was in the FIXTURES, not the guard: a batch that
always carries both fields cannot tell you which one the code reads.

A second mutation survived and was pinned rather than dismissed: setting the predicate's
accumulator to `true`, which would classify an empty or all-null PartialErrors array as
"every error is a duplicate". Through the client that arm is unreachable — the predicate is
only consulted inside a `partialErrorsHaveAny` gate — so no end-to-end test could reach it.
`TestIsDuplicateKeywordPartial` covers the predicate directly instead, because the function is
package-visible and a future caller without that gate would turn a batch that rejected nothing
into a silent no-op.

The ALL-not-ANY rule itself came out of a throwaway probe, not a review comment: a batch mixing
a duplicate with an editorial rejection returned `nil` error and a step line claiming the
rejected keyword already existed. Writing the probe was cheaper than reasoning about the
combination, which is the general lesson.
