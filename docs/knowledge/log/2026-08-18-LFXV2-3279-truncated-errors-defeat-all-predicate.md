# 2026-08-18 — LFXV2-3279 a truncated error array defeated the ALL-not-ANY duplicate predicate

**Fix** — a predicate that needs a complete set, reading a silently truncated one.

`isDuplicateKeywordPartial` is deliberately ALL-not-ANY. Its own comment explains why: a batch
can mix an already-exists refusal with a GENUINE rejection (editorial disapproval, bad bid,
over-length term), and an ANY test would classify that whole batch as "already attached",
return nil, and tell the operator that the editorially-rejected keyword "already existed on the
ad group" — a keyword that does not exist upstream and never will.

**But the predicate only ever saw the RETAINED errors.** `boundedErrorItems.UnmarshalJSON`
appended only while `len < maxDecodedErrorItems` (16, aliased from `maxRetainedErrorCodes`) and
parsed-then-discarded the rest, while `AddKeywords` sends up to `maxKeywords` (60). So on a batch
whose first 16 errors were duplicates and whose genuine rejection sat at keyword 40, the
rejection was dropped **before** classification. The surviving prefix read as all-duplicate, and
the batch was reported as duplicate-only success — precisely the outcome ALL exists to prevent.
The `maxDecodedErrorItems` comment claimed retaining 16 items "can never starve the code
collection", which is true for code *collection* and false for this *classification*: one needs
a sample, the other needs completeness.

This is the same hazard the id array already paid for, one bound earlier. `KeywordIds` was moved
off `boundedNumberIDs` onto `boundedKeywordIDs` because a limit sized for a campaign create
truncated a 60-keyword response; `targeting_test.go` records that `maxDecodedErrorItems = 16`
"is sized for a campaign create — one id — and for error arrays." **The id array was widened and
the error array was not**, so the same 16 kept one guarantee and quietly broke another.

## The shape, and why not the other option

Bounding the error array by `maxKeywords` on the keyword path — mirroring the id fix — was the
other candidate and was rejected. It fixes the 60-keyword case, but leaves the invariant resting
on a size coincidence: it breaks again the moment `maxKeywords` rises above the new bound or a
body null-pads past it.

Instead `boundedErrorItems` keeps its 16-item memory cap and **records** that truncation
occurred (it became a struct: `Items` plus `Truncated`). A truncated array is then not
classifiable as duplicate-only and falls through to the rejection path. That makes the invariant
structural rather than arithmetic:

> **absence of a rejection in a set known to be incomplete is never evidence there was none, at
> any size.**

Refusing beats a false success — the failure mode it replaces reported targeting the run never
achieved. The O(1) memory bound the cap exists for is untouched.

## Correction — v13 DOES expose a keyword read

Several comments on this branch asserted that v13 has no keyword read; one of them shipped to
operators in a step string ("Their ids are not readable in v13"). **The claim is false.**

`POST https://campaign.api.bingads.microsoft.com/CampaignManagement/v13/Keywords/QueryByAdGroupId`
takes an `AdGroupId` and returns an array of `Keyword` objects carrying `Id`, `Status`, `Text`,
`MatchType` and `EditorialStatus`.
Source: <https://learn.microsoft.com/en-us/advertising/campaign-management-service/getkeywordsbyadgroupid?view=bingads-13>
(SOAP `GetKeywordsByAdGroupId`; a sibling `GetKeywordsByIds` also exists.)

**How the false claim survived is the reusable part.** It was established by enumerating THIS
CLIENT's files — which really do contain no keyword read — and then promoted to an API-wide
claim without ever being checked against Microsoft's documentation. An absence in our code is
not evidence of an absence in the vendor's API. Every affected comment now says what is actually
true: this client calls no keyword read, and that is a GAP in this client rather than a limit of
v13. Adopting the read is a separate change with its own review; nothing here calls it.

## Known gap this leaves open

On a MIXED batch (some keywords new, some already present) only the NEW ids persist, because a
duplicate entry returns a null id slot. `MicrosoftDispatcher.ToggleStatus` then enables exactly
the recorded ids, so the pre-existing keywords **stay Paused**: the campaign reports live while
partly inert, spending on a fraction of the approved targeting. The ACTIVATE guard does not catch
it — it fires on `len(microsoftKeywordIDs(campaign)) == 0`, and the enabled subset is non-empty.
Reading the ad group's keywords through `QueryByAdGroupId` is what closes it. Recorded, not built.

## Mutation-tested

Three compiling mutations, all killed, and each checked for whether it could be OBSERVED rather
than merely compiled:

- Dropping the `!resp.PartialErrors.Truncated` gate at the call site → the new truncation test
  fails. This is the guard itself.
- `b.Truncated = false` (never recorded) → the truncation test and `TestParseErrorCodes` fail.
- `b.Truncated = true` unconditionally (recorded even when nothing was discarded) → four
  duplicate-path tests fail. This mutation is the one worth keeping: without it, a flag set
  unconditionally would refuse every legitimate all-duplicate batch and re-break the reuse path
  that the previous fix exists to make converge. Both directions of the flag are pinned, not
  just the true one.

The new test (`TestCreateCampaign_TruncatedErrorArrayIsNotDuplicateOnlySuccess`) sends 60
keywords with a genuine editorial rejection at index 40 and duplicates everywhere else; it was
verified to FAIL against the unfixed head with "must NOT be reported as duplicate-only success"
before the fix was written.
