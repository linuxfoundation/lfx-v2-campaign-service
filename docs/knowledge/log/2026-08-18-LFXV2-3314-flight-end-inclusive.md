# 2026-08-18 — LFXV2-3314 the flight end date runs through the end of its day

**Fix** — `internal/service/rules` was the only consumer treating `end_date` as an exclusive
midnight, so it cut the final day off every flight.

`end_date` is a `DATE` column (migration 000002) and means "through the END of that day". Every
platform adapter already reads it that way: Meta sends `in.EndDate + "T23:59:59+0000"`, X does
`endDateParsed.AddDate(0, 0, 1)`, LinkedIn uses end-of-day 23:59:59.999. The rules package used
the midnight the column decodes to as an exclusive bound — which is the *start* of the final day.

**The error is proportional, not a rounding artefact.** On a two-day flight (Aug 17 → Aug 18) it
halved the plan: `daysBetween` returned 1, so a $100/day campaign that had correctly spent $200
was priced against a single day and reported `pct=200`, labelled `overspending`. A HIGH-priority
budget item raised against a campaign doing exactly what it was told.

**And it blanked the last day entirely.** `WindowDaysWithinFlight` clamped the window's end back
to a midnight already in the past, so on the final date the clamp pulled `we` to or before `ws`
and the function returned 0 — which `ComputePacing` reads as "no overlap" and reports as
`unknown`. Pacing was therefore unavailable for the whole of every flight's last day, the day an
operator is most likely to be looking at it.

**A single-day flight was never measurable.** With `start == end` the flight had zero length and
tripped the inverted-flight guard, so every one-day campaign was permanently `unknown`. It is a
valid schedule, and it now measures; the case was removed from the incomputable table, which had
been asserting the defect.

The fix is one helper, `flightEndInstant`, normalising the end to the following midnight, called
by **both** `ComputePacing` and `WindowDaysWithinFlight`. One function on purpose: the defect
existed because the two sites reasoned about the same column separately, and two copies could
drift apart the same way again. `nil` in, `nil` out — an absent end stays open-ended.

**The start needs nothing, verified rather than assumed.** Midnight of the start date is already
the instant the flight opens, so a campaign starting today elapses a genuine 0.5 days by noon and
the one-elapsed-day floor correctly declines to pace it. Shifting the start would have broken
that floor.

**Existing tests had encoded the off-by-one.** Nine unit tests and two endpoint tests failed on
the fix — all of them because their expected numbers were derived from the short flight, not
because the fix broke anything. They were repaired by shifting the flight bounds so each case's
stated intent survives (a "20-day flight" is now `-10..+9`), rather than by re-baselining the
assertions onto whatever the new code emitted. `TestWindowDaysWithinFlight_FlightEndsMidWindow`
asserted 3 days where the correct answer is 4, and was the clearest statement of the bug in the
suite.

**Mutation-tested, five ways.** Reverting the helper to the identity fails 13 tests; reverting
each call site *independently* fails only that site's tests (9 pacing, 3 window), proving neither
is dead code riding on the other; making the `nil` arm return a zero time fails the open-ended
cases; and overshooting by a second day fails 11. The package remains at 100% statement coverage.
