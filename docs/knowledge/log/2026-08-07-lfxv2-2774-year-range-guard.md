# 2026-08-07 — LFXV2-2774: a four-digit `currentYear` was not narrow enough, and the past-editions filter inverted

**Update** — `ResolvePastEventNames` validated `currentYear` as "exactly four ASCII digits" while
`yearInName` could only ever EXTRACT a 19xx/20xx year from an event name. The two predicates
disagreed, and the gap between them turns the past-editions filter inside out.

**Fix** — The two out-of-range directions do NOT fail the same way, and only one of them is quiet.
ABOVE the range (`"9999"`, `"3000"`) the guard passes and every extracted year is strictly below
`currentYear`, so the `extractedYear >= currentYear` exclusion never fires: the filter does not fail
loudly, it INVERTS. "Past editions only" starts returning the current and future editions, and the
HubSpot audience built from them is silently wider than intended rather than absent. BELOW the range
(`"0000"`, `"0202"`) the comparison goes the other way — every edition is excluded and the resolve
fails closed with nothing. Both are wrong answers produced from an input the caller believed was
valid, so both are rejected at the door; it is the first that justified the change. The 19xx/20xx
range now lives inside the predicate itself (renamed `isSupportedYear`), so the comparison sites
cannot drift from the extraction sites again; the three duplicated copies in
`internal/platform/snowflake`, `internal/dispatch` and `internal/service` were each tightened, and
the now-redundant first-digit checks at the extraction sites were removed rather than left to
disagree a second time.

**Fix** — The first attempt at the predicate was itself too loose, and by exactly the mechanism it
was written to close. Checking only `s[0] != '1' && s[0] != '2'` accepts every year from 1000 to
2999 while the name, the error string, the doc comments and the log entry all said 19xx/20xx — a
validation contract that was false in its own commit. It now compares the full two-byte prefix, so
`"1899"` and `"2100"` are rejected as well, and the test carries those two boundaries specifically
because they are the ones a first-digit check gets wrong.

**Fix** — Three tests, one per copy of the predicate, because drift in any one of them would leave
the other two green. `TestResolvePastEventNames_RejectsOutOfRangeCurrentYear` asserts BOTH
directions — out-of-range years rejected, in-range boundaries still accepted — so the guard is a
range check and not a blanket reject. It deliberately does NOT call with `"9999"` and assert an
empty result set: that assertion would stay green if the range check were deleted and some unrelated
filter happened to empty the rows. `TestEventFamily` gained out-of-range details years (the details
field is hand-edited, so an unusable year is reachable there), and
`TestResolvePastEditions_OutOfRangeYearDoesNotReachTheResolver` pins the dispatch copy, where the
guard also gates the `yearIn(eventTerm)` fallback. Loosening the predicate back to a first-digit
check fails all three, in all three packages.

**Fix** — Tightening the predicate left its prose behind. The error string was updated to say
"4-digit 19xx/20xx year" but three comments around it still said "4-digit year", including one
asserting "we already validated it's a 4-digit year" — a comment that describes a guard as looser
than it is invites the next edit to loosen the guard to match. All three now name the range and say
why it is the range: it is the set `yearInName` can extract, so the comparison stays between two
values drawn from one vocabulary. `eventFamily`'s doc gained the case that matters — an
out-of-range details year is now zeroed rather than passed through, and the reason it must be is
that a wrong year excludes the wrong edition while a year above the range excludes NOTHING and
still looks like a successful build.

**Fix** — The finding came from Copilot on PR #70, against code that has since merged to `main` and
is no longer in that PR's diff. It is fixed here on its own branch rather than inside #70, because
adding warehouse files to a Google Ads metrics PR would recreate the scope problem the same review
raised one finding earlier.
