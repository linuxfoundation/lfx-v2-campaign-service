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
