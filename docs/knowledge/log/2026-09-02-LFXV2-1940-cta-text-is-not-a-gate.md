# 2026-09-02 — LFXV2-1940: CTA button text counted as a gate

**Fix** — the always-supplied blind spot was closed by stripping `[EVENT_NAME]`/`[LOCATION]`/
`[DATES]` and then testing for any remaining `[`. That leaves one shape uncovered: CTA button
text. `[ Register Now ]` and `[ View Full Schedule ]` name no fact at all, but they survive the
strip, so a claim could ride in gated by nothing but its own button label.

Not reachable in today's templates — all three CTA lines carry a real `[SCHEDULE_URL]` or
`[RECORDINGS_URL]` beside them — but it is the same shape as the headline bug one layer down, so
it is closed rather than noted.

A gate must now be a placeholder TOKEN: no spaces inside the brackets, and not one of the three
always-supplied names. Mixed-case tokens still count (`[Session title 1]` gates as much as
`[SCHEDULE_URL]`). Confirmed by mutation: a claim left gated only by `[ View Full Schedule ]`
fires exactly one finding.

Also corrects two comments describing an empty stage as resolving through `emailstage.Resolve`.
It does not — an absent or blank stage returns the frozen legacy prompt and never reaches Resolve;
only a non-empty unrecognised value falls through.
