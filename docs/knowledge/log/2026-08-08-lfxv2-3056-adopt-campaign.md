# 2026-08-08 — Adopting a campaign that already exists upstream

**Update** — `POST /projects/{project_id}/briefs/{brief_id}/campaigns/adopt` binds an
existing ad-platform campaign to an approved brief. It is the consumer for the by-id lookup
added in [LFXV2-3054](2026-08-08-lfxv2-3054-googleads-get-campaign.md). The mechanics are in
`docs/knowledge/code/internal-service.md`, `internal-dispatch.md` and
`internal-infrastructure-postgres.md`; this entry records the decisions behind them.

## The gap it closes

Every campaign operation this service offers — metrics, toggle, delete — resolves the campaign
through its stored row. A campaign launched in the Google Ads console before a foundation
onboarded, or during an outage, has no row. The only previous route to a row was `POST
.../campaigns`, which CREATES upstream; using it to register an existing campaign would produce
a second paid campaign beside the first.

## Why not `UpsertCampaign`

Reaching for the existing upsert would have been the small change, and it is the wrong one. Its
`DO UPDATE` arm is correct where it is used: the row it overwrites describes the same campaign
this service is provisioning, which is how a retried dispatch converges. Adoption's caller names
an **arbitrary** upstream campaign, so the same arm silently repoints a live binding, orphaning
the campaign it used to name — still running, still spending, and this service never pauses or
deletes upstream on its own. Freeing a wrong binding is already an explicit operation
(`DELETE`); adoption must not become a second, implicit one.

## Why 404 and 503 are not two flavours of failure

An operator told **"no such campaign"** concludes it is not there and creates one. If the real
situation was "we could not reach Google Ads", they now have two campaigns spending against
one budget. A false absence costs money; a false 503 costs a retry. That asymmetry is why the
fail-closed contract runs through all three layers rather than being a convention at one of
them, and why `TestAdoptCampaign_UnverifiableIsNeverReportedAsAbsent` exists.

## The status field was a real defect, caught before commit

The first version stored `ref.Status` — `"ENABLED"` — into `Campaign.Status`, this service's
LIFECYCLE vocabulary, where both `CampaignStatusDeletable` and
`CampaignStatusNeedsReconciliation` default-DENY an unknown value: the row would have been
undeletable *and* never reconciled. The fix went a level deeper than the assignment —
`model.PlatformCampaignRef` now carries no status at all, because adoptability is the
ADAPTER's decision, in the platform's own vocabulary. It surfaced only because the test
asserted `res.Status == "ENABLED"`, which is what asking "what would break if this were
wrong" buys over "does this pass".

## Round N+1: the arm a copied switch left out

The error switch was written against the metrics and toggle switches and is one arm short:
`domain.ErrSystemConnectionNotUsable` was missing. It is reachable — `LookupCampaign` resolves
through `resolveGoogleAdsClient`, whose deferred `systemScoped` **wraps rather than replaces**
— so `errors.Is` still reported `ErrConnectionNotUsable` and the general arm answered a 409
telling the caller to repair a connection their project does not have. The sentinel exists to
prevent exactly that misdirection, so omitting the arm made the tag decorative.

Three more of the same shape came out of the bot pass, and all three are about what a NEW write
path forgets rather than what it gets wrong. The adopted row did not carry the account it was
verified under, so `googleAdsCreationCustomerID` read "unknown" and the mismatch guards — which
treat unknown as permission to proceed — stopped protecting it. It did not stamp `created_by`
or `updated_by`, so an adopted campaign had no audit trail. And a locally-rejected malformed id
fell through to the 503 default, telling the caller to retry input that can never succeed. Each
is a column or an arm that every SIBLING path already has; none is visible from the new code
alone. The lesson is the same one twice over: diff a new write against the existing writes for
the same table, and a new error switch against the existing switches, field by field.

## Round N+2: the guards a new write inherits from the database, not from Go

Three more, and they rhyme with the round above without being the same lesson. Two were about
atomicity rather than a missing field. Approval was checked before a platform lookup bounded at
20 seconds and never re-checked, so a concurrent replace or archive inside that window left paid
spend bound to an unapproved brief — the approval gate beaten by latency alone. `job_repo.go`
already carries a long comment explaining exactly this and the `SELECT … FOR UPDATE` that fixes
it; the new path simply did not reach for it. And 000013's unique index answers "does this BRIEF
have a campaign here", which is the whole question for dispatch and only half of it for
adoption: the caller names an ARBITRARY upstream campaign, so a second brief could bind the same
one and the two rows would toggle it against each other, each individually well-formed. That
needed a second index (000020), scoped by project because a bare platform id is unique only
within the account that minted it.

The third was a claim, not a defect. The PR said an adopted campaign was "immediately toggleable
like any other"; the Google Ads toggle refuses ACTIVATE without the ad-group, ad and keyword ids
that prove targeting exists, and adoption never walks the campaign's children. The fix was to
narrow the documented contract rather than to widen the code — chasing the children would mean
verifying provisioning this service did not perform, and the guard is right to refuse. Worth
recording because the instinct on a "your docs overstate this" finding is to make the code match
the doc; here the doc was the thing that was wrong.

The through-line for the round: when a new write path reuses an existing table, the invariants it
must uphold are not all visible in Go. Some live in indexes and row locks that a sibling path
established, and reading only the sibling's Go code will not find them.

## Round N+3: a fallback that is safe everywhere except here

The credential fallback to the LF system account has been in this service for a long time and is
correct for every path that had it: each of those names a campaign the service already holds a
project-scoped ROW for, so the row is the authorization and it does not matter that several
projects share one ad account underneath. Adoption is the first endpoint where the caller names
an ARBITRARY upstream id, and that single difference turns the shared account into a
cross-project hole — project A binds project B's console-created campaign, then reads its spend
and pauses it. Every guard that looks like it should catch this misses for a structural reason:
the row-scoped guards on metrics and toggle check a row A legitimately owns, and the
account-mismatch guard compares two customer ids that are the same id.

The fix is a refusal, not a check, because there is nothing to check against. A campaign's name,
labels and budget are all set by whoever created it, so no upstream field is evidence of which
project owns it. `resolveOwnedGoogleAdsClient` therefore rejects `resolved.fromSystem` outright,
and the refusal costs nothing real: a project with no ad account of its own has no campaign of
its own to adopt.

Worth recording as a class. When a shared resource is safe because *every existing caller happens
to supply a scoped key*, that safety is a property of the CALLERS, not of the resource — and the
first caller that supplies an unscoped key inherits none of it. The audit question for the next
endpoint is not "does this reuse a vetted helper" but "does this reuse it with the same shape of
input the vetting assumed".

The generalisable part is WHICH arm went missing. Copying a switch reproduces the arms that fire
in ordinary testing and drops the one for a condition nothing local produces; both remaining arms
returned a `ConflictError`, so the code did not look incomplete. The new test asserts the response
TYPE rather than its message, which is what makes the deleted arm fail rather than fall through.

## Round N+4: three ways a guard can be present and still not guard

All three findings this round are the same shape — a protection that exists, reads as
sufficient, and is not.

**The echoed id.** The persistence rule was "record the id the platform returned, never the one
requested", and there was a test named for it. The reasoning was that recording the response
faithfully beats recording the request, which is true and beside the point: when the two
DISAGREE, faithfully recording the response means binding a brief to a real paid campaign
nobody named, and answering 201. The mismatch case had been reasoned about as a storage
question when it is an authorization question. `LookupPlatformCampaign` now refuses it, as
UNVERIFIABLE rather than absent — nothing in a response about campaign Y establishes that
campaign X is missing, and 404 is the answer an operator resolves by creating a duplicate. The
adapter already errors on an unhonoured id filter; this is the same check in the layer that
owns the contract, which is where it survives an adapter being added or rewritten.

**`IF NOT EXISTS` on 000020.** Every other index in this chain carries the clause, which is
exactly why it was written without a thought. A failed `CONCURRENTLY` build leaves an INVALID
index holding the name; the version goes dirty; the operator forces back and re-runs; the
clause sees the name and skips; version 20 records CLEAN over an index that enforces nothing,
and adoption starts binding one upstream campaign to two briefs silently. 000013 has the same
clause and is safe — but only because 000014 follows it with an `indisvalid` guard. Copying the
clause copied half a mechanism. Nothing follows 000020, so its guard has to be the ABSENCE of
the clause, and a test now keeps it absent, because the next person to read that line will
want to add it back.

**"No migration, so no ordering constraint."** The PR's own merge note, contradicted by the
`allowedVersionGaps` entries in the same PR — which exist precisely because 000020 sits above
two versions that are still on unmerged branches. Merge order is #106, then #103, then this.

The class: **a guard copied from a sibling inherits the sibling's context, not its safety.**
Each of these three was correct where it came from. The question to ask of a copied line is not
"is this what the neighbours do" but "what made it sufficient there, and is that thing here".


## Round N+5: a guard that no test could distinguish from its own absence

Three findings, all from Copilot's suppressed block — which is to say `unresolved=0` was true and
meant nothing.

**A missing connection answered 503.** `resolveOwnedGoogleAdsClient` refuses the LF system
fallback, because adoption must bind a campaign the project itself can reach. It handled the
`fromSystem` case and returned every other `resolve` error unchanged — including the one where
NEITHER the project nor the system scope has a connection, which `credsSource.resolve` reports as
a wrapped `domain.ErrNotFound`. That fell to the adopt switch's `default` arm: a 503 saying the
platform could not be reached. Nothing was ever contacted. The caller retries a permanent
condition, and the operator reads a network problem into a project that was simply never
connected. It now maps to `ErrAdoptionRequiresOwnConnection` and answers 409, the same permanent
refusal as the `fromSystem` case — the fallback distinction "use LF's instead" has no meaning on a
path where LF's is refused anyway. Every other `resolve` failure still passes through, because a
repo error and an unusable connection really do want different remedies.

**"Could not be reached" was false for most of what reached it.** The same `default` arm also
catches an unhonoured id filter, an undecodable row, a status outside the known set — cases where
the platform answered perfectly well and the ANSWER is the problem. The message now says the
campaign could not be *verified*, which is both true of every case and the only thing the caller
needs before retrying. `docs/api-catalog.md` says the same.

**The core guarantee had no test that could fail.** Migration 000020 stops one upstream campaign
being bound to two briefs. It was covered by a fake repository and by regexes over SQL source —
both of which assert what someone believed the index does. Neither can catch the failure that
matters: a subtly wrong predicate or key applies cleanly, passes every unit test, and lets two
briefs control one paid campaign, after which each brief's toggle and metrics reader act on it
independently and the rows stay individually well-formed. There is no runtime symptom to find
later. `dbtest/adopt_binding_live_test.go` now checks the real migrated schema, with one sub-test
per way the definition can be wrong, each verified by making that exact edit to the migration and
watching only its own sub-test fail.

The class: **the first two findings are the same mistake as the third.** A 503 that means "we
never tried", a message that names connectivity for a response-shaped defect, and an index test
that cannot fail — each is a signal that carries no information, and in all three cases the code
looked complete precisely because something was present in the place the signal belonged.

## Round N+6: a gate placed one step too late

Copilot, on `resolveOwnedGoogleAdsClient`: adoption refused the LF system fallback by inspecting
`resolved.fromSystem` on the value `creds.resolve` returned — but `resolve` LOADS, VALIDATES and
DECRYPTS the system row before it returns anything. An LF connection missing its credential blob,
or one that no longer decrypts, therefore comes back as `ErrSystemConnectionNotUsable` *instead
of* a value, and the gate never runs. The caller gets a 500 blaming an LF row for a request whose
remedy is "connect your own ad account", about a row adoption would have refused in perfect
health. Verified by reverting to `resolve`: `err = credentials came from the LF system
connection: … has no stored credentials`, where the 409 belongs.

The fix is not another sentinel arm. `credsSource.resolveOwned` consults the project's scope and
nothing else, so the refusal no longer depends on the fallback's state at all — and no future
failure mode of the system scope can leak onto this path and need a new arm. The strongest new
assertion is not about any sentinel: `Get(model.SystemProjectID)` must never be called. That one
covers the failure modes nobody has enumerated; the sentinel assertions each cover only the case
they name.

Removing the fallback made the service's `ErrSystemConnectionNotUsable` arm unreachable, and it
had a test — which passed, because `TestAdoptCampaign_ConnectionDefectsAreDistinguished` injects
the error directly into the adopter fake. A table test over a switch asserts what the switch does
with an input; it says nothing about whether anything can produce that input. Both the arm and
its case are gone, with the reasoning recorded in the test's own comment so the case is not
"restored" later as an apparent oversight.

The class, and it is the same one as Round N+5 on #102: **a guard is only as strong as the point
at which it runs.** There the redactor split before deciding whether splitting was safe; here the
ownership check ran after the thing it was refusing had already been fetched and decrypted. In
both cases the guard was present, correct in isolation, and one step too late to hold.
