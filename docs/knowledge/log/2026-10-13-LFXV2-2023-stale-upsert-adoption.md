# 2026-10-13 — A stale dispatch never overwrites an adoption

**Fix** — review of #260. The per-slot lock ordered the upsert after an adopt but did not stop its
`DO UPDATE` arm: claim → operator clears the pending claim → adopt binds the slot → the stale
dispatch's upsert repointed the adopted row at its own upstream id, untracking the adopted
campaign. The conflict arm now updates only the row whose stored `job_id` equals the writer's
(the claim INSERT stamps it; an adoption leaves it NULL; another job's claim carries that job's
id). Otherwise the upsert writes nothing and returns the new `domain.ErrDispatchClaimLost`, which
`dispatchPlatform` reports as an untracked upstream campaign needing reconciliation, keeping the
upstream id in the job result. The provenance live test that upserted twice with no job id now
carries one. See [internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md)
and [internal/service](../code/internal-service.md).

Follow-up, same review: an id-less partial (ambiguous create, LinkedIn group-only orphan) with a
lost claim kept nothing that identified it, because its reconcile detail lives only in `Result`
and that row is never written. The lost-claim result now carries the blob's identifying fields
(names and ids only, never Steps text or URLs) in the stored platform result's `reconcile` map
and in the ERROR log, and an id-less partial gets a message saying no campaign id was captured.

Second follow-up, same review: the claim RELEASE is keyed on the job id too.
`DeleteDispatchClaim` now takes the claiming job's id and deletes only that job's pending row,
so a stale pre-create release or read-back rollback can no longer delete a successor job's
in-flight claim after an operator clear; the operator's job-less clear is the separate
`ClearDispatchClaim`. And `reconcileRef` caps values in runes at 1024 (it dropped values over
256 BYTES, which lost legitimate 255-character multibyte LinkedIn names), truncating with a
`<key>Truncated` flag instead of dropping.

Third follow-up, same review: `docs/channel-connections-schema.md` and the postgres concept still
named "a concurrent `DeleteDispatchClaim`" as a way the claim row disappears before the upsert's
INSERT arm. With the job-keyed release only an operator clear (`ClearDispatchClaim`, or by hand)
can do that; both now say so, and that another row on the slot yields `ErrDispatchClaimLost`.

Fourth follow-up, same review: `reconcileRef` kept only scalar ids and names, dropping the
identifier collections partial results keep for reconciliation (Google's `creativeAssetIds` /
`extensionAssetIds`, LinkedIn's `imageUrns`). It now keeps string arrays and string maps under
`*ids`/`*urns`/`*names` (and the singular forms) keys, each string capped and the field flagged
when cut, so `reconcile` became a structured map. And the lost-claim ERROR log on the
retained-partial path now carries the provider's error as `dispatch_error`, which returning
early had left out; the job result's message stays generic.

Merge with main's #328 (legacy-binding refusal): `UpsertCampaign` takes the per-slot lock, then
`refuseLegacyBinding`'s (platform, upstream id) lock, matching `AdoptCampaign`'s slot → brief →
binding order, so no writer holds the binding lock while waiting for a slot. The job-id guard
still decides the conflict arm, so the legacy-row live test now seeds its row with a job id and
re-writes it as that job.

Fifth follow-up, same review: `reconcile` is now returned by `GET .../jobs/{jobId}` as a typed
`PlatformResult.reconcile` list (design + `make apigen`; `BriefService.GetJob` maps it), so the
lost-claim message points at identifiers a caller can fetch. And a stale slot-2 upsert after an
operator clear and an adopt (always slot 1) misses the four-column arbiter and violates 000022's
three-column index on the INSERT arm; `UpsertCampaign` now reports that as
`ErrDispatchClaimLost` too, instead of an ordinary persist failure that claimed the claim was
retained.

Sixth follow-up, same review: `reconcileRef` now descends into nested objects and arrays of
objects (depth 3), keeping identifier-keyed values under dotted paths (HubSpot
`abTestVariant.id`, Google `adGroups[0].adIds`, Meta `Ads[1].ImageHash`; `hash` joined the key
rule for Meta's image hashes), with the whole payload bounded at 256 strings / 32,768 runes and
`reconcile_truncated` set past it (new `PlatformResult` field, regenerated). The campaign's
status is no longer a `reconcile` entry — it is state, not a handle — and is logged instead.

Seventh follow-up, same review: `reconcileRef` walks breadth-first, scalars before collections
at each depth, so hundreds of nested ad-group criterion ids can no longer spend the payload bound
before the top-level campaign and budget handles are kept. And `UpsertCampaign` now refuses by
itself what 000022's index refused for it: under the slot lock it passes only this job's own
claim at its slot version, or an INSERT into a wholly empty slot; anything else is
`ErrDispatchClaimLost`. A live test drops the index in a scratch database to pin that on the
contracted schema.

Eighth follow-up, same review: X keeps the Media Library `mediaKey` of the image it uploads
before the campaign POST in its name-only partial, and the suffix rule dropped it. It is now an
exact, case-insensitive allowlist entry (not a `key` suffix, which would admit credential-shaped
fields). A scan of every dispatcher's partial-result struct found no other handle the suffix
rule misses (Meta's `ImageHash` is already covered by `hash`).
