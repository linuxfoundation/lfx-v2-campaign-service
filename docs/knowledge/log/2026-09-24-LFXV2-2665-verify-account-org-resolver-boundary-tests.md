# 2026-09-24 VerifyAccountOrg: pin the resolveOwned boundary with forced-system fixtures

**Update** — A Copilot review thread on PR #223 observed that `TestLinkedIn_VerifyAccountOrg`
(`internal/dispatch/linkedin_test.go`) left `resolve` and `resolveOwned` indistinguishable, so it
did not protect the security boundary `VerifyAccountOrg` introduced. The suite's own doc comment
conceded as much — none of its fixtures set `LFX_FORCE_SYSTEM_ADS_ACCOUNT`, and it deferred to
`internal/dispatch/creds_test.go` for that flag's effect. That deferral does not cover this call
site: `creds_test.go` proves what the flag does to `resolve`, not that `VerifyAccountOrg` declines
to use it.

The finding was correct. `LFX_FORCE_SYSTEM_ADS_ACCOUNT` is the only condition under which the two
resolvers diverge, so with the flag off every fixture in the file passes identically either way —
a future edit swapping `d.creds.resolveOwned` for `d.creds.resolve` at
`internal/dispatch/linkedin.go:648` would have been caught by nothing.

Added two subtests, both with the flag on:

- **no own connection.** Only the `model.SystemProjectID` row exists. `VerifyAccountOrg` must
  error, must ask the repository for the `cncf` scope ONLY, and must make NO request to LinkedIn.
  Under `resolve` the LF row resolves, the walk succeeds, and a project with no LinkedIn
  connection is told its connection verified — the exact "broken connection reported healthy"
  class this PR's other fixes close.
- **own connection present.** Both rows exist with the SAME account id but different `org_id`s;
  the project's agrees with the server's `reference`, the LF row's does not. A nil result proves
  the project's row was the one checked, since the substitution would surface as a confirmed
  mismatch rather than as a resolution failure.

Verified as a real guard by mutation, not just by passing: temporarily swapping `resolveOwned`
for `resolve` at the call site fails exactly these two subtests and no others; the edit was
reverted and `git diff` on `internal/dispatch/linkedin.go` is empty. The stale doc comment on
`TestLinkedIn_VerifyAccountOrg` was replaced with one stating what the fixtures now pin, and
[internal-dispatch.md](../code/internal-dispatch.md)'s `OrgReferenceVerifier` section records the
guard alongside the resolver rationale it defends.

No production code changed.
