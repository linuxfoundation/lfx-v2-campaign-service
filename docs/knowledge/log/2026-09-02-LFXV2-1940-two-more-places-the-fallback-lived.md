# 2026-09-02 — LFXV2-1940: the countdown fallback lived in two more places

**Fix** — the "Register Now" fallback was removed from Final Countdown's prompt body but survived
in `CTAStrategy` ("Primary: View Full Schedule -- ... else Register Now") and in the validation
checklist, which required a schedule CTA unconditionally. `CTAStrategy` is appended to the system
prompt, so it reintroduced exactly the contradiction the body fix removed.

Both guards had blind spots that let these through, and both are closed:

- `TestFinalCountdownNeverAsksForRegistration` required the literal word "CTA" on a line. A
  `CTAStrategy` entry never contains it — the field IS the call to action. Those entries are now
  checked wholesale.
- `isStructuralDirective` exempted every `□` checklist row as formatting. A row reading
  `□ One primary CTA: "View Schedule"` is a REQUIREMENT for a button pointing at a link nothing
  supplies. The box no longer confers the exemption on rows that mandate a specific CTA.

Both confirmed by mutation. The trimmed wording was also forced by
`TestComposedBoundClearsEveryStageFloor`: the first, wordier fix pushed Final Countdown to 6047
runes, so the worst valid composition reached 8447 against the 8400 bound and valid caller input
would have been refused with a 503. The bound test caught a regression introduced by a fix.
