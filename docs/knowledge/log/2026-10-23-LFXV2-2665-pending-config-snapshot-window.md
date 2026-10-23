# 2026-10-23 — close the pending-dispatch window around `config_snapshot`

**Fix** — the creative-asset delete guard reads `campaigns.config_snapshot` to decide whether an
asset is still referenced. Yesterday's entry
(`2026-10-23-LFXV2-2665-creative-asset-delete-ordering.md`) ordered that read against a dispatch
claim's INSERT. It did not cover what happens once the claim's row EXISTS and a different writer
changes the column the guard reads. Three changes close that, in both directions, plus two
sentences in `000047` that were simply wrong.

**The sweep that was missing.** The NULL arm of the `referenced` CTE was justified like this: a
claim inserts the `pending` row with a NULL `config_snapshot` before the dispatcher reads any
asset, so NULL means "a dispatch that may be about to use this asset". The first half is exact.
The second half assumed the claim was the only writer of that column in the window. It is not —
`BriefService.UpdateCampaign` writes `config_snapshot` too, gated on version rather than on the
brief, and `CampaignRepo.ReplaceCampaign`'s UPDATE touches `campaigns` only, so the brief lock
ordered nothing against it. A caller could therefore replace the claim's NULL with its own object
and make an asset the in-flight dispatch was about to use look unreferenced, and deletable. **The
general lesson: an arm that keys on a column's VALUE is only as good as the full set of that
column's writers, and the sweep has to run repo-wide and in BOTH directions** — a writer that
removes the value the predicate keys on, and a writer that adds one the reader's snapshot misses.

**Removal direction — the reader.** `deleteCreativeAssetQuery`'s `referenced` CTE gains
`c.status = 'pending'` as an independent first arm. It holds whatever the snapshot contains, so a
committed non-NULL snapshot on an unresolved campaign still blocks the delete. This is the
timing-independent half: the delete checks status under its own brief lock, so no ordering
argument is needed for it to be correct.

**The writer, same direction.** `UpdateCampaign` now refuses a CONFIG edit on a `pending`
campaign with `409 reason=campaign_dispatch_in_flight`. The status-mismatch check above it does
not catch this — `pending` is not a run status, so a `pending → pending` edit carrying a config
body passes it. 409 rather than 400 because nothing is malformed and the same request succeeds
once the dispatch settles; only a config-bearing edit is refused, so a name-only typo fix still
works mid-dispatch; and it is validated BEFORE the claim, like every other check on that path.
This keeps the snapshot honest rather than relying on the reader to compensate for a writer that
should not have written.

**Addition direction — the one the first two do not cover.** A campaign that is already `created`
can write a snapshot that ADDS a reference to an asset a concurrent `DeleteAsset` decided was
unreferenced. Nothing ordered those two: the replace's UPDATE touches `campaigns`, the delete
locks `campaign_briefs`, and the per-campaign advisory lock a caller may hold is keyed on the
CAMPAIGN. `ReplaceCampaign` now takes `lockBriefForAssetWriteQuery` as the OPENING statement of
its transaction — the same row lock `DeleteAsset` takes, so one waits for the other and the
second's statement runs on a snapshot that sees the first.

**The other writer, swept and left alone.** `config_snapshot` has exactly two writers:
`ReplaceCampaign` and `UpsertCampaign`, the dispatch-result write. `UpsertCampaign` needs no
change, and the reason is worth recording so nobody re-derives it. Its CONFLICT arm finalizes the
claim's own row, which is `pending` until that transaction commits — so a concurrent delete reads
`pending` and refuses on the new first arm, and once the upsert commits the snapshot is visible to
the next delete. Its INSERT arm writes a new row, and inserting `brief_id` fires the foreign key's
`FOR KEY SHARE` on `campaign_briefs`, which the delete's `FOR UPDATE` blocks — so that arm is
already ordered against the delete, for free, by the FK. `ReplaceCampaign` has neither property:
its UPDATE leaves `brief_id` untouched, so no FK trigger fires and no lock is taken. That is the
whole reason the lock had to be added there and nowhere else.

**Why that lock is unconditional, and why it belongs to the method.** The plan was to take it
only when the config actually changed. Reading the five call sites reversed that:
`ReplaceCampaign` writes EVERY mutable column, `nullJSON(c.ConfigSnapshot)` included, so every
caller rewrites the snapshot whether or not it meant to, and the statement cannot tell the two
apart. The hazard is the method's, not any one caller's.

**The acyclicity argument, stated against the right lock.** The first draft of these comments said
`ReplaceCampaign` takes the per-slot lock and the brief lock in `AdoptCampaign`'s slot → brief
order. It takes no slot lock at all. The only lock its caller may already hold is
`ClaimCampaignVersion`'s, which is keyed on the CAMPAIGN, not the slot — and is taken with
`pg_try_advisory_lock`, so nobody ever BLOCKS on it, for a documented pool reason. The useful
direction is INTO the holder, not out of it — the holder does go on to wait for the brief lock —
but a lock with no waiters is never an in-edge to its holder, and a node no edge enters cannot sit
on a cycle. Independently, the brief lock is the opening statement of the replace's transaction, so it
holds no `campaigns` row lock while waiting for it. Nothing anywhere acquires a slot lock while
holding the brief's. The lesson is the smaller one: naming the wrong lock produces an argument that
sounds rigorous and proves nothing, and two locks called "advisory" in the same file are exactly
where that happens.

**A new conflict reason is a DESIGN edit.** `ConflictError.reason` is an `Enum` in
`design/connection.go`, and the generated client's decoder ENFORCES it: a reason the server emits
and the list omits raises `InvalidEnumValueError`, so it is not an undocumented value — it is a
409 no generated client can read. `campaign_dispatch_in_flight` was added there and `make apigen`
re-run; the slug is now pinned in three places that must move together (the design Enum, the
service constant, and `TestUpdateCampaign_ConfigEditOnAPendingCampaignIsRefused`).

**Two sentences in `000047` were wrong.** The migration claimed a plain CHECK was preferred
because "there is nothing to scan" and because `NOT VALID` would leave a declared-but-unenforced
window. Both are false. Postgres DOES scan on `ADD CONSTRAINT` — the pass runs because the
constraint is added without `NOT VALID`, not because a row could fail it; what is true is that
the scan is over rows that are all `(NULL, NULL)`, so it finds nothing. And `NOT VALID` enforces
new writes IMMEDIATELY, deferring only the check of pre-existing rows. The corrected comment also
records why splitting would not shorten the lock as written: golang-migrate ships a migration file
to a SINGLE `ExecContext` unless `x-multi-statement` is set — `Run()` in
`database/pgx/v5/pgx.go` splits only when `p.config.MultiStatementEnabled`, and this repo's
`pool.go` never sets it — so `ADD ... NOT VALID` and `VALIDATE CONSTRAINT` would land in one
implicit transaction and the ACCESS EXCLUSIVE lock would be held through both. The constraint
itself is unchanged.

**Tests.** Each change has one that fails without it, verified by reverting the change and
watching it go red: `TestUpdateCampaign_ConfigEditOnAPendingCampaignIsRefused` (409, the literal
slug, and `camps.claims == 0` — the refusal happened before the claim), its companion
`TestUpdateCampaign_NameOnlyEditOnAPendingCampaignSucceeds` which stops the guard being widened to
"no edits while pending", `TestLiveDeleteAssetRefusesWhileACampaignIsPending` (a committed pending
snapshot that does not name the asset still blocks; the same row at `created` does not), and
`TestLiveReplaceCampaignTakesTheBriefLockBeforeWritingTheSnapshot`, which proves the replace WAITS
on a held brief lock rather than merely that it is slow, then that the reference it added is
visible to the next delete.
