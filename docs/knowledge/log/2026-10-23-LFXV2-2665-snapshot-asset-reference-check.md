# 2026-10-23 — refuse a replace whose snapshot names an asset the delete already removed

**Fix** — the brief row lock added earlier today
(`2026-10-23-LFXV2-2665-pending-config-snapshot-window.md`) ORDERS `ReplaceCampaign` against a
concurrent `CreativeAssetRepo.DeleteAsset`. Ordering is not arbitration: it says the two cannot
interleave, not which of them won. Only one of the two outcomes was closed.

**The outcome that was still open.** A caller reads a campaign, builds a configuration naming
creative asset A, and sends `update-campaign`. In the same moment a `DeleteAsset(A)` takes the
brief lock first. Its `referenced` CTE looks at the stored snapshots — none of which names A yet,
because the replace has not written anything — finds nothing, deletes A and commits. The waiting
replace then resumes and writes a snapshot naming A. `config_snapshot` is `jsonb` with no foreign
key to `creative_assets`, so nothing anywhere notices; the reference surfaces later as a failed
dispatch, far from the request that created it. Copilot raised exactly this on PR #390, and was
right: the existing live test releases a held brief lock *without* deleting the asset, so it
cannot reach this direction at all.

**The fix.** `internal/infrastructure/postgres/campaign_snapshot_assets.go` adds
`(*CampaignRepo).checkNewSnapshotAssetRefs`, called between the brief lock and the guarded
UPDATE. It extracts the asset ids the incoming snapshot names, subtracts the ones the stored
snapshot already carried, resolves the remainder against `creative_assets` for the campaign's
`(project_id, brief_id)`, and returns `domain.ErrCreativeAssetReferenceMissing` naming any that
did not resolve. `mapBriefErr` maps it to a `409` with
`reason = "creative_asset_reference_missing"` — the inverse of `creative_asset_in_use`, closing
the one window from the other side.

**Why the scope is NEWLY introduced ids, and not the whole snapshot.** `ReplaceCampaign` writes
every mutable column, so its four pass-through callers — the bid, budget and status-toggle
persists, and `persistMetaAdSetRecord` — rewrite `config_snapshot` with whatever they read a
moment earlier, without ever looking at it. A whole-snapshot check would make a status toggle
start failing because of an asset deleted days ago. Under this scope those four write back
exactly what they read, so none of them can ever newly fail, and the blast radius is confined to
`UpdateCampaign`. The cost is stated rather than hidden: a reference that is ALREADY dangling
stays dangling, and the dispatchers' own pre-create refusals are what cover it.

**Why the other writer of the same column is left alone.** `upsertCampaignQuery` also writes
`config_snapshot`, and is deliberately not guarded. `ReplaceCampaign` records an intent the caller
can still revise, so a refusal costs a retry; `UpsertCampaign` records a campaign the dispatcher
has already created upstream, and refusing there would reject the local record of a live, spending
ad — manufacturing the orphan class this repo treats as its most serious defect. The asset is not
in question on that path anyway: the dispatcher resolved it before its first upstream call. The
write that can still be taken back is checked; the write reporting an accomplished fact is not.

**Why matching is case-insensitive, which is not what the first draft did.** The first design
recognised two fixed key spellings, `imageAssetId` (the json tag on the Reddit, X and Microsoft
configs) and `ImageAssetID` (Meta and LinkedIn variants, which carry no tag at all). That is
wrong about where the stored text comes from. `BriefService.UpdateCampaign` stores
`redactedConfigSnapshot(p.Campaign.Config)`, which re-marshals a decoded `map[string]any` — so
the stored key is **whatever the caller sent**, not a Go struct's tag. And every dispatcher binds
that JSON with `encoding/json`, whose field matching is case-insensitive, so `IMAGEASSETID` and
`imageassetid` reach the same field and name a real asset just as surely. Matching two fixed
spellings would have let the others through silently — the same class of defect as a lock
predicate that matches no row. The check now folds case.

**Why the KEY decides and not the value's shape.** A snapshot legitimately carries uuids that are
not creative assets — brief, project, audience and platform campaign ids. Collecting every
uuid-shaped token would send those to the lookup, find nothing, and refuse configurations nobody
had a problem with. Ids are canonicalized through `uuid.Parse` afterwards, because it accepts
four spellings of the same id; without that, re-sending a stored id unhyphenated would read as
newly introduced and be refused for an asset the caller never added.

**Why the presence query does NOT borrow the archival predicate.** `getCreativeAssetQuery` and
`getCreativeAssetSizeQuery` scope their reads with an `EXISTS (… campaign_briefs … status <>
'archived')`. Reusing that here would newly refuse every config-bearing edit on an archived
brief — a replace under an archived brief is allowed today, and the brief lock deliberately
ignores the status it reads. The new query is scoped to `(project_id, brief_id)` alone, and the
query's own comment says why.

**Why a 409 and not a 400.** The configuration was well-formed, and was valid when the caller
read the campaign; what changed is the world, and re-uploading the asset then retrying is the
remedy. That is the 409 contract. It is also why `ErrCreateConfigInvalid` is not reused: its
contract is a create-time 400. A `pgx.ErrNoRows` on the stored-snapshot read returns nil rather
than an error, so the guarded UPDATE keeps sole ownership of the 404-vs-412 verdict. The message
names the ids that failed to resolve — safe precisely because the caller supplied them in that
same request and they were canonicalized, so no platform text and nothing from another tenant
can appear in it.

**Tests.** Four surfaces, because no one of them can reach the others:

- `TestLiveReplaceCampaignRefusesASnapshotWhoseAssetTheDeleteAlreadyRemoved` — the holder
  transaction takes the brief lock, DELETES the asset and commits; the waiting replace is refused
  and the stored snapshot is untouched. This is the direction Copilot named. Verified as a real
  guard rather than a tautology by disabling the call and watching it fail with `err=<nil>`.
- `TestLiveReplaceCampaignCarriesAnAlreadyDanglingReferenceThrough` — the scope contrast: a
  pass-through caller writing an already-dangling snapshot back unchanged still succeeds, and so
  does a caller that re-sends the same id in a different spelling. A check widened to the whole
  snapshot, or one that skipped canonicalization, fails this.
- `campaign_snapshot_assets_test.go` — eight unit tests over the extractor: every key spelling,
  nested and array shapes, the four uuid aliases, uuid-valued keys that are NOT assets, values
  that are not ids, the empty cases, malformed JSON, and a drift guard that marshals the real
  `meta.AdVariant` and `linkedin.CreativeVariant` rather than a literal.
- `dispatch.TestPlatformConfigsNameTheirAssetUnderOneKey` — the cross-package drift guard,
  marshalling all five platform configs and failing if any spells the key something the extractor
  would skip. It lives in `package dispatch` because the five config structs are unexported; the
  moment it exists to catch is the addition of a SIXTH platform, whose misspelled key would be
  skipped silently and read as a passing guard.

Service-layer mapping is pinned by `TestUpdateCampaign_ConfigNamingAMissingAssetIsRefused`
(status, literal slug, ids in the message) against
`TestUpdateCampaign_AConfigEditThatResolvesStillSucceeds`. The repo fake grew a `replaceErr`
field for it: the decision itself belongs under a lock a fake has no way to hold, so the test
drives the sentinel and owns only what happens downstream of it.

**Three surfaces kept in step by hand**, as the existing 409 reasons are: the `Enum` member in
`design/connection.go`, the service constant, and the `update-campaign` Description — plus
`docs/api-catalog.md`'s reason prose. Only the service half is test-pinned, because a test that
calls the service directly never crosses the generated encoder and so cannot observe the Enum at
all; delete the Enum member and every generated client starts failing to decode the 409 while the
test stays green. The constant's own comment says so.
