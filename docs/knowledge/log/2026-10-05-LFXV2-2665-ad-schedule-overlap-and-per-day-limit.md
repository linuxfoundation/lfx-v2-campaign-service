# 2026-10-05 — Overlapping ad schedules, the per-day limit, and a geo warning that could not see proximity

**Fix** — Three preflight defects in the Google Search/Demand Gen create path, all
found by review on `#239`, all of the same shape: a locally decidable input error that
was left to Google, where it is only reported at the `campaignCriteria:mutate` — after
the budget and the campaign are already committed. That is exactly the orphan this
branch's preflight exists to prevent, so each one contradicted a comment this branch
itself had written.

## Overlapping intervals on one day

`validateAdSchedules` (`internal/platform/googleads/campaign_criteria.go`) now refuses
two intervals that overlap on the same normalised day. Google rejects them, and
`09:00-12:00` plus `11:00-13:00` is a typo a caller makes rather than an intent.

**The old rationale was wrong, not merely incomplete.** The doc comment said no overlap
check was attempted because a test "would have to decide whether 09:00-12:00 and
12:00-17:00 touch". The function already defines its windows as half-open, and the
half-open test answers that question correctly on its own: `startA < endB && startB <
endA` makes `12:00 < 12:00` false, so a split-day schedule is NOT an overlap. The
property the comment claimed was undecidable is decided by one line, and the comment
was removed rather than defended.

Comparison is on MINUTES OF THE DAY, the same form the empty-window check above already
uses, so `24:00` is 1440 and sorts after every real end. An exact repeat is collapsed
BEFORE the overlap test runs: a duplicate is one criterion written twice, not two
criteria that overlap, and reporting it as an overlap would name the wrong defect. The
over-refusal guard — adjacent windows, end-of-day windows, the same window on different
days — is pinned by its own test rather than left to the reader.

## The per-day limit the global cap could not express

`maxAdSchedules = 42` carried a comment claiming "nothing Google would accept is refused
here" because 42 is 6x7 and Google permits at most six intervals per day of the week.
The arithmetic was right and the code did not hold the property: a list of seven Monday
intervals and nothing on Sunday satisfies 42 and is refused upstream. `maxAdSchedulesPerDay
= 6` is the half of that ceiling the global cap cannot express, counted per normalised day
over the DEDUPLICATED intervals, so a repeat does not consume a slot.

Because the limit is Google's own, enforcing it refuses nothing Google would have
accepted; it only moves the refusal to where it costs nothing. 6x7 across all seven days
is still accepted, and a test asserts that the list it builds is exactly `maxAdSchedules`
long — the two constants are now pinned against each other rather than coincidentally
consistent.

## The "NO geo targeting" warning counted only one positive shape

`internal/dispatch/googleads.go` logs after a successful create when a campaign will
serve wherever the ad account allows. It checked `len(cfg.GeoTargets) == 0` alone, which
predates `proximityTargets`. A proximity-only campaign therefore logged that it had no
geo targeting while carrying a radius criterion that bounds its spend exactly as a
location criterion does — a warning that is not merely noisy but false, and an operator
who learns to distrust this warning is worse off than one who never had it.

The condition now counts every POSITIVE shape. `ExcludedGeoTargets` is deliberately NOT
counted: an exclusion narrows an otherwise-unbounded campaign without bounding it, so a
campaign carrying only exclusions still serves wherever the account allows minus a few
places, which is precisely what the warning exists to say out loud. The test proves both
polarities — a case where the warning MUST fire passes alongside the three where it must
not, so a silent capture harness cannot make the suite look green.

**Docs.** `docs/api-catalog.md` is the consumer-facing validation contract for
`CreateCampaigns.config` (typed `Any` in `design/`), so the overlap refusal, its
half-open semantics, the exact-repeat distinction and the six-per-day cap all landed
there. `internal-platform-googleads.md` and `internal-dispatch.md` carry the same two
corrections on the code side; the latter had no account of this warning at all.
