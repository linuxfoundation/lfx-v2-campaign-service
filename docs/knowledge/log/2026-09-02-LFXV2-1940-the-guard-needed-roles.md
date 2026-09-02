# 2026-09-02 — LFXV2-1940: a CTA guard that could not tell primary from secondary

**Fix** — `TestStageCTAPromptMatchesDeclaration` checked every CTA label against ONE combined set
of allowed phrases, so a label that is legitimate in one role passed in the other. Post-Event
declares `Share Your Feedback` as its secondary and `Share Feedback` as its primary FALLBACK; two
prose lines labelled the secondary `Share Feedback`, which the combined set accepted. When
recordings are unavailable the model was being told to write the same button twice.

The guard is now role-aware: a line mentioning "secondary" is checked only against `SecondaryCTA`,
everything else only against the primary and its fallback.

The directive pattern also required the literal word "CTA", which the enforcement lists do not
always use — `- 1 OPTIONAL SECONDARY: "..."` has no "CTA" in it. That is the SECOND blind spot of
this shape (the first needed "the" to be optional), so the pattern is now permissive about the
wording around the noun rather than matching one phrasing.

Between them the two fixes found **four** drifted labels, only one of which a reviewer had
reported:

- Post-Event enforcement said `Share Feedback` for the secondary (reported by Copilot)
- Post-Event checklist said `Share Feedback` for the secondary (found by the role fix)
- Final Countdown checklist said `Download App` vs the declared `Download Event App`
- Schedule Announcement enforcement said `Register` vs `Register to Attend`

Each is mutation-confirmed. A mechanical sweep of every CTA label against its declaration now
reports only placeholder tokens and prohibitions, so no real drift is left.
