# 2026-10-05 — A second campaign on the same brief and platform (slot versions)

**Update** — An operator could not create a second campaign for a brief on a platform it
already had one on: `ClaimCampaignDispatch`'s `ON CONFLICT ... DO NOTHING` on
`(brief_id, platform, variant)` lost the claim, and `isReusableCampaign` returned the FIRST
campaign as a success. The service could not tell "another one" from "retry".

`create-campaigns` now takes `new_version`. Migration `000035` adds
`campaigns.slot_version` (default 1, `CHECK >= 1`), `000036` adds the four-column partial
unique index, and claim / upsert / adopt write through it. The latest live row is what
`GetCampaignByPlatform` returns and what a retry retries; `new_version` claims `latest+1`
only when the latest is complete. Versions are internal: Microsoft folds the slot version into
its opaque name suffix (`brief-id-2`) because it treats a matching name as the same campaign;
slot 1 names are unchanged. Every other provider also reuses or collides on a name that does not
vary per slot yet, so `new_version` is allowlisted to Microsoft and refused with a 400 elsewhere.
Google and LinkedIn belong to the Google-readiness workstream and are left to it. `Campaign`
responses carry `slot_version`, and so does the indexed `CampaignDoc` — lists and revision
history come from the Query Service, where two live campaigns on one slot would otherwise read
as duplicates.

**Expand/contract.** `000022`'s three-column index stays for this release, so the N-1 binary
keeps its serialization during the rollout. While it exists, a `new_version` claim on an
occupied slot raises `23505` on it and is reported as `domain.ErrSlotVersionUnavailable`
("not available yet"). The follow-up migration drops it, and `requiredIndexes` loses its
entry with it; only then does a second campaign actually get created.

**Caught before merge.** The first draft of `000035` added a column named `version` with
`ADD COLUMN IF NOT EXISTS`. `campaigns.version` already exists (`000002`) as the
optimistic-concurrency counter, so the statement was a no-op, `000036` would have keyed the
slot on a counter every write bumps, and the down file would have dropped the concurrency
column. `TestLiveSlotVersionDuringExpandPhase` asserts the upsert bumps `version` and leaves
`slot_version` alone, and the scan test uses distinct values for the two so a swap fails.

**Found in the pre-PR review.** Naming the four-column index as the arbiter while the legacy one
still exists let CONCURRENT slot-1 claims lose on the legacy index with `23505`, which the first
draft reported as "not available yet"; a live 16-way test reproduced it (3 of 16 errored) before
the fix. `AdoptCampaign` had the same blind spot and now classifies `23505` by index name. The
Goa description of `new_version` gained the allowlist and expand-phase caveats, so the published
contract no longer promises a second campaign this release cannot create.
