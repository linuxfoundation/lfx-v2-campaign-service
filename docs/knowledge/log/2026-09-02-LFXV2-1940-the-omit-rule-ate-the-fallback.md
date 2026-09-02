# 2026-09-02 — LFXV2-1940: the OMIT rule could delete the CTA fallback

**Fix** — the system prompt's OMIT rule removes any sentence whose placeholder has no supplied
value, and it explicitly outranks the stage brief. Every gated CTA was written as one sentence:

    PRIMARY CTA: [ View Full Schedule ] with [SCHEDULE_URL], else [ See You There ]

`[SCHEDULE_URL]` is never supplied, so the model may drop that whole line — taking the fallback
with it and leaving no CTA at all, which the service refuses with a 503.

This is the **third distinct route to the same empty-CTA failure**: first a "Register Now" fallback
that contradicted the stage, then an explicit "omit the CTA" instruction, now a fallback sitting
inside a sentence the OMIT rule can delete. Each was found by a reviewer.

Every fallback now lives on its own line naming no placeholder, where OMIT cannot reach it.
`TestCTAFallbacksSurviveTheOmitRule` pins that. The review named ONE site (Final Countdown); the
guard found **seven more** across Discount Offer and Schedule Announcement.

Raising the composed bound to 9000 also missed `docs/api-catalog.md`, which still published 8400
to consumers. `TestConceptDocSizingArithmetic` now checks the catalog too — guarding one document
let the other publish a threshold the endpoint does not enforce.

The measured floor is now **6078** (Post-Event), worst composition **8478**, headroom **522**.
