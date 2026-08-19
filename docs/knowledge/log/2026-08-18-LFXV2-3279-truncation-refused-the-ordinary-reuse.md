# 2026-08-18 — LFXV2-3279 refusing on truncation alone refused the ordinary reuse case

**Fix** — a safety gate that was correct but not live, because the condition it tested was a
proxy for the one that mattered.

The previous entry made `isDuplicateKeywordPartial` refuse to classify a TRUNCATED
`PartialErrors` array as duplicate-only. The reasoning was sound and still holds: absence of a
rejection in a set known to be incomplete is not evidence there was none. But the gate it
shipped, `!resp.PartialErrors.Truncated`, tested the wrong term.

`boundedErrorItems` sets `Truncated` whenever the wire array carries more than
`maxDecodedErrorItems` (16). `AddKeywords` sends up to `maxKeywords` (60), and the product's
typical brief carries ~38 keywords. On a reuse retry the whole batch is re-posted, so **every**
keyword comes back an already-exists duplicate — one `PartialError` each, ~38 of them — which
trips the 16-item cap on every ordinary retry. The duplicate arm was skipped, the batch fell to
`errPartialFailure`, and the converge-on-reuse behaviour that `d8b14d13` exists to provide was
live only for briefs of 16 keywords or fewer. A retry failed where it should have succeeded.

The safety property was never wrong. The liveness property was missing.

## Counting the discarded items does not fix it

The obvious repair is arithmetic: a batch in which every keyword is a duplicate emits exactly as
many errors as keywords sent, so record the pre-truncation count and compare it to
`len(msKeywords)`. That count IS available cheaply — the decode loop already visits every element
to advance the stream — but the comparison does not work, for two independent reasons, and both
were found by building it and watching tests fail rather than by reasoning about it.

**`PartialErrors` is SPARSE.** It carries an entry only for a FAILED item, so it legitimately runs
shorter than the batch whenever some keywords succeeded. `Total == len(msKeywords)` refused the
long-standing mixed-batch cases (`DuplicateKeywordCodeSpellings`, `ReusedAdGroupMixedDuplicate…`)
where 3 keywords produce 1–2 errors. The count has no fixed relationship to the request.

**And a matching count proves nothing anyway.** A duplicate and a genuine editorial rejection are
each exactly one error. The 60-keyword safety case — a rejection at index 40, duplicates
elsewhere — reports 60 errors for 60 keywords and satisfies every counting argument, while the
rejection sits inside the discarded region. Arithmetic over totals can prove how MANY errors
exist; it cannot prove they are all duplicates. The counting version passed the liveness test and
**broke the safety test**, which is how the flaw surfaced.

The framing that misled here is worth naming: the danger was described as "a non-duplicate
rejection of a keyword that succeeded", which a count can exclude. The actual danger is a
non-duplicate rejection of a keyword that also FAILED — fully accounted for by the count, and
invisible.

## The shape: classify during decode, where every element is seen

The ALL test does not need a count, it needs to have SEEN every error. So the classification is
performed in `UnmarshalJSON`, the one place every element passes through — including the ones
about to be dropped for memory. `boundedErrorItems` gains `NonDuplicateKeywords`, a tally over the WHOLE
wire array of entries carrying an actual error code that is not an already-exists keyword code.
The call site asks `NonDuplicateKeywords == 0`.

A large full-duplicate batch now converges; a batch whose rejection sits at index 40 of 60 is
still refused, because the tally counted it even though `Items` does not hold it. The O(1) memory
bound is untouched — one comparison per item, no retention.

Widening the retention bound to `maxKeywords` was again NOT chosen, for the reason the previous
entry gives: it would leave the invariant resting on a size relationship between two constants.
The tally keeps it structural at any size.

The single-item test is factored into `isNonDuplicateKeywordItem`, shared by the streaming tally
and `isDuplicateKeywordPartial`, so the two can never drift apart about which spellings count as
already-exists. `Truncated` is retained, but the honest statement of its status is narrower than
"still load-bearing": after this change no PRODUCTION path reads it — the flag is written during
decode and asserted only by tests, which is why mutating it away fails them. It stays because it
still describes the array truthfully and is the natural signal for any future consumer that needs
to know the set is a prefix; the term now doing the safety work at the call site is the tally.

## Mutation-tested

Nine compiling mutations, each checked for whether it could be OBSERVED:

- gate → `!Truncated` (the head behaviour) → **caught** by the new liveness test. This is the
  regression itself.
- gate → `true` (no guard) → **caught** by the retained truncation test.
- gate → `NonDuplicateKeywords != 0` (inverted) → caught.
- tally never increments → caught.
- tally moved INSIDE the retention test, so only retained items count → **caught**. This is the
  most valuable kill: it proves the safety test genuinely exercises the discarded region rather
  than the retained prefix, which is the entire point of the tally.
- null/placeholder slot treated as a rejection → **survived the first pass**, and that was a real
  gap. A body may null-pad the entries that SUCCEEDED to stay index-aligned; padding falling past
  the cap would have been counted as a rejection and refused a legitimate mixed batch. Reachable
  and consequential (20 duplicates + 18 created came back as a failure). Now covered by
  `TestCreateCampaign_NullPaddedSuccessesPastTheCapAreNotRejections`, and caught on re-run.
- dropping the 1542 match-type spellings from the shared helper → caught.
- `Truncated` never set → caught (the flag is still live).
- dropping the `isDuplicateKeywordPartial` conjunct at the call site → **SURVIVED, and stays
  survived.** This is a finding, not a coverage gap: the enclosing `partialErrorsHaveAny` already
  establishes that a retained item carries an ACTUAL code, so with `NonDuplicateKeywords == 0` the
  predicate cannot currently disagree — an all-null retained prefix diverts to the UNCONFIRMED
  path before reaching this branch (verified by probe). The conjunct is defensive redundancy. It
  is KEPT so the branch stays correct on its own reading rather than depending on a non-local
  invariant in a different `if`, and the comment now says that plainly instead of claiming it
  supplies a term the tally lacks.

## Tests

`TestCreateCampaign_FullDuplicateBatchLargerThanTheCapStillSucceeds` sends 38 all-duplicate
keywords and was verified to FAIL at head `6e12d304` ("must converge, not fail") before the fix.
`TestBoundedErrorItems_NonDuplicateKeywordsCountsPastTheRetentionCap` pins the tally at the decode layer,
including the case where the retained prefix looks wholly duplicate but the count does not.
The prior safety test is unchanged and still passing.
