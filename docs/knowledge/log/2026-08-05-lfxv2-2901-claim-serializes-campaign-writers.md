# 2026-08-05 — LFXV2-2901: one atomic claim serializes every campaign writer

**Update** — Closed the toggle If-Match TOCTOU race across all campaign writers
(LFXV2-2901). `BriefService.ToggleCampaignStatus` previously guarded optimistic
concurrency with a read-time in-memory `existing.Version != version` check BEFORE
the side-effecting platform call; that only rejects a stale caller, it does not stop
a SECOND caller reading the same version and also proceeding to mutate the
platform. Added `CampaignRepo.ClaimCampaignVersion` (`UPDATE campaigns SET
version=version+1 WHERE ... version=$expected`), an atomic version-gated bump with
no content column, mirroring `ReplaceCampaign`'s existing not-found/precondition-
failed disambiguation (prior art: `ClaimCampaignDispatch`'s `INSERT ... ON CONFLICT
DO NOTHING`). Both `ToggleCampaignStatus` (before the platform call) and
`UpdateCampaign` (before its DB-only write, which has no I/O gap of its own but can
otherwise win the row out from under an in-flight toggle's claim) now claim first,
so the version column serializes every campaign writer through one atomic gate
rather than each writer protecting only itself.

**Superseded** — the version bump described above is no longer how the claim excludes a
second writer. Later rounds moved exclusion onto a session advisory lock and left the
version untouched, so the increment happens only in `ReplaceCampaign`, inside the same
transaction that writes the outbox event — see
[`2026-08-05-lfxv2-2901-lock-connection-reuse-and-unconfirmed-cooldown.md`](2026-08-05-lfxv2-2901-lock-connection-reuse-and-unconfirmed-cooldown.md)
and [`2026-08-06-lfxv2-2901-advisory-lock-is-not-ownership.md`](2026-08-06-lfxv2-2901-advisory-lock-is-not-ownership.md).
