# 2026-08-19 — LFXV2-3260 a blank Microsoft conversion cell is not a measured zero

**Fix** — `foldReportRows` read `ConversionsQualified` with `parseReportFloat`, which
maps an empty cell to `0`. `ConversionsQualified` is always requested, so `convOK` is
true whenever the header names the column, and a row with a BLANK conversion cell
therefore contributed a hard `0` to the total and set `out.Conversions` non-nil. An
unreported count arrived at every consumer as a measurement.

That defeats the reasoning the same file states thirty lines earlier about the ABSENT
column: it stays nil, "which is the honest answer — not zero". A present column with a
blank cell is the same claim arriving one column lower and deserves the same answer.
`model.CampaignMetrics.Conversions` is a pointer precisely so the two can be told apart:
nil means "Microsoft did not tell us", `0` means "Microsoft measured zero conversions".
The `no_conversions` rule reads this total and fires on EXACTLY zero, so a fabricated
zero raises a High-priority finding against a campaign nobody measured.

The conversions path now uses its own `parseConversionCell`, which returns
`(value, present, err)` and reports an empty cell as `present == false`.
`parseReportFloat` is unchanged and still serves spend, impressions and clicks: a blank
cell in those columns legitimately means nothing was spent or served, and widening the
rule to them would turn an ordinary no-cost row into a failed read.

**The total is withdrawn entirely, not partially summed.** This was the live decision and
it went the other way from the obvious one. Summing only the rows that carry a value
publishes a PARTIAL count as a complete measurement, with nothing left in the type to
signal the difference. The counterexample decides it: a five-row report with four blank
cells and a fifth reading `0` sums, under skip-the-blanks, to exactly `0` — and
`no_conversions` fires High on a campaign whose real conversion count is simply unknown.
That is the rule manufacturing its own finding, the identical failure mode that per-row
rounding caused and that the surrounding comments already exist to prevent. So a single
blank cell anywhere in the column leaves `Conversions` nil for the whole report. This
mirrors `reportDataIsIncomplete` directly above it, which refuses a flagged report on the
flag alone, "whatever the rows happen to contain".

Mechanically the accumulator moved OUT of `out` into a local `convTotal *float64` plus a
`convIncomplete` flag, published only after the loop. Writing straight to
`out.Conversions` would leave a partial sum standing whenever the blank row was reached
after rows that had already accumulated.

**Verification** — two mutations, each compiling and each reverted:

- `parseConversionCell` reporting a blank cell as `present == true` (the old bug exactly)
  fails three tests: `Conversions = 0` for a single blank row, `Conversions = 0` for the
  four-blank/one-zero column, and `Conversions = 3` for a blank cell following a real
  value.
- Forcing the publish guard to `if true || !convIncomplete` fails the latter two, proving
  the withdrawal semantics are covered independently of the parse change and not merely
  as a side effect of it.

No pre-existing test asserted the old behaviour — checked by grepping the suite at the
pre-change tree for a row carrying an empty trailing conversions cell, which found none.
The bug was untested rather than wrongly tested, so nothing needed rewriting.

Two complement tests guard the opposite over-correction: a fully populated column
(including a genuine `0` row) still reports its total, and a blank SPEND cell still reads
as zero spend.

**Docs** — `docs/knowledge/code/internal-dispatch.md` also carried two stale claims,
both corrected here. It gave the domain field as `*int64` (it is `*float64`), and said
Google's and Microsoft's counts are "ROUNDED rather than truncated" when both adapters
now carry the fraction through unrounded. The vendor-type table was already correct and
is untouched.
