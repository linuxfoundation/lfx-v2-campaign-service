# 2026-08-07 — LFXV2-2774: a four-digit `currentYear` was not narrow enough, and the past-editions filter inverted

**Update** — `ResolvePastEventNames` validated `currentYear` as "exactly four ASCII digits" while
`yearInName` could only ever EXTRACT a 19xx/20xx year from an event name. The two predicates
disagreed, and the gap between them turns the past-editions filter inside out.

**Fix** — With `currentYear = "9999"` the guard passes, and every extracted year is then strictly
below it, so the `extractedYear >= currentYear` exclusion never fires. The filter does not fail
loudly — it INVERTS: "past editions only" starts returning the current and future editions, and the
HubSpot audience built from them is silently wider than intended rather than absent. `"0202"`,
`"0000"` and `"3000"` fail the same way. The 19xx/20xx range now lives inside the predicate itself
(renamed `isSupportedYear`), so the comparison sites cannot drift from the extraction sites again;
the three duplicated copies in `internal/platform/snowflake`, `internal/dispatch` and
`internal/service` were each tightened, and the now-redundant first-digit checks at the extraction
sites were removed rather than left to disagree a second time.

**Fix** — `TestResolvePastEventNames_RejectsOutOfRangeCurrentYear` asserts BOTH directions: the four
out-of-range years are rejected, and four in-range ones are still accepted, so the guard is a range
check and not a blanket reject. It deliberately does NOT call with `"9999"` and assert an empty
result set — that assertion would stay green if the range check were deleted and some unrelated
filter happened to empty the rows. Reverting the predicate fails all four rejection cases.

**Fix** — Tightening the predicate left its prose behind. The error string was updated to say
"4-digit 19xx/20xx year" but three comments around it still said "4-digit year", including one
asserting "we already validated it's a 4-digit year" — a comment that describes a guard as looser
than it is invites the next edit to loosen the guard to match. All three now name the range and say
why it is the range: it is the set `yearInName` can extract, so the comparison stays between two
values drawn from one vocabulary. `eventFamily`'s doc gained the case that matters — an
out-of-range details year is now zeroed rather than passed through, and the reason it must be is
that a wrong year excludes the wrong edition while an out-of-range one excludes NOTHING and still
looks like a successful build.

**Fix** — The finding came from Copilot on PR #70, against code that has since merged to `main` and
is no longer in that PR's diff. It is fixed here on its own branch rather than inside #70, because
adding warehouse files to a Google Ads metrics PR would recreate the scope problem the same review
raised one finding earlier.
