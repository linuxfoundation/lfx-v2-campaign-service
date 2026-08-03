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

## Filter-shape invariants (HubSpot rejects violations)

The client documents these on `internal/platform/hubspot/lists.go`, and all three were violated
in the first cut:

- **OR root, AND children, NO nested ORs.** Each past edition contributes its AND branches
  DIRECTLY to the single OR root — wrapping each edition in its own OR produced OR-of-OR.
- **`IN_LIST`, not `LIST_MEMBERSHIP`.** The latter is explicitly rejected.
- **Sibling filters inside one AND branch are ANDed.** So each list in the master needs its OWN
  branch; putting them side by side built an INTERSECTION (typically empty) rather than a union.

Country values are also canonicalized through `DisplayName` before reaching a filter: the region
map keys are lowercase for case-insensitive LOOKUP, but `IS_ANY_OF` is an EXACT match, so raw
keys build a list matching nobody.

## The master list is a UNION

The email dispatcher sends to `platform_master_list_id` and nothing else, so the master MUST be
the union of the inclusion lists (`MasterListFilter`, a `LIST_MEMBERSHIP`/`IN_LIST` OR). An
earlier cut recorded the FIRST inclusion list as the master — groups 5 and 7 were then created
in the portal and never emailed, a build reporting success while reaching a fraction of the
intended people.

## Only approved briefs

Building creates real HubSpot lists and makes a brief sendable, so `BuildAudience` applies the
same lifecycle guard as campaign creation: a draft or archived brief is rejected before anything
is resolved or created.

## Fail closed vs fail soft

- **Closed** where an empty result is dangerous: an empty event-name set would emit a filter
  matching everyone who ever registered for anything, so it errors instead.
- **Closed** on an unknown country in `RegionFor` — defaulting would widen a real audience to a
  continent the event has no reach in.
- **Soft** when a country has no region mapping, or the warehouse is down: the country-scoped
  lists are still valid, so the build produces a narrower audience and records why.

List names are **event-scoped AND build-scoped**. HubSpot list names are portal-global, so the
runbook's bare "Education Enrolled [Country]" collides between two events in the same country —
and event-scoping alone still collides between two BUILDS of the same brief, which is supported
(rebuilds, revised targeting). A collision is not loud: the create is rejected, or the new
audience silently adopts the older build's lists and the master points at stale membership.

The discriminator is the audience row id, so the row is created BEFORE the plan is finalised.
The plan is validated first, so a brief that cannot be planned leaves no row behind.

See [internal/audience](../../../internal/audience).
