# 2026-09-01 — LFXV2-1940: the unsupplied-fact guard was false-green

**Fix** — `TestPatternsDoNotAssertUnsuppliedCommercialFacts` split template text on periods alone.
These templates are newline-delimited directives, not prose: whole sections shared no period, so
they collapsed into one "sentence" and any stray `[PLACEHOLDER]` anywhere in that blob satisfied
the has-a-placeholder check for every claim inside it. The guard passed on a contradiction that
was genuinely present.

Splitting on newlines as well surfaced four real defects, each ordering the model to produce a
fact nothing supplies — no placeholder, so the OMIT rule could not remove them:

- **Post-Event** required a `Watch Recordings` primary CTA while the same template's CTA strategy
  said not to offer recordings without `[RECORDINGS_URL]`. Nothing supplies that URL, so the model
  had to violate one instruction or the other. Now conditional, falling back to `Share Feedback`.
- **Schedule Announcement** ordered "List 3-4 top speakers". Now gated on `[SPEAKER_NAMES]`.
- **CFP Launch** required talk types with examples — a selection POLICY, which invented tells a
  speaker their submission is judged against a rule the organisers never set. Gated on `[TALK_TYPES]`.
- **Final Countdown** required transit, parking and what-to-bring. Gated on `[TRANSIT_INFO]` and
  `[WHAT_TO_BRING]`.

The claim vocabulary was commercial-only (prices, codes, deadlines), which is why the speaker and
logistics families were invisible to it; editorial claims are now listed too. Three exemptions keep
it from crying wolf on its own scaffolding — structural directives (checklist rows, section
headings, ordering rules), explanatory prose describing the omit policy, and lines already gated by
a placeholder on the section heading above them.

Each of the five fixes was reverted individually and the guard caught every one.
