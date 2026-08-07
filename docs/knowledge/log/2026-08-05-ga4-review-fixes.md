# 2026-08-05 — GA-4: review fixes and migration hardening

**Update** — Resolve outstanding Copilot review thread on PR #69 GA-4
(`internal/platform/googleads/targeting.go:295`).

`adGroupCriterionID` validated the resource kind (`adGroupCriteria`) and the composite
`{adGroupId}~{criterionId}` shape, but never checked that the resource name's customer
segment matched this client's own account — unlike `validateCampaignResource`, which
rejects a campaign resource from a different account. A malformed/substituted 2xx
naming another customer's adGroupCriteria could otherwise be accepted as this call's
own criterion and its id persisted. `adGroupCriterionID` is now a `*Client` method that
also checks `pathParts[1] == c.account.CustomerID`, returning `("", "")` (classified
UNCONFIRMED by the caller) on a mismatch, mirroring `validateCampaignResource`.

**Update** — Resolve outstanding Copilot review thread on PR #69 GA-4
(`internal/platform/googleads/adgroup_ad.go:148`).

`adGroupCriterionID` accepted a resourceName with `len(pathParts) < 4`, unlike its
sibling `adGroupAdID` and `validateCampaignResource`, which both require EXACTLY 4
path segments. A resource name with extra path segments (a malformed/substituted
response) was silently accepted, with the extra segments ignored rather than
rejected. Changed the check to `!= 4`, matching `adGroupAdID`.

**Update** — Cursor Bugbot correctly flagged that `000013_rebuild_stuck_claim_index.down.sql`
(added in the entry directly below) unconditionally ran `DROP INDEX CONCURRENTLY IF EXISTS
idx_campaigns_stuck_claims`. On the common path, `000013`'s up is itself a no-op (`IF NOT
EXISTS`, since `000008` already built a valid index), so `000013` doesn't actually own the
index — rolling back only that version would drop an index that `000008` (still applied) is
relying on, leaving stuck-claim scans without their partial index. Changed `000013`'s down to
a no-op (`SELECT 1`), mirroring `000009`'s down for the same ensure/repair-semantics reason.

**Update** — Cursor Bugbot correctly flagged that the "migration concurrency" fix below (item 2 of the
same day's earlier entry) was itself broken: putting the DO-block DROP and
`CREATE INDEX CONCURRENTLY` in the same file means golang-migrate's pgx/v5 driver sends both
statements to Postgres in one implicit transaction (it does not wrap migrations in an explicit
transaction, but multi-statement files are still batched as one transaction block by Postgres
itself), and `CREATE INDEX CONCURRENTLY` cannot run inside a transaction block — the migration would
fail on apply. 000008's own comment already documents this constraint explicitly ("Do NOT add other
statements to this file"). Split 000009 back down to just the DO-block DROP (single statement), and
moved the `CREATE INDEX CONCURRENTLY IF NOT EXISTS` rebuild into a new migration file,
`000013_rebuild_stuck_claim_index.up/down.sql` (numbered 000013 rather than immediately after
000009 to avoid colliding with the `000010`-`000012` index-outbox chain from PR #60), mirroring
000008's single-statement, non-transactional shape.

**Update** — Renumbered `000013_rebuild_stuck_claim_index.up/down.sql` → `000015`. PR #64
(campaign delete) has since merged into `main`, claiming `000013_campaigns_partial_unique_platform`
and `000014_drop_campaigns_full_unique_platform` — versions this branch could not see when it
picked `000013` for the rebuild file above. `000015` is the next free version above every
migration now present on `main`. Updated the two in-file comments in
`000009_drop_invalid_stuck_claim_index.up.sql` and `000015_rebuild_stuck_claim_index.down.sql`
that referred to the rebuild migration by its old number.

**Update** — Closed 4 unresolved Copilot comments on GA-4 (`internal/platform/googleads/targeting.go`,
`docs/api-catalog.md`, `internal/infrastructure/postgres/migrations/000009_drop_invalid_stuck_claim_index.up.sql`,
`internal/container/container.go`):

1. **customAudiences preflight rejection** — This client always creates SEARCH campaigns, which do
   not support Custom Audiences per Google's documentation (limited to Display, Demand Gen, Gmail,
   Video, and Performance Max). Updated `validateAudienceSegments` to reject `customAudiences`
   resource names with a clear error message before any Google Ads requests are made. Updated
   `docs/api-catalog.md` to document the restriction and remove `customAudiences` from the
   supported audience types. Updated `targeting_test.go` to reject custom audiences and verify the
   error message.

2. **Migration 000009 blocking behavior** — The recovery rebuild was using plain `CREATE INDEX`
   inside a DO block (transaction), which blocks campaign inserts/updates/deletes during the
   entire build. Migrations run during rolling startup when other replicas are still active,
   stalling claims/finalization and manufacturing ambiguous outcomes. Refactored to keep the
   conditional INVALID-index drop inside the DO block (safe, as the index is not serving queries)
   and moved the CREATE INDEX statement outside to use `CREATE INDEX CONCURRENTLY IF NOT EXISTS`,
   matching the approach in 000008. The rebuild is now non-blocking.

3. **Container shutdown timeout budget** — The sweeper-stop wait (`sweeperStopTimeout`) was not
   included in `ContainerCloseTimeout`, so if the wait reached its 250ms bound, the orchestrator's
   post-cancel grace phase (reserved for detached campaign-persist and job-finalize writes) would
   be shortened below its documented window. Extended `ContainerCloseTimeout` to include
   `sweeperStopTimeout`. Updated the container_test.go assertion to verify the new budget.

**Update** — Closed 5 suppressed Copilot findings on GA-4 (`internal/dispatch/googleads.go`,
`internal/platform/googleads/targeting.go`):

1. **Real bug** — `ToggleStatus`'s ACTIVATE path wrapped a campaign-mutate failure with
   `wrapUnconfirmed(uerr)`, which only classifies ambiguous 5xx/timeout/transport outcomes as
   Unconfirmed and lets a definite 4xx pass through as a clean error. But on ACTIVATE the
   children (ad group, ad) are mutated FIRST — by the time the campaign mutate runs, a failure
   of any kind (including a definite 4xx) is a genuine partial cascade: the children already
   changed, the campaign's outcome is unknown. Changed it to wrap unconditionally via
   `&unconfirmedToggleError{err: uerr}`, mirroring the PAUSE path's existing child-after-campaign
   wrap. Added `TestGoogleAds_ToggleStatus_ActivateCampaignDefiniteFailureIsUnconfirmed` to pin
   this — a fake server returns a definite 400 on `campaigns:mutate` after the two child mutates
   succeed, and the test asserts the returned error is `Unconfirmed() == true`.
2. Fixed a misleading comment on the ACTIVATE keyword-provisioning guard that claimed it checked
   "if both are empty" when only `KeywordCriteriaIDs` is checked (audience criteria alone are
   observation-only and don't satisfy the gate).
3. Fixed a stale comment in `targeting.go` that implied `createAdGroupTargeting` sets the ad
   group's `targetingSetting` itself — it relies on the ad group create having already set it
   (see the 2026-08-04 GA-4 targeting-level entry).
4. Fixed two stale comments in `targeting_test.go` still describing campaign-level
   `targetingSetting` plumbing that moved to the ad group create.
5. Fixed `docs/api-catalog.md`: "campaign's `targetingSetting`... on create" → "ad group's
   `targetingSetting`... on the ad group create".

Deferred (test-coverage gaps, not correctness bugs, left for a follow-up): no integration test
for the `customAudience` targeting serialization branch (`targeting.go:263`, only `userList` is
exercised end-to-end); no full-success-path ACTIVATE cascade test reaching a successful campaign
mutate (only the failure path is covered by the new test above).
