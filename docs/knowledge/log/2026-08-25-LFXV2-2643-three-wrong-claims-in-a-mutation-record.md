# 2026-08-25 — three wrong claims in the LFXV2-2643 mutation record

**Fix** — `2026-08-25-LFXV2-2643-brief-and-job-repos-reach-a-live-database.md` shipped with
three inaccurate claims, found in review of #185 by dealako and corrected here. That entry is
left as written; this file carries the corrections, because a log entry is a record of what was
believed at the time and rewriting it would destroy the evidence that the belief was wrong.

Read the original with these three amendments.

## 1. A mutation row described a test that was never shipped

The table listed:

| mutation | result |
| --- | --- |
| `CreateJob("not-a-uuid")` → SQLSTATE 22P02 | FAIL — proves the 23503 check binds, not just `err != nil` |

That was a **dev-time probe**, not a delivered test. `grep -rn not-a-uuid dbtest/` returns
nothing: `TestLiveCreateJobRequiresARealBrief` passes a well-formed absent UUID and asserts
`23503`. Someone re-deriving the work would look for `22P02` and find no such test.

The two mutations that test actually binds, both re-run against shipped code:

| mutation | result |
| --- | --- |
| drop `campaign_jobs_brief_id_fkey` | FAIL — `CreateJob accepted a brief_id with no such brief` |
| `CreateJob` inserts `status='bogus'` (a non-FK error) | FAIL — `SQLSTATE 23514 ... want SQLSTATE 23503` |

The second is what pins the SQLSTATE check rather than a bare `err != nil` — the property the
fabricated row was reaching for, demonstrated by mutating shipped code instead of the test.

## 2. A count-bearing row had drifted, and only re-running found it

Asked to check the other rows, I re-ran each one carrying a count rather than reading it. The
`classifyNoRowTx` row claimed **"FAIL at 4 sites across 3 tests"**; the real figure is **5 sites
across 4 tests**. The cross-tenancy commit late in #185 added a fifth failure site
(`ReplaceBrief from a foreign project`) and nobody updated the row.

This is the more general lesson of the round. A mutation table is EVIDENCE, and evidence drifts
silently as the code it describes grows — the row was correct when written and wrong by merge.
Reading a count cannot detect that; only re-running it can. The other count-bearing row, "3 aged
rows stranded" for the cleanup-ordering control, was re-derived on a fresh database and holds.

The stated total was low for the same reason: the true ledger is **33** — 29 implementation,
3 schema, 1 test-side ordering control — not 31. The table is a selected subset of 18 rows, not
the ledger.

## 3. Two counts in one file disagreed

The file said "14 SKIP / 14 PASS" while its opening line said "15 tests added". **15 is
correct**, derived rather than copied, since either line could have been the wrong one:

- `comm` between the pre-merge base's and HEAD's sorted `grep "^func Test"` output gives
  **15 net-new, 0 removed**;
- the skip control, re-run with a run-set **generated from that same `comm` output** so the
  run-set cannot silently disagree with the count, reports **15 SKIP / 0 PASS** without
  `TEST_DATABASE_URL` and **0 SKIP / 15 PASS** with it.

## A fourth, caught before it shipped

The first draft of this correction asserted that "jsonb columns are a brief-repo concern". They
are not: **13** tables in this schema carry jsonb columns, `campaign_briefs` merely carries the
most (8). The same draft claimed 8 `assertJSONEqual` call sites in `brief_repo_live_test.go`;
there are **7**. Both were caught by re-deriving each number against `information_schema` and
`grep -c` before the push, which is the practice item 2 above argues for — applied to the
correction itself.

`assertJSONEqual` stays in `brief_repo_live_test.go`. The reason is where the jsonb ASSERTIONS
are, not where jsonb lives: 7 call sites there across five columns, 1 in `job_repo_live_test.go`
against `result`. That matches the package's shape — `insertApprovedBrief` is declared in
`audience_lease_live_test.go` and consumed by 6 files, `insertBrief` in `schema_live_test.go`
by 5 — and a helpers file for one function would break it rather than follow it.

## What to take from this

All three are one defect: **a claim that outlived the code it described.** Two were true when
written; one never was. The check that finds the first kind is re-deriving the number, not
re-reading the sentence — and it is worth doing on every count a document offers as evidence,
because the ones that drift are exactly the ones nobody thinks to question.
