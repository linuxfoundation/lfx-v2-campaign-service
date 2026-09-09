# 2026-09-09 — LFXV2-2775: the link rule is scoped to registration stages

**Fix** — the shared system prompt tells the model that every `href` in the body must be the
brief's Registration URL, copied exactly. That rule is appended for EVERY stage, and it is
absolute — but three stages render a call to action that asks for something other than
registration:

- **CFP Launch** renders "Submit Your Proposal".
- **Post-Event** renders "Share Feedback". That is the FALLBACK branch, and it is the one that
  actually runs: nothing supplies `[RECORDINGS_URL]`, so the declared "Watch Recordings" never
  executes.
- **Final Countdown** renders "See You There", a farewell — its purpose is to confirm attendance,
  sent 1-2 weeks out to people who have already registered.

Supplying the registration URL to those stages points a proposal button at a registration form, a
feedback button at registration for an event that already happened, and a confirmed attendee back
at a form they already completed. The brief carries one `url` column and no CFP-form or survey
field, so there is no correct destination to substitute.

`emailstage.Stage.LinksToRegistration` therefore WITHHOLDS the URL for those three, which reuses
the path the prompt already defines for a brief with no url: a plain-text call to action. A button
that is not a link beats a link to the wrong place.

**Note** — two things this turned up that were not in the original finding:

1. The reviewer named CFP Launch and Post-Event and proposed keeping Final Countdown linked.
   `TestStageLinkPolicyMatchesCTA` disagreed, and it was right: Final Countdown's declared
   "View Full Schedule" is gated on `[SCHEDULE_URL]` in its ContentPrompt prose rather than in the
   CTA text, so the running button is the "See You There" fallback. The first version of that test
   only looked for a `[` in the CTA string and missed prose-level gating.
2. Withholding the URL CHANGED THE SIZING ARITHMETIC. Post-Event is the worst stage floor and it
   now composes without the 19-rune `\nRegistration URL: ` line, so the worst valid composition
   fell from 8764 to **8744** and headroom rose to **556**. `TestConceptDocSizingArithmetic`
   computed that and failed on the stale prose immediately — the numbers were never transcribed by
   hand, which is why the drift surfaced in the same commit rather than three reviews later.

The input-size bound follows the same predicate: the URL is counted only when the stage formats it,
otherwise a CFP Launch caller could be refused with a 400 for a value their prompt never receives —
the same defect, one level down, as counting it on the frozen legacy path.
