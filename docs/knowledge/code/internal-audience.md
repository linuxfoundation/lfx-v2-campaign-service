---
type: "Code Concept"
title: "internal/audience"
description: "Derives the regional-expansion inclusion lists (HubSpot filter trees) that make up a brief's marketing audience, plus the exploratory half an operator drives BEFORE committing to one: the deterministic discovery classifier, master-list naming and quarter ranking, name-search predicates, pre-send QA rules, and the transport-neutral results all of it is reported through."
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

The client documents these on `internal/platform/hubspot/lists.go`. The first three were violated
in the first cut; the fourth surfaced later, against the live API:

- **OR root, AND children, NO nested ORs.** Each past edition contributes its AND branches
  DIRECTLY to the single OR root — wrapping each edition in its own OR produced OR-of-OR.
- **`IN_LIST`, not `LIST_MEMBERSHIP`.** The latter is explicitly rejected.
- **Sibling filters inside one AND branch are ANDed.** So each list in the master needs its OWN
  branch; putting them side by side built an INTERSECTION (typically empty) rather than a union.
- **`operator` is required at the FILTER level too**, not only inside `operation`. Omitting it
  fails the whole create with `Some required fields were not set: [operator]` — a 400 that names
  no field path, so it reads as a malformed request rather than a missing key. Confirmed against
  the live API: the identical payload succeeds with it and 400s without.

Country values are also canonicalized through `DisplayName` before reaching a filter: the region
map keys are lowercase for case-insensitive LOOKUP, but `IS_ANY_OF` is an EXACT match, so raw
keys build a list matching nobody.

## The master list is a UNION

The email dispatcher sends to `platform_master_list_id` and nothing else, so the master MUST be
the union of the inclusion lists (`MasterListFilter`, a `LIST_MEMBERSHIP`/`IN_LIST` OR). An
earlier cut recorded the FIRST inclusion list as the master — groups 5 and 7 were then created
in the portal and never emailed, a build reporting success while reaching a fraction of the
intended people.

The exception is an audience ATTACHED from several existing lists (`include_list_ids`, migration
`000040`): there is no composed master, so the send set is the recorded include lists themselves
and HubSpot unions them (`contactIlsLists.include` is an array). `platform_master_list_id` then
holds the FIRST include id only as a display/compatibility value. Every reader that decides who an
email reaches goes through `model.CampaignAudience.SendListIDs()` — the include lists when any are
recorded, otherwise the master alone — never the master column directly, or a multi-list audience
would silently send to its first list. Corrupt include ids are an error, not a fall back to the
master. `UniqueIDs` (trim + order-preserving de-dup) and `ExclusionIDs` (suppressions minus the
include set) are the shared helpers for both the explorer and the service.

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

`releaseUnstartedClaim` calls `ReleaseAudienceBuildLease`, **not** `UpdateAudience`, and the
difference is the whole point. `UpdateAudience` is version-gated on the version the claim was
taken at, and a concurrent `PATCH` that leaves the row `building` while editing any other field
still bumps that version — so the release comes back `ErrPreconditionFailed`, is logged as a
best-effort failure, and the row stays `building` forever. That is the exact stranded lease this
path exists to prevent, so the release is gated on the STATUS instead, by the same statement that
performs it. `TestBuildAudience_ReleasesTheLeaseAfterAConcurrentPatchBumpsTheVersion` bumps the
stored version mid-build and asserts the row still lands in `failed`; against the version-gated
call it fails with `expected "failed", actual "building"`.

One more thing had to move with it. The claim gates on approval itself, so a brief that was
never approved and a brief that moved mid-build both come back as `ErrStaleApproval` — a 409
about versions, which is right for the race and wrong for the ordinary case of someone
building a draft. `refusedClaimErr` re-reads the brief on the FAILURE path only and renders
that case as the 400 it was before, naming the status. A brief that cannot be re-read is
reported as THE READ'S OWN failure — `mapAudienceErr(berr)`, not a mapping of the claim error:
guessing 400 there would blame the caller for the service's own inability to look, and falling
back to the 409 would be worse still (see below).

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

**A non-approved status seen AFTER the claim is a 409, not the 400.** The two look like the
same condition and are not, and the claim itself is what tells them apart: reaching this guard
means the claim SUCCEEDED, and the claim gates on approval, so the brief was approved when the
row was locked. Anything else observed now happened in the interval since — which is a
retraction mid-build, exactly what `ErrStaleApproval` names. Answering 400 ("approve the brief
first") would be a false accusation: the caller did approve it, and telling them to fix input
they got right sends them looking in the wrong place while the actual event — somebody
withdrawing an approval under a running build — goes unnamed. `refusedClaimErr` produces the
400 for the case that genuinely is pre-claim; this branch is its complement.

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
it is. A brief that cannot be re-read AT ALL is different: "I could not look" must not be
reported as "it is not there."

And it must not be reported as the 409 either, which is the sharper version of the same rule
and the one that was actually wrong. `ErrNotFound` was handled and every OTHER read failure —
the pool drops between the two calls, the deadline expires — fell back to mapping the claim
error, i.e. to `ErrStaleApproval`'s "the brief changed; refresh and rebuild". **A 409 here is a
factual claim about what somebody ELSE did**, and this function was making it on no evidence:
the read that would have shown a change is the read that failed. The switch now returns
`mapAudienceErr(berr)` for any `berr != nil`, so the 409 can only be produced from a read that
succeeded. `TestBuildAudience_UnreadableBriefIsNotAStaleApproval` pins it with a plain
`connection reset by peer`, which is deliberately not `ErrNotFound` — the bug lived entirely in
the errors that were not the one anybody thought about.

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

"Failed upstream" is `audienceBuildErr`'s DEFAULT, not its only message — it classifies the cause
first, and two arms name a different system. `ErrSystemConnectionNotUsable` and its siblings say
the shared LF connection is at fault and the remedy is an operator's, so one incident is not
scattered across every foundation that shares the row. `errPortalUnconfirmed` says the portal
lookup could not answer and the build refused BEFORE contacting HubSpot at all — calling that
upstream would send an operator to a platform this path never reached. Both exist for the same
reason the two prefixes do: the message decides which system somebody goes and looks at.

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

## Also here: the exploratory half (LFXV2-2770)

Every suppression-hint check reads the name with its separators normalised too
(`hasSuppressionHint`, used by `DiscoveryHidden`, `ClassifyList`, `MatchesBrandMaster` and
`MatchesEventSuppression`), so `Opt_Out` is an opt-out. `DiscoveryHidden` classifies test and
staff before master, so a test or staff master rollup ("… Master [test]") is hidden rather than
expanded as a master. `TestOrStaffList` applies the same test and staff hints to every
automatic pick (`MatchesBrandMaster`, `MatchesBrandOptOut`, `MatchesEventSuppression`), so a
test or staff list is never filled in as a gate or a suppression. `DominantBrand` votes a long-form brand ("The Linux Foundation") as its
short name from `brandAliases`, and only lists sharing a distinctive event word vote; when every
word of the event name is generic ("Open Source Summit Japan 2026"), a voter's list name must
contain the whole event name, so another event's lists sharing generic words cannot win.

The files above are the **record** half — `plan.go` and `filters.go` materialise the audience a
BRIEF commits to, and their failures fail the brief. The `builder_*.go` files are the
**exploratory** half: what an operator assembles and inspects before any brief commits to
anything. The split is deliberate and the two halves stay separate, because almost everything on
the exploratory side degrades to a partial answer with the gap NAMED rather than failing.

Everything here is pure — no HTTP, no credentials, no clock beyond an injected one — which is why
the orchestration that calls HubSpot lives in [internal/dispatch](internal-dispatch.md) instead.

- **`builder_types.go`** — the shared vocabulary. `Signal` treats `SignalUncertain` as a
  first-class value rather than an error state: silently dropping an unclassifiable list is how
  an audience loses a group nobody notices is missing. `ClassifiedSignals` deliberately excludes
  `last_sent` and `added` (provenance, which can never be "missing") and `uncertain` (the
  fallback). The caps and budgets are not preferences — each bounds a fan-out against a
  rate-limited API, and changing one changes how honest a result can be.
- **`builder_discovery.go`** — the classifier, entirely DETERMINISTIC. The model is used for one
  thing only, extracting the event's identity from its page (`EventExtractionSystemPrompt`),
  because a model that guesses which list to email is a model that can silently mail the wrong
  ten thousand people. `speakerScopeSuffixes` is ordered LONGEST FIRST so a narrower scope wins.
  `DiscoveryQueries` probes the full exact event name first and only then name+year, because a
  bare brand acronym fills all twenty search slots with wrong-year noise.
- **`builder_master.go`** — naming and ordering. `QuarterCode` is computed in UTC deliberately,
  and `EventQuarterCode` falls back to the current quarter because a name missing its code sorts
  below every dated list. Blank segments are DROPPED from a master-list name, not blanked, and
  `CombinedSuppressionName` suffixes the operator's own base name so the pair lands adjacent
  alphabetically in the portal. `QuarterRank` returns `(-1, -1)` for an undated list — ranked
  below every dated one rather than treated as recent.
- **`builder_resolve.go`** — predicates, separate from the probes that find candidates, so both
  test without a HubSpot client. HubSpot's list search matches LOOSELY, returns at most twenty
  hits, and does not order by recency, so a probe proposes and a predicate decides. An EMPTY
  keyword set matches nothing on purpose: treating "no evidence" as "matches" would accept every
  suppression list in the portal. `MatchesEventSuppression` requires keyword overlap AND a
  suppression word, because either alone also matches the event's own AUDIENCE lists — and
  excluding those would suppress exactly the people the send is for.
  `LastSentTerms`/`MatchLastSent` are the same shape of decision for the last-sent listing, and
  they exist because matching the event name as ONE contiguous phrase found nothing: no marketing
  email is named `"KubeCon + CloudNativeCon North America"`. `NewLastSentTerms` splits the
  year-stripped name into DISTINCTIVE and GENERIC tokens (`genericEventWords`, curated beside
  `stopwords` on the same rule — a token belongs there only when it recurs across DIFFERENT events),
  and keeps the brand apart as a fallback tier. Membership in that set is the WHOLE definition of
  distinctive, so it has to cover ordinary English words and not just event-format ones: while it
  held only formats, `"Open Source Summit"` left `{open, source}` admitting alone and returned
  `"Registration Open"` and `"Source Code Newsletter"` as precedent — and each false hit, being a
  non-brand match, also DELETED the brand fallback rows that were the honest answer. Brand words
  stay OUT of it even though they recur, because subtracting `linux` would empty the fallback tier
  for every Linux Foundation event. `MatchLastSent` reads name AND subject, because
  either can carry the event, and admits on ONE distinctive token or TWO tokens of any kind. A flat
  minimum-overlap count was rejected: `"KubeCon NA 2026"` overlaps a KubeCon event on exactly one
  token and is the precise case this exists to find, so DISTINCTIVENESS rather than count is what
  separates `kubecon` from `summit`. A generic token still RANKS (`Overlap` counts both tiers); it
  just cannot admit alone, since `"Summit Recap"` shares `summit` with a dozen unrelated portfolio
  sends. A brand-only hit is reported AS one (`BrandOnly`) so the caller can demote it, and a brand
  token that is also an event token is removed from the brand tier — otherwise the fallback claims
  credit for a hit the event name already explains. `LastSentTerms.IsEmpty` reports that no tier
  holds a token, which is the one answer callers must check BEFORE sweeping a portal: `event_name`
  is constrained only by `MinLength(1)`, so a name that year-strips to nothing (`"2026"`) or whose
  every token is dropped as ≤2 characters (`"NA EU"`) empties the event's own two tiers, and an
  empty term set matches no row by design. It reads all THREE tiers, so such a name is only
  "nothing to search on" when `brand_short` is degenerate or absent as well — a usable brand
  leaves the fallback tier populated and the sweep worth running. `KeywordOverlap` was removed
  with this rule: its only remaining reference was a log fragment, its godoc described a ranking
  role `MatchLastSent` had taken over, and a scoring helper that looks usable but is not is how
  the contiguous-phrase defect shipped in the first place. `SharesKeyword` remains, and is the
  live boolean form.
  `MatchesStandardSuppression` compares NORMALISED names — lowercased, quarter tokens (`26Q1`,
  `Q1 2026`) and years stripped, separator runs collapsed, a trailing `list`/`lists` dropped —
  and requires equality, not containment. A raw comparison missed every portal whose hygiene lists
  are named `"Unsubscribed - Lists"` or `"GDPR_Suppression 26Q1"`, so the standard suppressions
  resolved as "not found in this portal" while existing.
  `StandardSuppressionTerms` also carries the three Linux kernel contact lists
  (`Kernel Domain Contacts`, `Persona Linux Kernel`, `LF kernel.org Contacts`), matched the same
  way, so `26Q2-Kernel-Domain-Contacts` and `Q1 2026 - LF - kernel.org Contacts` resolve and next
  quarter's rebuilds do too.
  `MatchLastSent` also refuses ANOTHER EDITION of the same series: `editionRegions` reads the
  region an event or email names (`editionRegionAliases`, token → edition region — not
  `region.go`'s `countryToRegion`, which maps MARKETING regions and would fold Japan into APAC), and
  a row that names regions none of which is the event's is dropped before scoring. A row naming no
  region is kept, so an unlabelled send is never lost. Without it, the location words the
  distinctive tier admitted (`japan`, now in `genericEventWords` with the other place names)
  returned every Japan event's sends as precedent for one of them.
- **`builder_qa.go`** — three pre-send checks, inferred from a list's own `filterBranch` plus the
  NAMES of the lists it references, because the portal carries no machine-readable marker for
  "this is the GDPR list". `NEEDS VERIFY` is the honest and most common verdict, and no caller
  may read a `PASS` as authorization to send. `Severity` ranks a finding by legal exposure, not
  tidiness, and a `Finding`'s `Fix` is never empty.
- **`builder_results.go`** — transport-neutral results, defined here so orchestration tests need
  no generated types and a change to the generated contract cannot silently change what
  orchestration promises. `Size` is a `*int64` throughout because an absent size is not zero;
  `DiscoveryOutcome.Inspected` is reported so a capped run is visible AS capped; a
  `SuppressionRow`'s `Key` identifies the ROW and not the list, so an unresolved GDPR row shows
  as unavailable instead of vanishing; and `ListBrief.Missing` is carried explicitly because a
  deleted list still leaves its id on the email that targeted it.
- **`builder_errors.go`** — the failure vocabulary, living beside the results rather than in the
  orchestration, because importing the orchestration package would close a cycle through its
  tests. `ComposePartialError` unwraps to BOTH `ErrComposePartial` and the underlying cause: the
  master create is unconfirmed (compose creates no suppression list of its own any more), which is
  a state an operator must be shown rather than a failure to retry blindly.
- **`builder_standard.go`** — the standing suppression lists: `StandardSuppressionName` (one per
  project, no date or fingerprint, so every audience reuses it), `CurrentRegistrantsName` and
  `CurrentRegistrantsFilter` (the edition's registrants via the registration custom event),
  `SplitStandardSuppressions` (substitute the standing list only for a COMPLETE standard
  selection), `SameIDSet`, and `MasterListWithSuppressionsFilter` (one `NOT_IN_LIST` per excluded
  list on every inclusion path).

See [internal/audience](../../../internal/audience).

## Custom audiences: the model proposes, this package builds (LFXV2-2770)

`builder_custom.go` and `builder_custom_interpret.go` are the "build from scratch" path, ported
from the legacy audience builder's Custom Audience tab. There, a model read a free-text request
AND wrote the HubSpot filter JSON AND called the create. Here the model does only the first:

- **`CustomInterpretSystemPrompt` / `ParseCustomDraft`** turn the request (plus answers to
  earlier questions) into a DRAFT `CustomCriteria` and the questions that still block it. The
  reply is decoded with unknown fields REFUSED — a model inventing `gate_list_id` must not have it
  silently ignored — while one unusable filter row is dropped with a note rather than costing the
  whole draft. The model never supplies list ids (the gate and any speakers list are resolved
  against the portal) or property types (read from the portal's definitions). Operator text is
  quoted inside `BEGIN/END` blocks with the markers neutralised, exactly as the event-page prompt is.
- **`CustomListFilter`** is the only thing that builds the filter, and it is pure and table-tested.
  The shape is the reference list's (`26Q3 - Seoul - Mailable Contacts - …`): an OR root of AND
  branches — `city`/`ip_city` (+ `country_dropdown`/`ip_country` when broadened), each gated by
  the brand master list; and event-history branches qualified INSIDE the `UNIFIED_EVENTS` node on
  the event's own `event_city`/`user_city` (+ country), gated only when `GateEventHistory` is set.
  `RegisteredFor` adds a branch for the named editions' registrants (an event draft with a location —
  cities or countries plus `contact_location`, `event_registered` or `education_enrolled` —
  defaults to the warehouse's past editions), gated like the other event history when `GateEventHistory` is set.
  An audience of roles or properties alone has one base branch, which carries the gate under
  `GateEventHistory` too. `AllBranchesGated` reads these base branches rather than restating the
  rules, so preview's upper bound cannot drift from what is built.
- **Restrictions reach EVERY branch.** Extra property filters are ANDed into each branch; the
  speaker restriction is an `IN_LIST` on an existing speakers list or, absent one, each branch is
  copied once per speaker signal the portal's registration event defines
  (`registration_type CONTAINS Speaker`, `is_event_speaker_bool`, `is_event_speaker`), each with
  the operation type its portal property requires (HubSpot has no `CONTAINS_TOKEN`), because
  HubSpot will not take an OR nested inside a branch; ambassadors copy each branch once per programme property. A restriction on only some
  branches is an OR'd way around it. Copies clone their slices so appending to one cannot alias
  another. The expansion is capped at `customBranchLimit` (60).
- **No suppression on a custom list.** It is an inclusion building block: suppressions apply once,
  at the composed master or at send time, and baking them in would apply them again wherever the
  block is reused (the legacy rule "single-signal lists never carry their own suppression").
- **Fail closed.** `ValidateCustomCriteria` refuses an ungated contact-location branch (a city
  filter alone selects everyone who ever typed that city, mailable or not), a speaker restriction
  with neither a list nor event names (every speaker of every event), an unknown operator, and a
  value operator with no values. Every refusal wraps `ErrInvalidRequest` with operator-facing text.
- **Naming.** `CustomListName` follows the reference pattern
  (`<YYQN> - <Location> - Mailable Contacts - <Label>`, `<Event> Speakers - <Scope>`,
  `<YYQN> - <Event> - Event Registration`). A taken name is versioned `(v2)`, `(v3)` and never
  reused: a matching name does not prove a matching audience. Every derived or versioned name is
  cut (on a rune boundary) to `CustomNameMaxLen`, the input's 250-rune limit, so the name preview
  returns is one create accepts back. More than three places are named as
  `A, B, C and N more`, and any name over `MaxListNameLength` (200, HubSpot's list-search limit) is
  refused as `ErrInvalidRequest` before the taken-name search, which would otherwise fail as a 500.
- **Speakers-list auto-pick fails closed.** `SpeakerListFits` decides whether an existing list
  may stand in for a speaker restriction; a filled `ListID` REPLACES the scope and event-name
  filters, so it accepts a list only when its name provably fits: a speakers list discovery would
  offer, naming every requested edition (NO editions means no fit — nothing can be checked), and
  of the requested scope. A scope suffix (` - Current`, ` - Past`, ` - Current + Past`) must equal
  the requested scope; `current_past` — the parser's default — is met ONLY by that explicit
  suffix, since an unsuffixed name says nothing about which editions it covers; `current`/`past`
  refuse a name that mentions the other scope. Anything else is left for the operator.
- **Countries are canonicalized on entry.** `CanonicalCountries` maps each value through
  `DisplayName` (and collapses aliases of one country) where criteria enter the service —
  `ParseCustomDraft` and the service's `customCriteria` — exactly as the brief path's `BuildPlan`
  does, so "USA" filters on "United States" instead of a literal that matches nobody.
- **Draft arrays stay inside the contract.** `ParseCustomDraft` caps value arrays at 25,
  `property_filters` at 10 (`customMaxFilters`) and `page_view_urls` at `CustomMaxPageViewURLs`
  (10), each matching its `MaxLength`, and drops, with a note, a suggested property filter whose
  name is outside the contract's `^[A-Za-z0-9_]+$` or longer than 100 characters. A comparison
  value is kept whole up to 2000 characters (`customValueMaxLen`) or dropped with a draft note,
  never cut to a prefix, since a shorter value matches more contacts. Producers of
  `registered_for` honour `CustomMaxRegisteredFor` (25), so an unedited draft passes preview. The
  `internal/apivalidation` drift test decodes a body at and one past each cap through the
  generated validator, so a Go cap drifting from its `MaxLength` fails there.
- **Gate resolution.** `BrandMasterProbes`/`MatchesBrandMaster` find the brand's STANDING master
  list (`… - Master-list`), never a per-event `… - Master`, which is one event's audience and the
  wrong gate for a location list. A short brand matches only as a whole token.

Both legacy pieces the first cut left out are now ported: the Education brand-fit topic filter
(`EducationTopics` / `EducationTopicProperty`) and the REUSE/UPDATE/UNRELATED negotiation
(`CustomReuseSystemPrompt` / `ParseCustomReuse`) — see below.

**Added 2026-10-10.** Three more criteria close the legacy "Create list" gap for every missing
signal:
- `EducationTopics` narrow education enrolment to the brand's own courses with a CONTAINS filter
  inside every education `UNIFIED_EVENTS` node; with no location they are the education signal on
  their own. The property they filter on (`EducationTopicProperty`) is picked from the portal's
  education event definition by `CourseTopicCandidates` (course > topic > subject > program >
  curriculum, by name or label), over STRING properties only, since the topic filter is always a
  string CONTAINS. A caller's value is kept only when it is one of the portal's
  `CourseTopicCandidates`, otherwise the best candidate replaces it; the model's choice is never
  used. Topics with no such
  property are refused rather than silently dropped, because dropping them widens the list to
  every enrolment.
- `PageViewURLs` add one ungated `PAGE_VIEW` branch per URL (`HAS_PAGEVIEW_CONTAINS`, `pageUrl`).
  `filter.MarshalJSON` omits `operation` for that filter type only; every other filter's wire
  shape is unchanged. A fragment shorter than 8 characters is refused, since it matches every
  page that mentions it. A URL is kept whole up to 2000 characters (`customURLMaxLen`) or dropped
  with a draft note, never shortened, since a prefix matches other pages too. The PAGE_VIEW shape was confirmed by a live create (list 31588).
- **Reuse before create.** `CustomReuseProbes` searches by location/label, `ReusableCandidate`
  drops masters, suppressions and test lists, and the model ranks the survivors
  (`CustomReuseSystemPrompt` / `ParseCustomReuse`: reuse / update / unrelated). It ranks only
  what the search found — an id it returns that was not a candidate is dropped. The ranking
  prompt carries the operator's answers and the settled restrictions (speakers, ambassadors,
  property filters), which narrow the request. A property filter is described by its property and
  operator only: its values can be a real contact's email or phone, and the prompt goes to an
  external model. `GuardReuse` then holds any "reuse" of a list whose name does not show those
  restrictions to "update": a speakers list must be the one already selected, or one
  `SpeakerListFits` accepts for the requested scope and editions, and a property filter never
  shows in a name, so with one no list is "reuse". Its "is this the same event" check uses only
  DISTINCTIVE label words: the audience's own cities and countries and generic event words
  ("summit", "tokyo") are dropped, so another event's list in the same city is never kept. A
  label with no distinctive word left ("Open Source Summit Japan") must instead appear whole, year
  aside, in the list's name, and only a label of two or more event words can match this way: a
  one-word label ("Seoul") never matches by name, since as a bare substring it would match another
  event's list in the same city. Course topics are not a restriction: every education audience carries them as its brand
  fit, so counting them would make reuse unreachable (2026-10-15).

**QA and send-time suppression (2026-10-12).** `CheckExclusionCompleteness(filters, sendTime)`
counts suppression lists the send applies as well as the list's own `NOT_IN_LIST` filters. Custom
lists (and lists attached without a composed master) carry no suppression by design, so before
this every one of them failed QA despite going out suppressed.

