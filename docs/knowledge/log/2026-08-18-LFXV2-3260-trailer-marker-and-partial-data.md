# 2026-08-18 — LFXV2-3260 trailer marker and partial data

**Fix** — The CSV trailer filter recognised the wrong character, and the tests
could not have caught it because every fixture encoded the same guess. Microsoft
also flags a partially-processed report in the CSV header block, which the parser
ignored, so an under-count was returned as a measurement.

## The footer marker was a guess its own fixtures validated

`dropTrailerRows` matched Microsoft's copyright trailer on `"©"` alone. Microsoft's
published sample report renders it as **`"@2020 Microsoft Corporation. All rights
reserved. "`** — an `@`, not a `©` — and the `ExcludeReportFooter` element
description repeats that spelling independently. Note also the trailing space and
the dynamic year.

The consequence is not cosmetic. An unrecognised footer is not dropped quietly: it
survives the filter, reaches `foldReportRows` as a data row, and fails
`parseReportInt` on its text. **Every otherwise-successful live report would fail
to parse** the first time the gate was flipped on.

**The tests could not have caught it, and that is the part worth recording.** All
six fixture footers used `©` — the same character the code matched — so reverting
the marker failed tests that only ever proved the code agreed with a fixture nobody
had validated against Microsoft's published output. A guard and its test drawn from
one assumption test the assumption's self-consistency, not its truth. The fixtures
are now split deliberately: `realisticCSV` and the short-row fixture use `@`,
`headerOnlyCSV` and the unit fixtures keep `©`, and two tests assert each marker
independently, so dropping either one fails a test.

**Both markers are accepted rather than swapping one guess for another.** A locale,
report-writer version or documentation revision could plausibly produce either, this
contract is UNVERIFIED, and the cost of accepting both is nil — neither character can
begin a legitimate numeric data row. Matching is on the marker PREFIX, not the full
sentence, because the year is dynamic and pinning the literal would break each January.

A generic rule sits alongside the marker list: a **single-cell** row after the data
section is dropped whatever it says. A trailer is one unquoted sentence, so the CSV
writer emits it as one cell, while a data row needs three named columns to be read at
all — so a one-cell row could never have been a measurement. That covers a footer
whose wording this file has not anticipated. It stops there on purpose: a wrong
rejection is a silent under-count, which is the failure class this file exists to
prevent, so narrowness alone still never drops a row.

## `ReturnOnlyCompleteData: false` had no partial-data check

`ReturnOnlyCompleteData` is sent `false`, and it must be: Microsoft fails a `true`
request outright with `NoCompleteDataAvaliable` (2004) when the window is not fully
processed, which would make every window including today unreadable. The cost is that
the last day may be an under-count, and Microsoft says so **only in the report's
header block**, never in the HTTP status:

```csv
"Potential Incomplete Data: true"
```

The parser ignored that line, so a partial total was indistinguishable, to every
consumer, from a complete measurement of a smaller number.

It is now **refused**, not surfaced. `model.CampaignMetrics` has no field for
"provisional", and adding one would have to be honoured by every consumer to mean
anything — until then, returning the numbers WOULD be the silent under-count.
Refusing is recoverable in a way a wrong number is not: the caller can re-read once
processing settles, and `ErrReportDataIncomplete` says so. The dispatcher maps it to
neither metrics sentinel (both mean 400, and a 400 tells the caller to stop asking);
it rides the default 503 arm, where waiting genuinely does help.

The guard refuses only an **explicit** affirmative. A complete report emits the same
label with `false`, and the whole header block disappears under
`ExcludeReportHeader` — Microsoft states that with `ReturnOnlyCompleteData=false`
"there is no indication as to whether the data is complete". So absence is not
evidence of completeness, and refusing on it would fail every report rather than the
partial ones. The honest scope is "refuse what Microsoft explicitly flags", not
"prove completeness". The search is bounded to rows above the header, so text in a
data row cannot trigger a false refusal.

## Report ids go out as quoted strings — the precedent was a different API

The scope ids were sent as bare `json.Number`, citing `campaign.go`. That precedent
does not transfer: `campaign.go` is **Campaign Management v13**, this is **Reporting
v13**, and the two are versioned in lockstep but are not one contract.

Microsoft's Reporting v13 JSON reference renders every `long` in this request as a
**quoted string**, and its placeholder convention distinguishes the cases rather than
quoting everything — `CampaignReportScope` and `AccountThroughCampaignReportScope`
show `"AccountId": "LongValueHere"` and `"CampaignId": "LongValueHere"` quoted, while
`ReportTime`'s `Day`/`Month`/`Year` on the same page show `IntValueHere` unquoted.
`long` quoted, `int` bare, in one document: the quoting is a type signal, not a
formatting habit.

Quoting is also the precision-safe direction independent of the docs. A 64-bit id
exceeds the 2^53 a JSON number holds exactly; a server accepting `long` parses a
numeric string, whereas a bare number risks silent rounding. An id that had been
rounded would scope the report to the wrong campaign and report **another campaign's
numbers as this one's**.

One caveat, recorded rather than smoothed over: no Microsoft page states the
long-as-string rule in prose, and no concrete example uses real numeric ids. The
conclusion rests on the type-differentiated placeholder convention — strong, but
inference from examples. A reviewer's supporting claim that the official Python SDK
types both ids as `StrictStr` was **not** confirmed: that citation names `bingads`,
the legacy SOAP SDK, whose own example passes ints; the REST SDK is the separate
`msads` package, whose source could not be reached. The docs carry the conclusion;
the SDK claim does not.

## Four stale doc claims, each verified against head first

- `internal/platform/microsoft/client.go` said the account id "also reaches the
  request body via `Scope.AccountIds`". False since `67d7a35d` removed that field —
  it documented the very union bug this PR fixed. It now names
  `Scope.Campaigns[].AccountId` and says why `AccountIds` is omitted.
- `docs/knowledge/code/internal-platform-microsoft.md` said "the one path where
  zeroes are truthful is a `Success` status with no download URL". False since
  `6e89ebf4`; both empty shapes return `ErrNoRowsInReport`. No path in the file
  synthesizes a zero.
- `internal/dispatch/microsoft.go` claimed `ErrReportNotReady` maps to **500**. It
  matches no sentinel arm, so it falls to the metrics switch's `default:` in
  `internal/service/brief.go`, which returns `ConnServiceUnavailableError{Code:
  "503"}`. 503 is also the right code: it promises waiting might help, and for a
  build time varying around the poll budget it might.
- `docs/knowledge/kubernetes/deployment.md` documented only
  `REDDIT_METRICS_ENABLED`. `MICROSOFT_METRICS_ENABLED` exists on the same terms and
  is now documented alongside it.

## Mutation results

Each guard was reverted with a compiling change and the suite re-run. Two mutations
survived the first pass and were the useful ones — both gaps are now closed:

| Mutation | Caught by |
| --- | --- |
| Marker list keeps only `©` | `TestDropTrailerRows_BothCopyrightMarkersAreRecognised/@` |
| Marker list keeps only `@` | `TestDropTrailerRows_BothCopyrightMarkersAreRecognised/©` |
| Marker check removed entirely | same test, both subtests |
| Single-cell trailer rule removed | **survived at first** — every trailer fixture carried a marker, so the marker check masked it. Now caught by `TestDropTrailerRows_SingleCellTrailerWithoutAMarkerIsDropped` |
| Incomplete-data check disabled | `TestGetCampaignMetrics_IncompleteDataIsRefusedNotReported`, `TestFoldReportRows_IncompleteDataFlagVariants` (5 subtests), `..._IsCheckedBeforeTheColumns` |
| Any incomplete label treated as affirmative | `TestFoldReportRows_IncompleteDataFlagVariants/{explicit_false,split_cells_false,empty_value}` |
| Ids reverted to bare `json.Number` | `TestGetCampaignMetrics_SubmitBodyShape` |
| Incomplete check moved below the column guard | `TestFoldReportRows_IncompleteFlagIsCheckedBeforeTheColumns` |
| Flag scanned over the whole file, not the preamble | **survived at first** — no fixture placed the label below the header. Now caught by `TestFoldReportRows_IncompleteFlagIsScopedToThePreamble` |

The two survivors are the same lesson as the trailer marker itself: a guard is only
pinned by a test that can distinguish it from the guard next to it. The single-cell
rule and the marker match both dropped the same fixtures, so either could be deleted
unnoticed until a fixture exercised one without the other.
