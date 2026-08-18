# 2026-08-18 — LFXV2-3314 two more ways pacing reported a failure as a measurement

**Fix** — a campaign in its first day, and a window that precedes the flight, both produced
confident HIGH-priority underspending items.

**The first day.** The `!now.After(start)` guard pinned the instant of launch, and then
`daysBetween`'s one-day floor invented a full day of expected spend one minute later. A campaign
launched 60 seconds ago reported 0% and raised an underspending item against itself. Two fixes:
elapsed time is now measured as a FRACTION of a day (`elapsedDays`) so expected spend scales
smoothly, and pacing is `unknown` below one elapsed day. The second is not redundant with the
first — a minute into a 30-day $1000 flight the honest expectation is two cents, and 0% of two
cents is arithmetically correct and operationally useless. Ad platforms also report spend with a
lag, so the first hours read as zero regardless of what the campaign is doing.

**The window's position.** `WindowDays` returned a day COUNT, which carries no information about
WHERE the window sits. `last_month` is 31 days, but for a campaign that started last week those
days lie almost entirely before the flight began, so a spend of zero over that window is correct
and means the campaign did not exist yet. `min(spendDays, elapsed)` aligned the two periods'
LENGTHS and never their OFFSETS. `WindowInterval` now resolves a window to an actual span and
`WindowDaysWithinFlight` intersects it with the flight; no overlap yields no pacing.

**The floor belongs on the divisor, not the numerator.** `daysBetween` keeps its one-day floor
for the flight's TOTAL length, where a zero would make expected spend infinite. Applying the same
floor to elapsed time inflated a numerator instead — the same function, correct in one position
and wrong in the other.

**This is the fourth and fifth door on one defect.** Future-dated flight, nil start date,
`start == now`, first day, window offset: every one produced a plausible number from a period
that was not the campaign's. The shape to check is whether the two sides of a ratio describe the
same span of time — in length AND in position.
