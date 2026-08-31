# 2026-08-31 — LFXV2-2775: a claim with no placeholder cannot be omitted

**Fix** — the composed prompt instructs the model to OMIT any sentence whose `[BRACKETED]`
placeholder has no supplied value. That rule reaches placeholders, and only placeholders.

`Registration Push` — the DEFAULT stage every unrecognised value resolves to — carried
`PreviewPattern: "Registration closes [DATE]. Early bird pricing available now."`. The second
sentence has no placeholder, so nothing could remove it, and the model was told as FACT that early
pricing exists for an event whose price is never supplied. Only `eventName`, `location` and dates
are ever filled.

Both offending patterns now carry placeholders (`[EARLY_PRICE]`, `[DISCOUNT_AMOUNT]`,
`[PROMO_CODE]`), so the existing OMIT rule drops the claim instead of asserting it.

**The test had to be strengthened before it could catch the reported case.** The first version
asked whether the STRING contained a `[`. `"Registration closes [DATE]. Early bird pricing
available now."` does — the bracket belongs to a different sentence. The mutation caught the
Discount Offer case and let the reported one through. The check is now SENTENCE-scoped, matching
what the OMIT rule actually operates on, and the mutation then fails on both.

**The general shape:** when a guard operates at some granularity — a sentence, a line, a field —
a test written at a coarser granularity passes on inputs the guard cannot handle. Ask what unit
the rule acts on and assert at that unit.

Also here: the test inventory in `internal-service-email-copy.md` said "20 test functions" while
the file held 24. Replaced the count with the three missing names, because a count goes stale on
the next commit and nothing greps for it.
