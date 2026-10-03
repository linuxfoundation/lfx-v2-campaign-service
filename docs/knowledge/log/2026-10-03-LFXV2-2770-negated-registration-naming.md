# 2026-10-03 — LFXV2-2770 check 4: "unregistered" contains "registered"

**Fix** — Review response on PR #240, Copilot's High finding. `CheckCurrentRegistrants`
gated on `containsAny(name, registrationHints)`, a substring scan, so it could not tell
this event's registrants from the people who have NOT registered.

**The consequence was inverted, not merely wrong.** "KubeCon 2026 - Unregistered
Prospects" matched the `registered` hint and failed QA as CRITICAL — the list a
registration-push send most obviously *should* include, reported as the list that must be
excluded. An operator who sees QA fail on the correct list learns to ignore QA. Nine such
names were enumerated and all nine matched: `Unregistered`, `Not Registered`,
`Non-Registered`, `Never Attended`, `Not Attendees`, `Deregistered`, `Pre-registration`,
`Unattended`.

**Why a token boundary alone is not the fix.** There are two causes, and a `\b` only
catches one. A negating prefix fused to the term (`Unregistered`, `Deregistered`) is a
boundary problem; a separate negating word before it (`Not Registered`, `Never Attended`)
is not — `registered` sits at a word boundary there. Fixing only the boundary would have
left half the class, which is the shape that manufactures the next review round.

**Why the negation has to be ADJACENT.** A first attempt looked for a negating word
anywhere in the name. It turned `Registrants (De-duplicated)` and `Registration - No
Discount` into undecidable names: both carry a negating word, negating something else
entirely. Seven such names are now in the test. The distinction is the lesson — asking
whether the negation attaches to the registration term is a question about **structure**
and converges; asking whether any negating spelling appears is a **denylist over
spellings** and does not. This same check already cost four such iterations on the region
question, each fix producing the next false positive.

**Any un-negated occurrence decides it.** `Not Registered - Event Registration List` names
both forms, and the list does hold registrants. Deciding on the *first* match instead reads
it as negated — pinned by its own case.

**NEEDS VERIFY, with its own message.** CRITICAL fails QA for the right list; a silent skip
hides the inclusion this check exists to catch. The existing undecidable text names the
*region* cause, so reusing it would have sent the operator to check the wrong thing — the
two causes are tracked separately and reported separately.

**Discovery is deliberately untouched.** Its two `registrationHints` call sites ask whether
a list carries a registration *signal*, which produces a label rather than a verdict.
Different question, different consequence, and nothing flagged them.

**Verification.** Both directions enumerated rather than sampled: 9 negated, 14 plain
(including the 7 a spelling denylist breaks), 3 with no mention. Three mutations — the
substring scan fails the NEEDS VERIFY test; first-occurrence fails the both-forms name; and
the negation-anywhere version fails in **both** directions at once, 4 false positives and 2
false undecidables. Build, vet and `gofmt -s` clean on the pinned Go 1.25.0.
