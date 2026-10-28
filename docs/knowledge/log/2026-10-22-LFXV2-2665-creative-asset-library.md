# 2026-10-22 — a brief's creative assets become readable and deletable

**Update** — `list-creative-assets`, `get-creative-asset` and `delete-creative-asset` join
`upload-creative-asset` on the briefs service, and `CreateAsset` starts enforcing a per-brief
quota. Before this, upload was the ONLY creative-asset method: a brief's library was write-only,
nothing could enumerate it, nothing could free its bytes, and nothing bounded its growth.

Folder upload is what makes those gaps blocking rather than untidy. A folder is a SEQUENCE of
requests, and every one of the three gaps shows up as a client that cannot be written correctly:
it cannot show what is already stored, cannot tell the operator how much room is left before it
starts sending, and cannot offer a way out when the answer is "none".

## The quota is enforced where the lock already is

`model.MaxCreativeAssetsPerBrief` = 100 and `model.MaxCreativeAssetBytesPerBrief` = 512 MiB, both
a STATED ASSUMPTION — issue `#171` owns the real figures — chosen so the endpoint cannot be used
to fill the table, not derived from a product requirement.

The aggregate runs inside `CreateAsset`'s transaction, after the `FOR UPDATE` lock the parent-brief
gate already takes. That is what makes it race-free: two concurrent uploads serialize on the same
brief row, so neither reads a total the other is about to invalidate. In the service layer, or in
its own statement, it would be a TOCTOU window with nothing holding it shut.

`ErrCreativeAssetQuotaExceeded` maps to **409, not 400**. The request is well-formed; the brief's
state is what refuses it, and the remedy is to change that state — which is why the message names
the endpoint that provides the remedy. "The brief is full" with no way to empty it is not
actionable.

**Dedupe hits are exempt**, and that exemption is the subtle half. The aggregate runs only when
the insert was not a duplicate. Without it, a plain retry of an already-stored upload — which
adds nothing to either total — would start failing once a brief reached the cap, so the upload
would stop being idempotent exactly when a client is most likely to be retrying.

## Delete is one statement because the gap between two is the bug

`DeleteAsset` is a HARD delete. Campaigns and briefs retire by a status flip; this row is
removed, because reclaiming the storage against the quota is the entire reason the endpoint
exists.

It is three CTEs — `target`, `referenced`, `removed` — returning `found`/`deleted`, resolved
against ONE snapshot. `!found` → `ErrNotFound`; `found && !deleted` → `ErrCreativeAssetInUse` →
409; both → success. A single row count could not separate "no such asset" from "refused". The
reference check cannot be hoisted into its own query: between it and the delete,
`claimCampaignDispatchQuery` can insert a `pending` campaign row and a dispatch can begin reading
the bytes being removed.

The refusal predicate is any campaign on the brief with `status <> 'deleted'` whose
`config_snapshot` names the asset id **or is NULL**. Three details, each load-bearing:

- **The NULL arm is exact, not a hedge.** `claimCampaignDispatchQuery` inserts the claim row with
  no snapshot BEFORE the dispatcher resolves any asset. A NULL snapshot on a live row is not "we
  cannot tell" — it is precisely the window in which a reference is about to exist.
- **Both id spellings.** `config_snapshot` is persisted unencrypted and its sanitization strips
  URL query/fragment and credentials but keeps `imageAssetId` VERBATIM, so the id survives as
  text — in whichever form the caller sent. The hyphenated and 32-char dashless forms are both
  matched, lower-cased on both sides; that is exhaustive over `uuid.Parse`'s four shapes, because
  the `urn:uuid:…` and `{…}` forms each CONTAIN the hyphenated one.
- **`strpos(...) > 0`, not `LIKE`.** A UUID contains no `LIKE` metacharacter today, but a pattern
  built from an identifier is a class of bug worth not having.

Only `deleted` campaigns are ignored, and multiples of them are legal —
`uq_campaigns_brief_platform_variant_slot_version_live` is partial `WHERE status <> 'deleted'`.

## The list result publishes the ceilings, not just the totals

`CreativeAssetList` carries `assets` plus `count`/`count_limit` and `bytes_used`/`bytes_limit`,
all five REQUIRED. The ceilings are there so a client can decide BEFORE it starts sending;
without them it discovers a full brief by sending the file that gets refused, having already paid
for every round trip before it. Required is what separates an empty library from a total the
server declined to compute — "0 of 100 used" is exactly the state a folder picker must render.

The handler reads the same `model` constants the repository enforces against, so a drift between
the published ceiling and the enforced one is a test failure rather than a client quietly told
the wrong number (`TestListCreativeAssets_PublishesTheEnforcedQuota`).

**No LIMIT or cursor.** A brief stored before the quota existed may hold more than `count_limit`,
and its full set is precisely what an operator must see in order to delete back down to it.

The query is driven FROM `campaign_briefs` with a LEFT JOIN onto `creative_assets`, not from the
assets table. That is the only shape separating "this brief is empty" (a row, no assets, a
non-nil empty slice) from "this brief is not visible to you" (no row → `ErrNotFound`); an
assets-first query returns zero rows for both, so an archived or foreign brief would read as an
empty library. Totals are computed in the same snapshot as the rows — under READ COMMITTED a
separate aggregate would be free to disagree with the list printed beside it.

## Two small decisions worth recording

**`GetAssetMetadata` is a fifth port method, not a flag on `GetAsset`.** The difference is
whether up to 30 MiB of `BYTEA` is detoasted for a response that never carries it. Its test
asserts `Bytes == nil` first, because that is the whole point of the method existing.

**No new Goa error type.** `commonBriefErrors()` already declares `Conflict`/`ConflictError`, and
the two causes are separated by the `reason` discriminator — `creative_asset_quota_exceeded` and
`creative_asset_in_use` — the mechanism `ab_test_unsupported_send_type` established. A folder
upload has to branch on WHICH refusal it got: stop and prompt the user to free space, versus skip
this one file. Those are indistinguishable from prose, and identical from a bare 409.

## Tests

Service: `TestCreativeAssetReads_UnavailableWithoutRepo` (all three take the typed 503),
`TestListCreativeAssets_PublishesTheEnforcedQuota` (the constant-drift gate, plus order and the
absent-vs-zero dimension mapping), `TestListCreativeAssets_EmptyLibraryIsAnEmptyArray`,
`TestGetCreativeAsset_ReturnsMetadataAndNeverCreated`, and
`TestDeleteCreativeAsset_RefusalsCarryTheirDiscriminator` over both reasons. The fake's
`ListAssets` SUMS its fixture rather than taking the totals as settable fields — a fake free to
disagree with its own rows would let a handler that published unrelated totals pass.

Live: `TestCreativeAssetRepo_CreateAsset_EnforcesPerBriefCount` (one past the cap refused, the
refusal stores nothing, a dedupe hit still admitted, another brief unaffected),
`TestCreativeAssetRepo_GetAssetMetadata_ScopesLikeGetAssetWithoutBytes`,
`TestCreativeAssetRepo_ListAssets_SeparatesAnEmptyLibraryFromAnInvisibleBrief`,
`TestCreativeAssetRepo_ListAssets_ReturnsOwnRowsOldestFirstWithTotals`,
`TestCreativeAssetRepo_DeleteAsset_RefusesWhileALiveCampaignCouldNeedIt` (every arm, including
the dashless spelling and the NULL snapshot) and
`TestCreativeAssetRepo_DeleteAsset_FreesTheBriefsQuota` — the last one because the 409's stated
remedy has to actually work.

The BYTE arm of the quota is deliberately NOT exercised live: 512 MiB of inserts would dominate
the suite's runtime to reach the other branch of the same `if` the count arm already covers.
