# 2026-08-19 — LFXV2-2641 an omitted `negative` is POSITIVE, not unknown

**Fix** — the criterion-type gate landed at head `1fef400a` refused every ordinary positive
keyword, making `ApplyKeywordActions` unusable on its normal path. Reported by Cursor Bugbot
(High) and reproduced before changing anything.

The gate decoded `ad_group_criterion.negative` as a `*bool` and computed

    positive[...] = row.AdGroupCriterion.Negative != nil && !*row.AdGroupCriterion.Negative

so a nil field — no `negative` key in the row — resolved to `positive=false`. That value is
present in the map, so the id does NOT take the absent-row arm; it takes the `!isPositive` arm
and is reported as "a NEGATIVE keyword". Google Ads REST is protobuf JSON, which does not
serialise a field at its default value, so an ordinary positive keyword arrives with `negative`
OMITTED. Every positive keyword was therefore refused, and re-reading could not repair it: the
second response omits the field identically.

**Absence already carries a meaning in this wire format — `false`.** A guard that reads it as
"unknown polarity" gives absence a SECOND meaning it cannot carry. The fix decodes `negative` as
a plain `bool`, so omission decodes as `false` = positive = allowed, while an explicit
`negative: true` is still refused.

A MISSING FIELD and a MISSING ROW are different facts and the previous fix conflated them. Only
the missing-row case fails closed, and that remains correct and untouched — it is the guard's
whole purpose, since a userList criterion returns no `keyword_view` row at all.

The premise was confirmed empirically before any edit rather than reasoned from the wire format
alone, because the previous commit's prose asserted the opposite:

- `Negative *bool` was the **only** `*bool` response decoder in the entire Google Ads client,
  and the only decoder in the repo treating nil as anything but false. Its counterpart
  `customerClient.manager` (`client.go:1159`) is a plain `bool` used directly.
- The other Google Ads `*bool`, `campaignBudgetCreate.ExplicitlyShared` (`campaign.go:240`), is
  an OUTBOUND field, and its comment states the same rule from the sender's side: "A pointer so
  the false value is always emitted rather than omitted."
- `campaign.go:255` already reasons the standard way about the identical construct — a campaign
  targeting no network "is what an omitted `networkSettings` resolves to — proto3 bools default
  false". The codebase held both readings; only one matches the wire.
- The repo has **no** `testdata/`, cassette or captured wire body. Every Google Ads response in
  the suite is a hand-written string literal, so no fixture was evidence of real API behaviour.
- `keywords.go:754` conceded the point in prose — "a positive keyword can legitimately arrive
  with the field absent" — and refused it anyway.

**The test gap was the real finding.** `criterionRowJSON` always rendered `"negative":%t`, so
`TestApplyKeywordActions_PositiveKeywordSucceeds` fed the decoder an explicit `"negative":false`
— a body no conformant protobuf-JSON serialiser ever produces. Fixture and code shared the same
false assumption, so the happy-path test passed against broken code, and the previous commit's
own mutation report read that pass as proof the guard "is not merely refusing everything". The
single fixture that DID omit the key asserted refusal. Two dispatch-level fixtures carried the
same explicit spelling; the whole class was swept, not just the client one.

The helper now omits `negative` for a positive row, which is the realistic shape. Added:
`OmittedNegativeFieldIsPositiveAndActionable` (the realistic wire body, inline at the
assertion so a helper change cannot quietly reintroduce the explicit form),
`ExplicitNegativeFalseIsPositive` (both spellings must reach one verdict), and
`MissingFieldAdmittedMissingRowRefused`, which pins BOTH verdicts in one test so the two facts
cannot be collapsed again. `OmittedNegativeFieldFailsClosed` asserted the defect and is gone.

**Verification** — one mutation, compiling, reverted: restoring the `*bool` field and the
`!= nil && !*...` expression fails 9 client tests and 2 dispatch tests, every one reporting
"which is a NEGATIVE keyword" for a positive criterion — including
`PositiveKeywordSucceeds`, which now fails where before the fix it passed, and
`MissingFieldAdmittedMissingRowRefused` on its missing-FIELD half while its missing-ROW half
still refuses. `HappyPathReturnsOutcomes` fails at the dispatcher.
