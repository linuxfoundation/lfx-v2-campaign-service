# 2026-08-18 — LFXV2-3283 the campaign geo path accepted any trailing segment

**Fix** — a resource name is the only proof of what a record IS, and the Search path was not
checking it.

`createCampaignGeoTargeting` extracted criterion ids with bare `resourceID`, which returns any
non-empty trailing segment. Every one of these was ACCEPTED and persisted as a successful geo
attachment:

| Response resource name | What it actually names |
|---|---|
| `customers/9999999999/campaignCriteria/222~900` | **another ad account** |
| `customers/1234567890/adGroupCriteria/222~900` | a different resource **kind** |
| `customers/1234567890/campaignCriteria/999~900` | **another campaign** |
| `garbage/4242` | not a resource name at all |

The Demand Gen path already did this correctly — `adGroupCriterionID` checks segment count,
resource kind, THIS client's customer id, and the composite shape, then verifies the returned
parent is the one that was asked for. `campaignCriterionID` is its sibling and the Search path
now uses it, so both paths agree and both return the criterion half alone.

**Both bots flagged this independently**, which is the strongest signal a finding gets. Worth
recording that an earlier pass of mine noticed the two paths returned DIFFERENT id shapes
(`222~900` vs `900`) and documented that as a deliberate design difference. It was not a design
difference; it was the symptom. Writing the asymmetry down as intentional nearly closed the
question instead of opening it.

**Two smaller fixes in the same pass.**

The untargeted-create WARN fired before input validation and before adoption, so a rejected
request or an adopted campaign — which may already carry targeting — logged a claim that a
campaign would serve worldwide when no create had happened. It now fires after the create
returns a campaign id, and logs the normalised channel rather than the raw caller value.

Two `httptest` handler captures were unsynchronised. The race detector did not flag them, but
that is timing rather than a guarantee, and every sibling test in this package guards its
captures. The retry counters matter more than convention: an unsynchronised counter can
UNDER-count, which would let the "a mutating create must never be retried" tests pass against an
implementation that does retry.
