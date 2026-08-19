# 2026-08-18 — LFXV2-3260 width is never a trailer

**Fix** — A width-based trailer rule came back. `dropTrailerRows` dropped every
single-cell row, which is the same unsound reasoning an earlier commit on this branch
removed from this exact function, narrowed from "shorter than the header" to "one
cell". A data row truncated to its `CampaignId` was discarded before any validation
could see it, and its metrics vanished into a total that still looked clean.

## The rule the docstring above it already refused

Commit `975b3544` removed width-based detection from `dropTrailerRows` and wrote down
why, in a docstring that is still there:

> Width alone is not evidence of a trailer: an earlier revision dropped every row
> shorter than the header on the reasoning that "a DATA row always carries the full
> column set", which is an assumption about response SHAPE on a contract this file
> declares UNVERIFIED.

Commit `5e6aa31e` then added:

```go
if len(row) == 1 {
    continue
}
```

with the justification that "foldReportRows needs three distinct columns to read one,"
so a one-cell row could never have been a measurement. That confuses **what the fold
can read** with **what the row is**. A data row truncated in transit to just its
`CampaignId` has exactly this shape. It is not a trailer; it is evidence that the
report is damaged, and it is precisely the evidence the parser needs to see.

The failure is the indistinguishable under-count this whole PR has been eliminating.
The truncated row is dropped at the filter, the surviving rows fold into a total that
is short by the dropped row's impressions/clicks/spend, and **nothing errors**. The
regression probe demonstrated it before the fix: a two-row report with one truncated
row returned `Impressions:10000 Clicks:250 CostMicros:125500000` and a nil error — a
clean-looking measurement that is simply wrong.

The rule was added to catch a trailer whose wording this file had not anticipated. That
problem was already solved in the same commit by matching **both** `©` and `@`, so the
width rule bought nothing and cost the guarantee. It is deleted. Every non-blank,
non-marker row now reaches `parseReportInt`/`parseReportFloat`, whose short-row error
REPORTS the truncation. An unanticipated trailer carrying neither marker now fails
loudly as an unparseable row — the honest failure, not a quiet under-count.

**The generalisable lesson: narrowing an unsound rule does not make it sound.** "Width
implies trailer" was wrong at the header width and is wrong at one column. The
docstring is corrected to say width is never evidence of a trailer *at any width*,
since the previous wording ("width ALONE") was what left room to re-derive the rule.

## The report timezone is not UTC, and no enum value is

The comment on `ReportTimeZone` promised UTC semantics:

> Microsoft defaults it to Pacific, so a UTC-computed window would aggregate a
> different day than the dates above name.

`GreenwichMeanTimeDublinEdinburghLisbonLondon` maps to `Europe/London`, which observes
British Summer Time. From late March to late October it is **UTC+1**, so the promised
semantics hold only part of the year.

**There is no fixed-offset UTC value to switch to.** The published `ReportTimeZone`
value set has 75 entries and contains no `UTC`, no "Coordinated Universal Time" and no
Reykjavik member. The only other UTC+0 entry, `CasablancaMonrovia`, maps to
`Africa/Casablanca`, which observes its own offset changes and is therefore strictly
worse. The value is kept as the best available approximation and the comment now says
so instead of claiming a guarantee the value does not provide.

The residual error is bounded and stated: `reportDateRange` computes calendar dates in
UTC and `toMSDate` sends bare Y/M/D, so during BST the aggregated window is shifted one
hour earlier than the window this service names. That can move an event between two
adjacent days at either end of the range; it cannot lose one, because the range is
aggregated as a whole. If per-day exactness is ever needed the fix is not another enum
value — it is converting the window to `Europe/London` before `toMSDate`.

## Mutation testing found two live gaps

Two of the mutations survived their first pass and both were real.

**The timezone value was never asserted.** The test checked only that the key
`"ReportTimeZone"` appeared in the submit body, so setting it to
`"PacificTimeUSCanadaTijuana"` — the exact default the field exists to override — kept
the suite green, while the assertion's own message read "Microsoft defaults to
Pacific". A key-presence check cannot pin a value. The test now asserts the literal
zone, and both the Pacific and `CasablancaMonrovia` mutations fail it.

**A bounds guard was unreachable dead code.** `if len(row) > 0 && isReportTrailerCell(row[0])`
looks like a live guard against a zero-length CSV record, but no input can reach it: a
zero-length row has no non-blank cell, so the blank check above always skips it first.
Removing the guard entirely left every test green, which is what exposed it — a guard
no mutation can kill is guarding nothing. The safety is real but it lives in the
**order** of the two checks, not in the guard, so the dead condition is removed and a
test now pins the ordering instead. Reordering the checks panics with
`index out of range [0] with length 0`, exactly as the test predicts.

## The concept doc lagged the parser by a commit, five times running

This is the fifth stale-doc finding on this PR, so the sweep covered the whole metrics
section of `docs/knowledge/code/internal-platform-microsoft.md` rather than the two
flagged lines. Corrected there:

- The ragged-CSV description called the trailer a "one-column `©` trailer", and the
  parsing paragraph still claimed positive identification was "blank, or a first cell
  starting with `©`" — both predate the two-marker parser.
- The width claim is restated to cover one column, so the doc no longer reads as
  permission to re-derive the single-cell rule.
- Three submit-request properties were never documented at all despite being
  load-bearing and fixed earlier on this branch: campaign-only `Scope` (and why
  sending `Scope.AccountIds` alongside would return account-wide totals), scope ids
  going out as quoted strings rather than bare JSON numbers, and the `ReportTimeZone`
  choice with its DST caveat.

The pattern behind five instances is that the doc is updated from the diff being
written rather than re-read against the code that now exists. A doc paragraph and the
guard it describes drift apart silently, because nothing compiles the prose.
