# 2026-08-18 — LFXV2-3050 an empty CURRENT account is an absence, not a mismatch

**Fix** — the account-provenance guard shipped with the absence question asked of only one side.
`created == ""` was handled as "unknown, proceed"; an empty CURRENT id was not, so it compared
unequal to every recorded account and returned a 409.

**Meta is the only platform where that is reachable, and the only one it was missed on.** Unlike
every sibling, Meta's `ToggleStatus` and `ReadMetrics` deliberately do NOT require an account
selection — they address the campaign node by id (`POST /{campaignID}`,
`GET /{campaignID}/insights`) and never read `AccountConfig.AccountID` — so a connection whose
account was cleared via PUT can still pause a campaign and read its metrics.
`resolveMetaCredentials` says so in its own words and `requireMetaAccountID` is called only from
`Dispatch`. LinkedIn, Reddit and Twitter all refuse an account-less connection upstream with
`ErrAccountNotSelected`, so the arm is dead there. The guard now asks the absence question of
both sides on all four, and the three unreachable arms say they are unreachable rather than
silently depending on a precondition nobody pinned — Reddit's is now pinned by a test, LinkedIn's
already was.

**The existing contract tests could not catch it.** `TestMeta_ToggleStatus_NoAccountIDNeeded` and
its ReadMetrics twin both clear the account id, but both pass a campaign with NO `Result` blob —
so `created == ""` short-circuited first and the case passed through a DIFFERENT arm than the one
under test. The combination that broke needs a cleared account AND a row recording provenance,
which via the `MetaURL act=` fallback is nearly every historical row. A case that passes on an
arm you did not mean to exercise is not coverage.

**`normalizeMetaAccountID` did not do what its name and its own doc claimed.** It promised
"an empty input stays empty, never `act_`" while `act_` itself, `act_act_777` and `act_abc` all
returned non-empty — values naming no account, yet treated as real ones, comparing unequal to
every legitimate connection and manufacturing a false mismatch on a campaign nobody could
re-point. Anything that is not `act_<digits>` now normalises to `""`, which puts malformed
provenance in the proceed arm where it belongs. Unreachable today behind
`design/connection.go`'s `^act_[0-9]+$` — which is exactly why the helper must not depend on that
constraint holding forever.

**A doc claim attributed to the wrong owner.** `meta.CampaignResult.AccountID` said it stores the
id "in Meta's documented `act_<digits>` form"; the field stores `c.account.AccountID` verbatim and
applies no normalisation. The form comes from the connection's upstream `^act_[0-9]+$` pattern.
The comment now names that source, because a sentence stating an invariant of the wrong layer is
one a later reader will rely on.
