# 2026-09-02 — LFXV2-1940: the guard skipped lines containing any negative word

**Fix** — `TestPatternsDoNotAssertUnsuppliedCommercialFacts` skipped a sentence whenever a negation
appeared ANYWHERE on the line, so an affirmative directive with a negative aside passed. The line
`- 1-2 attendee testimonials (genuine quotes, not marketing)` is an ORDER to compose quotes, and
"not marketing" made the guard treat it as a prohibition. Nothing supplies testimonials, so the
model would have invented attributed quotes.

The negation test now runs on the sentence with parentheticals stripped, which is the directive
itself. Genuine prohibitions written as redirects keep working — "Pricing (belongs in Registration
Push email)" states its ban inside the aside — via a separate `redirect` pattern.

Three more sections were gated after the guard's claim vocabulary was extended to programme and
social-proof facts (`session title`, `tracks`, `schedule`, `testimonial`, `event highlights`):
Final Countdown's learning-track overview, Schedule Announcement's concrete session titles and
track breadth, and Post-Event's testimonials and highlight statistics. The `View Full Schedule`
primary CTA in two stages is the same shape as the earlier `Watch Recordings` one — a required CTA
pointing at a link nothing supplies — and now falls back to `Register Now`.

Also corrected the "compose, don't branch" invariant in `email_copy.go` and
`docs/knowledge/code/internal-service-email-copy.md`: the absent-stage legacy prompt IS a branch,
and an invariant with a silent hole is worse than one with a stated boundary.
