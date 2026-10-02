# 2026-10-02 — LFXV2-2023 account-scope guard on the LinkedIn and Meta monitor reads

**Fix** — `ListAccountCampaignMetrics` accepted a caller-supplied `accountID` and passed it
to the upstream read without checking it against the account the project's own connection is
bound to. Reddit had checked this since round-18 review; LinkedIn, Meta and Google Ads did
not. LinkedIn and Meta now do.

A connection is singleton per project and stores exactly one account (`account_id TEXT NOT
NULL` under `UNIQUE (project_id)`, a column shared by every provider table), so a request
naming any other account is a request mismatch and answers
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

**Severity split.** LinkedIn is the live case — `tlf` and `lf-events` hold two different
LinkedIn accounts under two different projects, and one LinkedIn token reaches several ad
accounts (that is what `ListAccounts` enumerates), so the token is not the boundary. Meta is
latent: it uses the separate-account-per-foundation model but has one account configured
today, so connecting a second would make it reachable with no code change. Google Ads is one
shared customer id across every foundation, so there is no second account to cross into; its
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
