# 2026-10-02 — LFXV2-2023 account-scope guard on the LinkedIn and Meta monitor reads

**Fix** — `ListAccountCampaignMetrics` accepted a caller-supplied `accountID` and passed it
to the upstream read without checking it against the account the project's own connection is
bound to. Reddit had checked this since round-18 review; LinkedIn, Meta and Google Ads did
not. LinkedIn and Meta now do.

A connection is singleton per project and stores exactly one account: `account_id TEXT NOT
NULL` on every provider table, with one LIVE row per project — a unique index on
`(project_id)` partial to `WHERE status <> 'deleted'` (migration 000001 is the sole
authority; the schema doc renders it as a flat UNIQUE and is stale on that detail). So a
request naming any other account is a request mismatch and answers
`domain.ErrAccountNotManagedByConnection` (400 at `internal/service/connection.go`) before
any upstream call is issued. The sentinel and its 400 mapping already existed for Reddit;
only the two guards and their tests are new.

**Why refusing the LF system fallback was not already enough.** Round-16 review escalated a
credential-scope gap on these same endpoints to Critical and fixed it by refusing the system
fallback, noting that membership-checking against `ListAccounts` "would not have closed it" —
correct, because two projects on one shared credential see an identical account list. That
reasoning does not cover comparing against the project's own STORED `account_id`: `resolved.accountID`
comes from the connection row the project owns, and `resolveOwned` already refuses the system
fallback, so the value is genuinely per-project and the comparison is not a no-op. The fallback
check is about whose CREDENTIAL is used; this is about which ACCOUNT the request named.

**Severity split.** LinkedIn is the live case: it genuinely runs several ad accounts across
foundations (`tlf` and `lf-events` are two of them), and one LinkedIn token reaches several
of them — that is what `ListAccounts` enumerates — so the token is not the boundary and a
project naming another project's account is a real, reachable request. Meta's guard is
DEFENSIVE rather than a live exposure: Meta is one shared ad account across foundations
today, so a project's stored `account_id` IS the shared account and a legitimate request
matches it. It refuses nothing that would otherwise have succeeded, and it is the only thing
standing between a shared-account read and a per-project one if Meta ever splits.
`docs/architecture.md`'s "Account Tenancy" table lists Meta/Reddit/X as per-foundation; that
is stale against how the accounts are actually run and is not the authority here. Google Ads
is likewise one shared customer id across every foundation; its
guard is deliberately deferred because `internal/dispatch/googleads.go` is in an open,
approved PR and landing this there would hand that PR a conflict for a latent issue.

**Not proven.** That no local guard existed is verified by mutation: with the mismatch arm
disabled, both tests fail and the request reaches the platform — Meta's 401 names the foreign
account in the URL (`GET /act_999/campaigns`). Whether LinkedIn's API would actually serve a
foreign account to that token is NOT established; that needs a live read with a real
credential. The guard is worth carrying either way, since it makes the `AccountMetricsReader`
contract ("scoped to platform via the project's stored connection credential") true in this
service rather than delegating it to another vendor's permission model.

**Shape detail.** LinkedIn keeps an empty stored account distinct from a mismatch
(`ErrAccountNotSelected` vs `ErrAccountNotManagedByConnection`): its owned-discovery resolver
deliberately returns success on `ErrAccountNotSelected` so `VerifyAccountOrg` — which takes no
`accountID` and so has nothing to compare — can report a half-configured pairing itself. That
is also why the guard sits in each `ListAccountCampaignMetrics` body rather than in the shared
resolver. Meta layers its check onto the existing `requireMetaAccountID` so the empty-account
handling cannot drift between the dispatch, budget and monitor paths.
