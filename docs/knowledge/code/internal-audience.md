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

## One build at a time per (brief, platform)

Every list this package creates is a REAL, billable object in the HubSpot portal, and the
build makes them before it could possibly learn a sibling build is doing the same. They cannot
even collide by name: the plan's `BuildRef` is the audience row's own id, chosen so a later
build never adopts an earlier one's lists — which means two concurrent builds leave two
complete, indistinguishable sets and nothing downstream notices.

The lease closing that window is migration `000018`, a partial unique index over
`(brief_id, platform) WHERE status = 'building'`. The loser's insert is rejected by the
database and surfaces as `domain.ErrAudienceBuildInFlight`, a 409 in its own right rather than
the generic `ErrConflict`. The distinction is the instruction: "the resource already exists"
tells a caller to stop asking for something that exists, when nothing it asked for exists yet.
It must equally not be confused with the stale-approval 409, whose remedy is the opposite —
that one says refresh and rebuild, and rebuilding is precisely what duplicates the in-flight
build's lists.

**An index only serializes builds whose rows overlap, so WHEN the row is inserted is part of
the lease.** `BuildAudience` originally resolved past editions — a Snowflake round-trip, by far
the slowest thing it does — before inserting, and everything ahead of that insert is a window in
which a second request finds no row to conflict with. A double-click whose second request was
delayed there long enough for the first to finish would insert cleanly against a now-`built`
row and create a whole second set.

Moving the claim ahead of the warehouse read fixed the biggest window and left the argument
in the wrong shape: EVERY blocking call ahead of the insert is a window, and the next one
along — `briefs.GetBrief` — is a database round-trip rather than a Snowflake one. That is a
smaller window, not a bound, and "small enough" is a claim about latency that no test pins
and the next edit can quietly falsify. So the claim is now the FIRST thing `BuildAudience`
does after resolving its dependencies: the brief read, plan validation and the warehouse call
all happen under it, and the ordering stops depending on how fast the steps ahead of it
happen to be. `TestBuildAudience_SecondRequestIsRejectedWhileTheFirstReadsTheBrief` holds one
build inside its `GetBrief` and runs another to completion against it; concurrent repository
inserts cannot catch this, because in the broken ordering the two requests never reach the
repository at the same time.

Claiming before validating has a visible consequence, and it is accepted rather than worked
around: a brief that cannot be planned at all now leaves a released `failed` row where it
previously left nothing. Every early return between the claim and the first upstream call
therefore goes through `releaseUnstartedClaim` — nothing exists upstream on those paths, so
there is nothing to reconcile first, and a `building` row left behind by a request that gave
up would block every later build of the brief behind a 409 until an operator intervened.

One more thing had to move with it. The claim gates on approval itself, so a brief that was
never approved and a brief that moved mid-build both come back as `ErrStaleApproval` — a 409
about versions, which is right for the race and wrong for the ordinary case of someone
building a draft. `refusedClaimErr` re-reads the brief on the FAILURE path only and renders
that case as the 400 it was before, naming the status. A brief that cannot be re-read falls
through to the generic mapping: guessing 400 there would blame the caller for the service's
own inability to look.

A build that dies holding the lease keeps blocking rebuilds. That is intended: its lists exist
upstream, so the old answer of building again is what duplicated them. An operator reconciles
the portal FIRST and only then `PATCH update-audience`es the row to `failed`. That order is not
stylistic: failing the row frees the slot immediately, so doing it first admits the next build
while the dead build's lists are still in the portal — the duplicate set the lease exists to
prevent, arrived at by following the remedy for it. The 409's message states the reconciliation
first for the same reason.

**"Reconcile the portal" cannot mean "read `inclusion_summary`".** A row that is genuinely stuck
is the one least likely to have recorded anything: the claim inserts with an empty summary and
the ids are written only once `createPlanLists` returns, so the crash-mid-build case leaves real
lists upstream and an empty row. An operator who reads the summary, finds it empty and concludes
there is nothing to reconcile will fail the row and let the next build duplicate them — again
by following the remedy. The durable handle is the NAME: every list a build creates carries the
first 8 characters of its audience row id in parentheses (`Plan.BuildRef`, the same suffix that
stops a rebuild adopting an earlier build's lists), and that is true whether or not the row ever
recorded an id. The 409 and `docs/api-catalog.md` both name the prefix first and treat
`inclusion_summary` as a supplement.

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

`CreateAudienceForApprovedBrief` gates the insert on the brief being approved, locking the
brief row inside its own transaction, and reports back the VERSION it observed under that
lock. It takes no expected version, and that is the point: running first is what makes the
lease a bound, and running first means there is no earlier read for a caller to have pinned.
Same shape as `JobRepo.CreateJobForApprovedBrief`, which closed this race for campaign
creation, though not the same signature.

**The claim's gate and the pre-upstream re-check are not redundant.** Moving the claim ahead
of the warehouse read moved its approval gate there too, so on its own the gate now says the
brief was approved BEFORE the slowest call in the build rather than after it — which is
nothing at all about the brief at the moment lists are created, and that moment is what the
gate exists for. `confirmStillApproved` runs as the last thing before the first HubSpot call
and re-reads the brief, failing unless it is still approved at the version the claim locked.
The two guards answer different questions: the claim's gate SERIALIZES builds, this one DATES the
approval. A read failure here is reported as-is and never treated as "probably still fine" —
the caller is about to create real lists, and the only safe reading of "could not check" is
that the check did not pass.

**The re-check has to LOCK, which is why it is a repository operation.** Its first form read
the brief with `GetBrief` and compared in the service, and that cannot answer the question it
was asked. `GetBrief` is a plain `SELECT`, so under READ COMMITTED it returns the last
COMMITTED row: a `ReplaceBrief` that has updated the row and not yet committed is invisible
to it. The check would pass, the withdrawal would commit, and the lists would be created
from an approval the operator had already revoked — with nothing afterwards to tell those
lists from a legitimate build's. `BriefReader.ConfirmBriefApproved` does the read under
`SELECT ... FOR UPDATE` inside its own transaction, so the confirmation QUEUES behind such a
writer instead of reading around it and sees the writer's row once it commits. It does not
close the window (the lock is released when that transaction ends, and holding a transaction
open across an HTTP call to HubSpot is not an option) — what it removes is the
already-decided case, where the withdrawal has happened and merely has not committed yet.

**A brief that is not there is a 404, not the stale-approval 409.** The claim's gate cannot
find a missing or archived brief either, so it too refuses with `ErrStaleApproval` — and left
to that mapping the caller is told to refresh and rebuild a brief that does not exist, about a
race they were not in. Before the claim moved ahead of the brief read this was a plain 404, and
`refusedClaimErr` keeps it one by re-reading and treating `ErrNotFound` as the definite answer
it is. A brief that cannot be re-read AT ALL is different and still falls through to the
generic mapping: "I could not look" must not be reported as "it is not there."

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

## When the RECORD fails, the ids travel in the error

Created list ids normally live in `InclusionSummary` so orphaned HubSpot lists can be reconciled.
When the write that records them fails, the row stays `building` with an empty summary while real
lists exist — so `unrecordedListsErr` puts the ids in the returned error instead. The 500 body is
then the operator's only remaining handle, and the message says not to retry blindly.

Both exits need this, with different urgency:

- **Partial build** — some lists exist; the build failure and the record failure compound.
- **Successful build** — *worse.* Every inclusion list AND the master exist, so a blind retry
  duplicates the entire set. This path also had no safety net: `mapAudienceErr` has no case for a
  database error, so a pgx failure fell through to `default:` and returned a bare
  "an internal server error occurred" carrying nothing.

`createPlanLists` returns the master as the LAST element of `ids`, so `ids` already covers it —
re-appending `master` would name it twice and read like two separate orphans. The code guards on
`slices.Contains` rather than assuming, since that invariant is easy to break from the other side.

The two exits also use DIFFERENT error prefixes, because they blame different systems:
`audienceBuildErr` ("failed upstream") for the partial path, where HubSpot really did fail, and
`audiencePersistErr` ("created but recording them failed") for the success path, where HubSpot is
the one system known to be fine. Reusing the upstream wording there would send an operator to
investigate the platform when the remedy is to reconcile the listed ids.

## Every ambiguous outcome says UNCONFIRMED in the RESPONSE

`hubspot.IsUnconfirmed` classifies four ambiguous sources — a 2xx-with-no-id, a mutating 429, a
mutating 5xx, and a mutating transport failure. All four keep the row `building` (a list may
exist upstream), but only the first carried "verify before retrying" in its own message. The
other three surfaced as a plain 500 reading like an ordinary transient error, inviting the blind
retry that duplicates a list HubSpot may already have created. `unconfirmedNote` now annotates
the returned error for every ambiguous outcome, so the row state and the response agree.

## A brief with no location resolves editions BROADLY — recorded, not refused

`ResolvePastEventNames` omits its location predicate when `locationTerm` is blank, so the
warehouse matches the event FAMILY alone and a multi-city family ("Open Source Summit") can
resolve other cities' editions.

This is disclosed in the summary rather than prevented, because the containment is structural:
resolved names are only ever used ANDed with the host country (group 5) or the host region
(group 7), so a stray Milan edition cannot email people in Italy — it widens the audience to
prior attendees of the family who are *already in the target geography*. Refusing to build, or
degrading to country-only, would instead discard a correct returning-event audience every time a
brief omits an OPTIONAL field, which is the worse trade.

`PlanInput.EditionsUnnarrowed` therefore records the risk so an operator can audit the breadth,
set a location, and rebuild. A located brief carries nothing — a caveat on every audience would
stop being read.

It lands in `Plan.Caveats`, NOT `Plan.Notes`. The two render in different sections and mixing
them inverts their meaning: `Notes` renders under **"Not included"**, so filing this there would
announce that groups 5 and 7 are missing in the same summary that lists them as built. `Caveats`
renders under **"Caveats (these lists WERE built, with qualifications)"**, positioned directly
after the past-edition names it qualifies and before the inclusion lists, so the qualification is
read alongside the names it is about.

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
