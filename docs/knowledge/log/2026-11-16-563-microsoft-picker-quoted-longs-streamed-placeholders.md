# 2026-11-16 — #563 Microsoft pickers read quoted longs, cap the rows they decode, and pass zero-code placeholders

**Fix** — The #613 review findings on the remarketing-list and conversion-goal picker reads, split
out of the shared 2026-11-16 fix entries when the Microsoft gaps PR was divided (the pickers ship
in part 2). Four findings:

- The remarketing-list picker reads `SearchSize` and `MembershipDuration` as a JSON number or a
  quoted one (`"12000"`, Microsoft's REST spelling of a long), through `wholeNumberField`. Before
  this, a quoted long failed the whole read. A value that is not a non-negative whole number in
  range (a fraction, a negative, text, an overflow) still refuses the answer as malformed. Covered
  by `TestListRemarketingLists_QuotedAndNumericLongs`.
- Both picker reads decode their body token by token (`decodeCappedList`, `client.go`), so the
  5,000-row cap refuses an oversized answer at the first row past the cap and stops decoding
  there. The cap does not limit the HTTP read: `doRequest` has already buffered the body (up to
  8 MiB). The malformed-response classification is unchanged. Covered by
  `TestListRemarketingLists_RowCapStopsDecoding` and `TestListConversionGoals_RowCapStopsDecoding`.
- `indexedErrorItem.isError` shares `audienceErrorCode`'s rule: an entry whose codes are all
  defaults (absent, null, 0 or "") is a placeholder, so `{"Index":0,"Code":0,"ErrorCode":null}`
  no longer fails the conversion-goal picker read.
- `isDefaultCode` treats a quoted `"0"` as a default too, so `{"Index":0,"Code":"0"}` is a
  placeholder in the conversion-goal picker read (the remarketing-list picker already used
  `audienceErrorCode`). Covered by `TestListRemarketingLists_DefaultCodeIsNotAnError` and the
  placeholder cases in `TestDecodeCappedList_RefusesUnreadablePartialErrorsAndDuplicateKeys`.
