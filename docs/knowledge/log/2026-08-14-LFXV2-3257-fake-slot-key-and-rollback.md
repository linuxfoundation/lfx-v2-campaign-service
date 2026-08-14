# 2026-08-14 — LFXV2-3257 one slot key in the fake, and a loud rollback

**Fix** — The fake campaign repository's methods disagreed about the key.
`GetCampaignByPlatform` and `AdoptCampaign` were made variant-aware while
`ClaimCampaignDispatch`, `DeleteDispatchClaim` and `UpsertCampaign` kept keying
on `(brief, platform)` alone.

The consequence is the exact shape the slot key exists to prevent: a Demand Gen
dispatch missed on the variant-aware read, then had its CLAIM answered with the
brief's existing SEARCH row — reported as a reused success, so the dispatcher
never ran and the caller was told a Demand Gen campaign existed when only a
Search one did.

All six methods now go through one `slotKey` function mirroring migration
000022's `(brief_id, platform, variant)`. `storeRow` additionally indexes a
DEFAULT-slot row under the bare `(brief, platform)` key so the many existing
single-variant fixtures keep working — a concession to the tests, not to the
schema: a demand-gen row never lands on the bare key and so can never be
mistaken for a Search row.

**Fix** — `000024`'s DOWN used `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS`.
A failed CONCURRENTLY build leaves an INVALID index under the same name, and
`IF NOT EXISTS` would then silently skip the rebuild: the migration would be
marked down, `000022`'s down would drop the widened arbiter, and the table would
be left with NO working uniqueness on the dispatch claim.

The forward path has `000023` to catch that condition; the rollback path has no
later guard, so failing loudly is the only place it can be caught. A
duplicate-name error on rollback is recoverable by hand; a silently missing
arbiter is not, because nothing reports it.

**Note** — The test for the claim defect passed against the bug on its first
writing. It seeded the fake by assigning `existing[slotKey(...)]` directly, so
only the qualified key was populated — a variant-blind claim looked in the bare
key, found nothing, and "correctly" claimed. Seeding through `storeRow`, which is
what the repository itself uses, is what makes reverting the fix fail it. Seeding
a fake by hand rather than through its own writer is a reliable way to write a
test that cannot fail.
