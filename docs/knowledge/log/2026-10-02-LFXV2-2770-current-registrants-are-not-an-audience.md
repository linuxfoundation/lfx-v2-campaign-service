# 2026-10-02 — an event's own registrants are a suppression, not an audience

**Fix** — the audience QA audit had no check for the one mistake that defeats a
registration-push send: inviting people who have already registered.

Observed on AGNTCon + MCPCon North America (2026-09-30, project `tlf`). The edition's own
registration list — `26Q2 AGNTCon + MCPCon North America 2026 Event Registration`, 1,346
contacts — was offered under Event Registration as an **include**. Ticking it raised no
warning and the audience QA still passed.

The three existing checks cannot catch it. `CheckSignalMapping` asks whether the filters select
on something a contact DID (a registration list passes emphatically), `CheckSuppression` asks
whether the consent suppressions are applied, and `CheckExclusionCompleteness` asks whether the
exclusions are well-formed. None reads the audience's own INTENT, which is where this lives.

**The check** — `CheckCurrentRegistrants` is check 4. It flags an INCLUDED list that carries a
registration signal AND belongs to this edition.

**Three conditions, and the first two versions were wrong.** A check that fires on the common
case gets switched off, so the predicate was probed against real list names before it was
wired:

| predicate | past edition | sibling region |
| --- | --- | --- |
| event tokens alone (`MatchLastSent().Matched`) | FAIL ✗ | FAIL ✗ |
| \+ require the event's year | PASS | FAIL ✗ |
| \+ require the FULL token set | PASS | PASS |

Including a PAST edition's registrants is correct — it is the strongest evidence a first
edition has. A sibling region (`AGNTCon + MCPCon Japan 2026`) is a different event. Both had to
keep passing.

`NewLastSentTerms` strips the year deliberately, because it exists to FIND past editions, so
matching on its terms alone flagged exactly the lists that are right to include. The GENERIC
tier carries the region, so requiring every token is what separates NA from Japan: measured,
the NA list scores overlap=4 while Japan scores 2. And the year is not redundant to that — a
2024 edition carries the identical token set and scores 4 as well.

**Absence is NEEDS VERIFY, never PASS.** QA can be run on a bare list id, where the event is
genuinely unknown, and an event name with no year cannot be told from its own history. Both
return NEEDS VERIFY with a finding naming what a human has to confirm. A silent pass there
would be indistinguishable from an audit that looked and found nothing.

`event_name` is a new OPTIONAL attribute on `run-audience-qa`, and `current_registrants` is
OPTIONAL in the result — omitted rather than zero-valued when the check did not run, so a
client cannot read an empty verdict as a pass.

## Tests

Eleven cases in `builder_qa_test.go`. Each of the three conditions is mutation-verified against
its own case: reverting to `.Matched` fails the sibling-region case, dropping the year check
fails the identical-token-set case, and returning PASS for an absent event name fails the
no-event-name case.
