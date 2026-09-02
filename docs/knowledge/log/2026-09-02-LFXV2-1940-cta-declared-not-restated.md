# 2026-09-02 — LFXV2-1940: the CTA is declared once, not restated four times

**Fix** — the same CTA defect shipped three times in one day, each caught by a reviewer and each
"fixed" by editing the one line that was reported:

1. Final Countdown fell back to "Register Now" for readers who had already registered.
2. The fix made it omit the CTA entirely — but the schema requires `cta` and the service returns
   503 on an empty one, so the stage could not answer at all.
3. The fix for THAT missed the CTA ENFORCEMENT line, which still said "NO primary CTA".

The cause was structural. Each stage states its call to action in up to four hand-written places —
the numbered hierarchy, the ENFORCEMENT list, the validation checklist, and `CTAStrategy` — so
fixing one surface leaves the others contradicting it, and the model receives both.

`Template` now declares `PrimaryCTA`, `PrimaryCTAFallback` and `SecondaryCTA` as data.
`TestStageCTAPromptMatchesDeclaration` walks every CTA directive in the prose and fails when it
names a button that is not one of the three, with no prefix tolerance — accepting "Register" for a
declared "Register to Attend" would tolerate exactly the drift this exists to stop.

It found two live inconsistencies immediately: Schedule Announcement called one button "Register to
Attend" and "Register"; Registration Push called one "View All Options" and "View Options".

Verified by mutation against the ORIGINAL bugs, not just a synthetic one: restoring the
"Register Now" fallback fires 2 findings, restoring "NO primary CTA" fires 1, and drifting a single
checklist row fires 1. All three would now fail the build instead of reaching review.

A stronger version would GENERATE the prose from the declaration so drift is impossible rather
than merely detected. That is a larger refactor of all six templates and belongs in its own change.
