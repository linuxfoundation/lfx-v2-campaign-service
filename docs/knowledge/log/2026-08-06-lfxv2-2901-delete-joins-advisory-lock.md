# 2026-08-06 — Delete joins the campaign advisory-lock protocol (LFXV2-2901)

**Update** — `DeleteCampaign` now acquires the same campaign advisory lock as
`ClaimCampaignVersion` before taking its `FOR UPDATE` row lock. Copilot found the
hole: `FOR UPDATE` serializes delete against the dispatch path, which UPDATEs the
row, but NOT against an in-flight run-state toggle. A toggle holds its claim
ACROSS the platform call, and in the window between `ClaimCampaignVersion` and
`ReplaceCampaign` it holds no row lock at all. A delete committing inside that
window soft-deletes and bumps `version`, so the toggle's
`ReplaceCampaign(expectedVersion)` fails AFTER the paid side effect already landed
upstream — the campaign is changed on the ad platform with no local record of it.
Taking the advisory lock makes delete wait for the toggle to release, after which
it observes the bumped version and returns an actionable 412.

**Update** — The delete transaction begins on the connection already holding the
advisory lock (`conn.Begin`), not on the pool (`r.db.Begin`). Beginning on the
pool would take a SECOND connection while the first is held, which self-deadlocks
whenever the pool is saturated — `pool_max_conns=1` guarantees it — because the
delete would wait for a connection only it could free. The unlock runs in a
`defer` on a context detached from the request, with destroy-on-unlock-failure,
mirroring `ClaimCampaignVersion`: a session advisory lock is not released by
returning the connection to the pool, so a failed unlock would strand it and block
every future claim and delete for that campaign.

**Update** — Pinned by `TestDeleteCampaign_ParticipatesInAdvisoryLockProtocol`,
which asserts on the method body's source. This repo has no DB-backed test
harness, so racing two real transactions is not available; both properties pinned
are structural. Verified binding by reverting `conn.Begin` to `r.db.Begin` and
confirming the failure.

**Update** — Replaced a `switch { case err == nil: ... }` in
`internal/service/brief_test.go` with a plain `if`/`continue`; CI's staticcheck
flagged it as QF1002 (could use tagged switch), which failed Build and Test.
