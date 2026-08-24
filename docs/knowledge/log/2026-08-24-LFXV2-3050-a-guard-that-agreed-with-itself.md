# 2026-08-24 — LFXV2-3050 a coverage guard that could not detect what it existed to catch

**Fix** — the provenance coverage table's guard compared itself to a hand-maintained literal, so
the one change it existed to catch was the one change it could not see.

`TestAllDispatchers_StampProvenanceOnEveryCampaignReturn` walks a table of every dispatcher and
asserts each stamps `RanOnSystemAccount`. Guarding that the table stays complete, it read:

```go
if len(cases) != 7 {
    t.Fatalf("this table covers %d dispatchers, but the package has 7 Dispatch methods ...")
}
```

Both sides are maintained by hand, and adding an eighth dispatcher moves NEITHER. The message
promises to catch "an uncovered one can lose its defer silently"; the code cannot. An assertion
whose expected value is a literal detects edits to the TABLE, never changes to the thing the
table is supposed to track.

**Measured, not argued.** A compiling eighth dispatcher (`MutantDispatcher` with a real `Dispatch`
method) was added to the package and both guards were run against that same tree:

- old `len(cases) != 7` — **PASSED** (a survivor: the uncovered dispatcher sailed through)
- new derived guard — **FAILED**: "this table covers 7 dispatchers, but the package declares 8"

The replacement derives the expected count from the package's own source, parsing the non-test
files for `func (d *XDispatcher) Dispatch(`. The count now moves in the same commit that adds a
dispatcher, and this test is what fails until the table grows a row for it. `dispatchMethodCount`
fails on a zero result rather than returning it, because a broken scan would otherwise make the
guard vacuous in exactly the silent way it is replacing.

The production registry (`internal/container`) would be the other honest source, but importing it
from `internal/dispatch` is an import cycle; the source scan stays inside the package it audits.

Two nil-contract wording defects were corrected in the same sweep. A test diagnostic and several
docs said `nil` "is reserved for rows that predate the column". It is not: `AdoptCampaign` omits
`ran_on_system_account` from its INSERT column list, so a campaign adopted TODAY reads back NULL.
`nil` means provenance was NOT OBSERVED — pre-`000027` rows and upstream adoptions both. Reading
it as an age signal would date a fresh adoption to before the migration, and reading it as `false`
would fold unknown spend into "the project paid".
