# 2026-08-17 — LFXV2-3064 discovery doc sweep

**Docs** — Five sites still described account discovery as a Google/Meta-only capability after
this ticket added the LinkedIn and Microsoft handlers. Raised by dealako in review round 7; two
were blocking because they state something now false rather than merely incomplete.

`docs/knowledge/code/internal-service.md` claimed **"Both handlers are three lines over one
`listAccounts` helper"** — there are four — and, worse, that a message pointing Microsoft at
`.../accounts` **"would prescribe a route that 404s"**. That was true before this ticket and false
after it. The constraint the paragraph exists to defend is still real, but it now survives on
Reddit and X alone; stating a correct rule in terms of a provider that has since gained the route
is how the rule ends up cited as evidence for a wrong fact.

`internal/domain/errors.go` had the same shape one level down, and its own comment records having
already been corrected once for a "sweeping form" that was false. The replacement enumerated Google
Ads and Meta — accurate then, falsified by this ticket. It now describes the AccountLister
providers as a SET, because an enumeration is falsified by the next provider added with nothing
failing to say so. That is twice this comment has gone stale by naming members; naming the shape
is what stops a third.

Also corrected: `internal-dispatch.md`'s discovery intro (named two implementations, there are
four) and a block comment in `internal/bootstrap/sysacct_test.go` asserting "All four lack
discovery" — the four remaining providers are no longer excluded for the same reason. Reddit and X
still lack discovery; LinkedIn has it but its create path names nothing, so it is excluded by the
OTHER half. Microsoft is excluded by NEITHER — `validateMicrosoftConnection` already tags
`ErrAccountNotSelected` on a path create reaches, so it has both halves and its absence from the
map is sequencing alone. Lumping it in with LinkedIn was the error this entry itself made.

**Tests** — `connection_accounts_test.go` covered the descriptor wiring for Google and Meta but not
for the two handlers this ticket adds. Every handler reaches the identical switch, so one wired to
another provider's `accountDiscovery` answers with the right STATUS and the wrong TEXT: a 404
naming google ads on a LinkedIn project, or a 400 telling a Microsoft operator to check
`access_token`, which a Microsoft credential does not carry. No status-code assertion can see it,
and the operator's next action is determined entirely by that text.

Mutation-verified both directions: pointing LinkedIn's descriptor at google ads fails the 404 case
with `message = "no google ads connection configured for this project"`, and swapping Microsoft's
remedy for Meta's fails on all four field names.
