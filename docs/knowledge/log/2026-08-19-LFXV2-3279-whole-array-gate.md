# 2026-08-19 — LFXV2-3279 the outer keyword gate read only the retained prefix

**Fix** — the truncation change moved the SAFETY term to a whole-array tally
(`NonDuplicateKeywords`), but left the OUTER gate reading the retained prefix:

```go
if partialErrorsHaveAny(resp.PartialErrors.Items) {   // Items = first 16 only
```

`PartialErrors` can be index-aligned, carrying a null placeholder for every entry that
succeeded. Microsoft does not document the ordering, so nothing stops a body from putting
those placeholders in the LEADING slots and its real errors after them. When it does, the
retained 16-item prefix is entirely null, `partialErrorsHaveAny` is false, and the whole
partial-error branch is skipped — after which the null `KeywordIds` of the duplicate
entries fall to the cardinality check and surface as `errNoID`/UNCONFIRMED.

That is the exact inverse of the ordering
`TestCreateCampaign_NullPaddedSuccessesPastTheCapAreNotRejections` already covered, and it
turns an ordinary converge-on-reuse into an operator-facing "keywords may exist — verify
before retrying". Reproduced before fixing:

```
duplicates past the retention cap must still converge as ordinary reuse:
  microsoft-ads keyword targeting UNCONFIRMED (... keywords may exist ...):
  returned no usable id at index 18: create response carried no id
```

The previous revision recorded this as a known residual in a comment at the call site
("fail-closed, but narrower than every full-duplicate batch converges"). Fail-closed is the
right default, but the narrowing was not benign: it silently made convergence depend on
where in the array the wire happened to place its errors.

Decode now also tracks `AnyErrors` — whether ANY element of the whole wire array carried an
actual code — alongside the existing tally, and the gate uses it. Presence is tested on the
RAW bytes, for the same reason `NonDuplicateKeywords` is: a code this client cannot render
is still an error, and naming-failure must not read as absence-of-error.

`isDuplicateKeywordPartial` needed the same treatment and it is the half that is easy to
miss. Its `found` term is a presence check over `items`, so with an all-null prefix it
returned false and the batch still fell to `errPartialFailure` even once the outer gate
admitted it. It now takes `anyErrors` as its seed. The ALL question is still answered from
the retained items, paired as before with the whole-array `NonDuplicateKeywords == 0`, so a
non-duplicate discarded past the cap still blocks the duplicate-only conclusion.

Two documentation corrections from suppressed reviewer comments, both accurate:

- The `boundedErrorItems` doc block still asserted that a production consumer depends on
  `Truncated`. It does not — repo-wide, `Truncated` is written during decode and read by
  nothing outside tests. It is now described as descriptive metadata, with the two tallies
  named as the terms that carry the safety.
- `TestCreateCampaign_TruncatedErrorArrayIsNotDuplicateOnlySuccess`'s doc comment ended with
  "a truncated array must fall through to the rejection path", which this change
  deliberately makes false — refusing on truncation alone is what broke ordinary reuse. It
  now states the property in terms of the whole-array tally.

**Verification** — three compiling mutations, each reverted:

- outer gate back to `partialErrorsHaveAny(Items)` → `TestCreateCampaign_DuplicatesOnlyPastTheCapStillConverge` fails.
- `found := anyErrors` back to `found := false` → same test fails. Two independent terms,
  two independent kills; fixing only the outer gate would have left the branch entered and
  still misclassified.
- `AnyErrors` never set during decode → five tests fail, confirming the flag is load-bearing
  rather than write-only (the mistake this entry corrects for `Truncated`).

One reviewer comment on this PR was NOT actioned, with reasoning rather than a silent pass:
it read `TestCreateCampaign_FullDuplicateBatchLargerThanTheCapStillSucceeds`'s doc comment as
explaining the property arithmetically ("38 vs 37 errors"). The comment already says the
opposite in as many words — "The distinguishing fact is not arithmetic... No total can tell
them apart" — and then names decode-time classification of every wire item as what licenses
the conclusion. Nothing to correct.
