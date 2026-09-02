# 2026-09-02 — LFXV2-1940: a stage that could not satisfy its own contract

**Fix** — removing Final Countdown's "Register Now" fallback left "otherwise OMIT the primary CTA"
as the always-taken branch, because nothing supplies `[SCHEDULE_URL]`. The shared JSON schema asks
for `cta` unconditionally and `GenerateEmailCopy` rejects an empty one with a 503, so the stage
became unanswerable: obey the brief and the response is refused, obey the schema and violate the
brief. The fix for one contradiction created a worse one — the first made a wrong claim, this made
the stage fail outright.

Final Countdown now falls back to `[ See You There ]`, which names only the event itself. Discount
Offer had the same shape one step earlier — its CTA read `Register with Code [PROMO_CODE]` with no
fallback, so with no code supplied the model would emit the bracket or invent a code — and now
falls back to `Register Now`.

`TestEveryStageCanProduceACTA` pins it: no stage may instruct the model to omit the CTA, and every
stage must declare a `CTAStrategy`. Mutation-confirmed — restoring the omit instruction fires
exactly one finding.

Two notes for the next editor:

- The template bodies are Go RAW STRING literals. Writing a backticked word inside one terminates
  the literal and the package stops compiling. Say "cta field", not the backticked form.
- `TestFinalCountdownNeverAsksForRegistration` matches on the stem `regist`, so explanatory prose
  containing "already registered" trips it. Phrase the reasoning without that word.
