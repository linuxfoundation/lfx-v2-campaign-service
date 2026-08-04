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

## Post-create writes are detached

Once the HubSpot lists exist, the row recording them is written on a context DETACHED from the
request. A client disconnect between the final create and that write would make pgx skip it,
orphaning a real master list while the row still reads `building` — a build that succeeded on
the platform and looks failed in the database. Same reasoning as the orchestrator's post-create
persist, bounded so it cannot hang shutdown.

## The event NAME decides the edition year

`eventFamily` takes the year from the event name when it has one, and only falls back to the
brief's `year` detail. A detail year that disagrees with the name is self-defeating: for
"KubeCon Korea 2026" with a stale `year=2025`, the warehouse query keeps 2026 in the search term
while excluding 2025, so the CURRENT edition comes back as a "past" one and the audience is
built from people who already registered. The details field is hand-edited and can go stale; the
year inside the name cannot disagree with the name.

## Only approved briefs — checked ATOMICALLY

`BuildAudience` rejects a brief that is not approved, but the check alone is not enough: past-
edition resolution is a warehouse round-trip, so a concurrent `ReplaceBrief` can reset the brief
to draft and bump its version in that window. The plain `CreateAudience` only gates on
`status <> 'archived'`, so the build would then create REAL HubSpot lists from a stale approved
snapshot.

`CreateAudienceForApprovedBrief` gates the insert on the brief still being approved AT the
version observed by the check, returning `ErrStaleApproval` (→ 409) otherwise. Same shape as
`JobRepo.CreateJobForApprovedBrief`, which closed this race for campaign creation.

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

## "No warehouse" and "broken warehouse" are DIFFERENT degrades

Both leave `PastEditions` empty, but they warrant opposite operator conclusions, so `PlanInput`
carries `PastEditionsErr` and `BuildPlan` writes a different note for each:

| Condition | Note | Means |
|---|---|---|
| Snowflake unconfigured, or the event genuinely has no prior edition | "No past editions resolved… expected for a first-time event" | benign, final |
| Snowflake CONFIGURED but unusable, or the query failed | "could NOT be resolved… NARROWER THAN INTENDED — rebuild it once the lookup succeeds" | an outage; the audience is incomplete |

The boot-time client construction error is therefore **carried, not just logged**
(`dispatch.NewDegradedAudienceBuilder` → `AudienceBuilder.snowErr`). Dropping it left a nil
resolver, and a nil resolver answers `(nil, nil)` — identical to "unconfigured". A rotated
`SNOWFLAKE_PRIVATE_KEY` would then make a returning KubeCon lose its entire past-registrant
audience while the build reported success and stored the first-time-event note. The boot log
rotates away; `InclusionSummary` is the durable record, so the distinction has to live there.

## A partial build whose RECORD also fails returns the list ids

The partial-build path records created list ids in `InclusionSummary` so orphaned HubSpot lists
can be reconciled. When that write ALSO fails, the row stays `building` with an empty summary
while real lists exist — so the ids are appended to the returned error instead. The 500 body is
then the operator's only remaining handle on what the build left in the portal, and the message
says not to retry blindly (a retry without the ids duplicates every list).

## `titleCase` decodes runes, not bytes

Country display names feed `IS_ANY_OF`, an EXACT match. Slicing the first BYTE would split a
non-ASCII name (Türkiye, Côte d'Ivoire) mid-rune into mojibake that matches nobody, with no error
at any layer. Every key in the region map is ASCII today, but the map invites additions.

List names are **event-scoped AND build-scoped**. HubSpot list names are portal-global, so the
runbook's bare "Education Enrolled [Country]" collides between two events in the same country —
and event-scoping alone still collides between two BUILDS of the same brief, which is supported
(rebuilds, revised targeting). A collision is not loud: the create is rejected, or the new
audience silently adopts the older build's lists and the master points at stale membership.

The discriminator is the audience row id, so the row is created BEFORE the plan is finalised.
The plan is validated first, so a brief that cannot be planned leaves no row behind.

See [internal/audience](../../../internal/audience).
