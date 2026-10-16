# 2026-10-16 — LFXV2-2665: the Google negative-keyword present-set must be KEYABLE, not just filtered

**Fix** — PR #346's third bot round raised one more instance of a class the first two rounds
raised four of, this time on the pre-read rather than on an error exit:
`ListCampaignNegativeKeywords` accepted a row whose `keyword.text` was empty, because
`utf8.ValidString("")` is **true** and the only text guard was the UTF-8 check. Running the data
axis over the rest of the path rather than fixing the named instance found two more.

**The class, stated properly.** The row loop was enumerating the WHERE predicate — negative,
KEYWORD, this campaign — and answering "does this row belong in the answer?". That is only half
the question. Every row in that answer becomes a map KEY, and the second question is "can this row
serve as one?". The three fields that carry key weight each need their own refusal:

| Field | Was | Now |
| --- | --- | --- |
| `keyword.text` | UTF-8 valid | UTF-8 valid **and** non-blank |
| `keyword.matchType` | enum-checked | unchanged — already closed |
| `criterionId` | non-empty | `numericID`, the spelling the mutate path already requires |

**What a bad key costs.** `campaignCriteria:mutate` is ATOMIC — `partialFailure` is never set on
this path — so a criterion that reads as ABSENT is sent again, and the duplicate fails the whole
batch, taking down every keyword Google would have accepted. The operator sees a platform
rejection of keywords that were perfectly acceptable. A bad `criterionId` costs something
different and quieter: it is handed back as an `ALREADY_PRESENT` outcome's `NegativeKeywordID`
and recorded as the criterion the campaign holds, so the READ would assert an identifier the
WRITE on the same resource would have refused (`compositeResourceID`'s `numericID` check), and no
later lever can address it.

**The third defect, which no review round reported.** The present-set key itself disagreed with
the request key on whitespace. `googleAdsNegativeKey` is built from two sources that do not arrive
in the same shape: the REQUEST side comes through `validateNegativeKeywordBatch`, which normalises
with `microsoft.NormalizeNegativeKeywordText` (`strings.Fields` joined by one space — trimmed,
every internal run collapsed); the PRESENT side is whatever Google returned, normalised by nobody.
So a criterion Google holds as `" free download"`, `"free  download"` or with a non-breaking space
did not match the caller's `"free download"`, read as ABSENT, and failed the atomic batch.

The fix collapses whitespace **inside** `googleAdsNegativeKey`, so both call sites are normalised
by construction and cannot drift the way two call sites would. The alternative — refusing rows
with odd whitespace — was rejected: over-refusal is the expensive direction here, because it
blocks the whole add. Collapsing only ever makes the present-set match MORE, and only for texts
Google itself treats as one keyword.

`validateKeywordList`'s own dedupe key (`internal/platform/googleads/targeting.go`) was
deliberately NOT collapsed to match. It trims but does not collapse internal runs, and the two keys
still agree on the negative path because every text reaching it has already been through
`NormalizeNegativeKeywordText` in `validateNegativeKeywordBatch` — there is no internal run left to
disagree about. Collapsing there as well would be inert on that path and would change the POSITIVE
create path, which no dispatcher key constrains. The agreement is kept where it is load-bearing
rather than widened where it is not; the comment at the key now says so, because the flat claim
that the two folds agree had become true only by way of an upstream step it did not name.

**Tests.** `TestListCampaignNegativeKeywords_RefusesUntrustworthyRows` gains eight rows — no
`text` key, `"text":null`, `""`, whitespace-only, no `keyword` object at all, and a `criterionId`
that is non-numeric, signed, or whitespace.

Each row now also names the SENTENCE its guard must produce, not merely that some guard refused
it. The sibling-parity sweep found that necessary rather than decorative, and found it the hard
way: several of these rows are degenerate in more than one way at once — a row with no `keyword`
object has neither a text nor a match type — so under an `err != nil` assertion alone a row can be
caught by a guard it was not written for and still read as green, which is the failure mode
`TestMeta_WriteBudget_NoAccountSelectedIsRefused` already demonstrated once. Reverting the
empty-text guard now fails all five text rows, including `no keyword object at all`, which
previously passed on the match-type guard.

The same assertion immediately corrected a claim: the invalid-UTF-8 row does **not** reach the row
loop's `utf8.ValidString` check at all. `gaqlSearch` runs `!utf8.Valid(raw) ||
hasUnpairedSurrogateEscape(raw)` over the entire response envelope before decoding, and those
bytes include every row, so the per-row check is defence in depth behind it and that row is not
revert-binding on it — exactly the shadowing `TestGetCampaignSettings_UnpairedSurrogateRefused`
records on its own sibling. The row asserts the envelope's sentence, and says why.

In `internal/dispatch`,
`TestGoogleAdsNegativeKey_AgreesAcrossBothSources` builds the request side through
`validateNegativeKeywordBatch` (so it cannot pass by normalising its own input) and asserts seven
stored spellings key identically to the caller's; it was confirmed to discriminate — five of the
seven fail with the collapse reverted. `TestGoogleAdsNegativeKey_StillSeparatesDifferentKeywords`
and `…_MatchTypeStillSeparates` pin the other half, that the collapse does not make the key match
more than Google's own notion of one keyword — an over-match would report a keyword
`ALREADY_PRESENT` and never send it, silently dropping an exclusion, which is worse because
nothing surfaces it.

**Sibling parity — the same gap found on X, and fixed here too.**
`internal/dispatch/twitter_negative_keywords.go` carried the identical key asymmetry:
`twitterNegativeKey` was built from X's raw `tc.TargetingValue` on the present side and the
normalised `k.Text` on the request side. It is materially less costly there — X sends one paced
POST per keyword rather than one atomic batch, so a missed match duplicates a single criterion
instead of failing the batch — but it is the same defect on the same shape, and deferring it would
have split one class across two reviews. The collapse moves inside the key, with
`TestTwitterNegativeKey_AgreesAcrossBothSources` /
`…_StillSeparatesDifferentKeywords` / `…_TargetingTypeStillSeparates` mirroring the Google trio;
reverting the collapse fails five of the seven agreement cases and leaves the over-match guards
green, the same discrimination profile.

`internal/platform/microsoft` does not carry it: its duplicate is a per-item outcome, so it has no
pre-read, and its request-side dedupe normalises both sides of its own key.

**Why the previous round missed it.** The earlier sweep enumerated the error exits that EXIST.
This defect was a branch that should exist and did not, which a control-flow walk is structurally
blind to — and the pre-read looked settled because its comments were detailed. Thorough prose
about the guards that are present is not evidence the guard SET is closed. The pre-push sweep now
runs a data/key axis and a trust-boundary axis alongside the control-flow one.
