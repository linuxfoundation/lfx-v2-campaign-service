---
type: "Code Concept"
title: "internal/audience"
description: "Derives the regional-expansion inclusion lists (HubSpot filter trees) that make up a brief's marketing audience."
resource: "internal/audience"
---

# internal/audience

Turns a brief's event details into the HubSpot list definitions that constitute its audience.
Until an audience reaches `built`, the **email channel cannot send at all** — the HubSpot
dispatcher refuses any brief whose audience is unbuilt or carries no master list.

## Ported from an operational runbook, not invented

The model comes from `hubspot-event-list-builder`, the runbook that produced these audiences by
hand. Only its DETERMINISTIC half is mechanised:

| Group | List | Needs |
|---|---|---|
| 4 | Education Enrolled *(country)* | the country only — always buildable |
| 5 | Event Registered *(country)* | past-edition names from Snowflake |
| 7 | *(region)* Event Registrants | past editions + a mapped region |

**Group 6 (Expanded Web Visitors) and domain-fit narrowing are deliberately NOT built.** They
need judgement — the *nearest* sibling event by date and brand family; which course topics suit
this event — and `BuildPlan` records them as not-built in the audience's `InclusionSummary`. A
plausible-looking wrong audience is worse than an absent one: it sends real email to real people.

## Details that silently produce an empty list

None of these fail loudly. HubSpot accepts the request and builds a list matching nobody, which
is indistinguishable from a correct empty audience:

- **Event names are matched INSIDE the `UNIFIED_EVENTS` node**, not as sibling property filters.
  A sibling filter tests a *contact* property named `event_name`, which does not exist.
- **`eventTypeId` values are portal-wide constants** (`6-58204655` education, `6-48984571`
  registration). The runbook is explicit that looking them up per event returns per-event ids
  that match nothing.
- **Names must come from `snowflake.ResolvePastEventNames` verbatim.** A guessed or remembered
  name matches nothing.
- **Country filters OR `country` with `ip_country`.** The two disagree often enough that
  filtering on either alone measurably shrinks the audience.

## Fail closed vs fail soft

- **Closed** where an empty result is dangerous: an empty event-name set would emit a filter
  matching everyone who ever registered for anything, so it errors instead.
- **Closed** on an unknown country in `RegionFor` — defaulting would widen a real audience to a
  continent the event has no reach in.
- **Soft** when a country has no region mapping, or the warehouse is down: the country-scoped
  lists are still valid, so the build produces a narrower audience and records why.

List names are **event-scoped**: HubSpot list names are portal-global, so the runbook's bare
"Education Enrolled [Country]" collides between two events in the same country.

See [internal/audience](../../../internal/audience).
