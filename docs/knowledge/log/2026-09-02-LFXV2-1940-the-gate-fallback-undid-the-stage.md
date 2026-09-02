# 2026-09-02 — LFXV2-1940: a CTA fallback undid its own stage

**Fix** — gating Final Countdown's "View Full Schedule" CTA on `[SCHEDULE_URL]` gave it a fallback
of "Register Now". Nothing supplies that URL, so the fallback fires on every request — and Final
Countdown is for people who have ALREADY registered. Its own SECTIONS TO REMOVE excludes
"Registration info". The gating fix therefore turned every Final Countdown email back into the
Registration Push it exists to follow.

The fallback is now to OMIT the primary CTA rather than substitute a contradicting one. Schedule
Announcement keeps "Register Now", where it is correct.

`TestFinalCountdownNeverAsksForRegistration` pins it. Deliberately narrow: a general
"stage contradicts its own removals" rule was written first and produced a FALSE finding on
Discount Offer, whose entry reads "Registration details -- only the discount matters" and bans a
SECTION while the stage's whole purpose is a discounted-registration CTA. The distinction lives in
prose written for humans; a guard that cannot read it accuses the wrong stage. The narrow test
also had to exempt prohibition lines, since the fix itself names registration in order to ban it.

Three attempts were needed before the guard bound: the first parser dropped multi-word removal
entries ("Registration info"), so it passed against the very bug it was written for.
