# 2026-08-18 — LFXV2-3314 two pacing guards had no binding test

**Tests** — the inverted/zero-length flight guard and `minTime`'s clamp were both unreachable
from the suite. Deleting the first made no test fail.

**The table cases meant to reach the inverted-flight guard bound a different one.** Both used a
start at or after `now`, so they exited at the earlier `!now.After(start)` check and never
reached `!end.After(start)`. Only a flight with a PAST start and an earlier end reaches it — and
that is storable: `start_date` and `end_date` are nullable `DATE` columns with no CHECK
constraint and no service-side ordering validation, so a typo'd end date produces it. Without the
guard, `daysBetween` floors the total at one day, a lifetime budget collapses to a single day of
plan, and an ordinary campaign reports several hundred percent.

**The completed-flight test initially masked its own subject.** `measured` is
`min(spendDays, elapsed)`, so a `spendDays` at or below the flight length caps the result
whether or not `elapsed` was clamped — removing `minTime` changed nothing observable. Only a
`spendDays` large enough for `elapsed` to be the binding term can detect it. Without the clamp,
expected spend keeps accruing after a flight ends and a fully-delivered campaign drifts into
`underspending` the longer it sits finished.

**The general lesson: a passing mutation test can be masked by an unrelated cap.** When a
mutation produces no failure, check whether some OTHER term is binding the result before
concluding the guard is redundant. The rules package now has 100% statement coverage, but the
coverage number is not the point — every guard was reverted individually to confirm a test fails.
