# 2026-08-18 — LFXV2-3279 an unparseable error code read as a placeholder

**Fix** — the decode-time duplicate tally could not tell an error it failed to PARSE from an
entry that never failed at all, so a genuine rejection past the retention cap was reported as a
clean converge-on-reuse success.

## The defect

`isNonDuplicateKeywordItem` decided whether an element was a real rejection by rendering its
codes with `codeString` and testing for the empty string:

```go
if codeString(it.ErrorCode) == "" && codeString(it.Code) == "" {
    return false // a null/placeholder slot, not an error
}
```

`codeString` returns `""` for a genuinely absent or null field — the intended case, an
index-aligned array null-padding the entries that SUCCEEDED. But it also returns `""` for any
shape it cannot render as a string or a number: a JSON object, an array, a bool, or a
whitespace-only string. Those are not placeholders. They are errors this client cannot NAME.

Folding the two together is a false absence, and it is the licence-to-succeed value. Past
`maxDecodedErrorItems` (16) the item is discarded from `Items`, so `isDuplicateKeywordPartial`
never sees it either — neither term of the call-site guard can observe it. A 38-keyword reuse
retry carrying one editorial rejection whose code arrived as an object was classified as wholly
duplicate:

```text
err=<nil> AlreadyExisted=true keywordIDs=0
```

The operator is told the keyword already exists on the ad group. It does not exist and never
will.

## Why this change introduced it

The previous entry's `!Truncated` gate happened to refuse this input for the wrong reason — the
array was truncated, so it refused everything past the cap regardless of what the errors were.
Replacing that gate with a tally that is right about parseable codes silently dropped the
property for unparseable ones. This is the "what did my fix just make possible?" class: the new
term was strictly better on the case under test and strictly worse on a case no test covered.

## The fix

Presence is now tested on the RAW bytes, independently of whether the value can be rendered:

```go
func rawCodePresent(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}
```

A field that is absent or JSON null is padding; a field that is PRESENT but unrecognizable is an
unclassifiable error and counts as a rejection. "Cannot classify" fails closed to the rejection
path rather than collapsing into "not an error". `isDuplicateKeywordPartial` uses the same
predicate, so the streaming tally and the retained-slice check still cannot drift apart.

## Mutation-tested — both directions

The guard has two failure modes and each is pinned by its own test, so neither can be weakened
without a red suite:

- **Under-rejection** — reverting the presence test to `codeString(...) == ""` compiles and
  fails `TestBoundedErrorItems_NonDuplicateKeywordsCountsPastTheRetentionCap/a_present_but_unparseable_code_is_a_rejection,_not_a_placeholder`
  (four shapes: object, array, bool, whitespace) and
  `TestCreateCampaign_UnparseableCodePastTheCapIsNotDuplicateOnlySuccess`, which reports
  `AlreadyExisted=true` — the exact silent success above.
- **Over-rejection** — treating every element as a real error compiles and fails
  `.../an_absent_or_explicitly_null_code_is_still_a_placeholder` and
  `TestCreateCampaign_NullPaddedSuccessesPastTheCapAreNotRejections`. Over-rejecting is a false
  absence too: it would refuse the ordinary mixed batch this ticket exists to converge.

The null-skip `continue` in `isDuplicateKeywordPartial` remains independently binding —
replacing it with `if false` fails `TestIsDuplicateKeywordPartial/only_null_placeholders_carry_no_duplicate`,
because a placeholder must skip WITHOUT setting `found`.

## Also corrected in this pass

- The doc block for `isDuplicateKeywordPartial` had been orphaned onto the newly inserted
  `isNonDuplicateKeywordItem`: with no blank line between the two comment runs, Go attached the
  whole 28-line group to the new function and left `isDuplicateKeywordPartial` with no godoc at
  all. Confirmed by parsing the file with `go/parser` and reading each `FuncDecl.Doc`. The
  file's most important safety argument — ALL-not-ANY — was filed under the wrong symbol.
- `TestCreateCampaign_FullDuplicateBatchLargerThanTheCapStillSucceeds` carried a doc paragraph
  arguing the classification rests on an arithmetic identity ("the count leaves no room for an
  unaccounted-for entry"). That is the approach this ticket tested and REJECTED, and it is
  refuted three times elsewhere in the same change. A reader trusting it would reintroduce the
  hole. Replaced with the mechanism actually under test.
- `NonDuplicates` renamed to `NonDuplicateKeywords`. The field lives on `boundedErrorItems`,
  which is embedded by the campaign, ad-group, ad and keyword responses, but "duplicate" here
  means specifically an already-exists KEYWORD (1517/1542) — and `errCodeDuplicateCampaignName`
  is a real and different concept in this same package. The local `wholeArrayIsDuplicates`
  became `sawNoKeywordRejections`: the old name asserted a property the value does not carry on
  its own, since an all-placeholder array also tallies zero.
- The previous entry's claim that `Truncated` is "still load-bearing" is narrowed to what is
  true: no production path reads it — `grep -rn "\.Truncated" --include="*.go" . | grep -v _test`
  returns only the write at `client.go:1098`. It is asserted by tests, which is why mutating it
  away fails them.
