# 2026-09-25 A rejected discovery request is not a verdict about the connection

**Fix** — Review of PR #223 found the non-`429` `4xx` escape in
`VerifyAccountOrgReference` treating four very different refusals as one outcome. It marked all of
them `ErrOrgVerificationFailed`, which `internal/service` uses as its ECHO ALLOWLIST — so a `400`
caused by a request this service built wrong reached the operator as a confirmed verdict about
their stored `account_id`/`org_id` pairing, telling them to audit a connection that may be
entirely correct. That is the mirror of the bug this whole path exists to close: not a broken
connection reported healthy, but a healthy connection reported broken, with the operator sent to
fix something they do not own.

The deciding fact is what the discovery call actually contains. It is
`GET adAccounts?q=search&pageSize=…` plus an optional page token — it embeds **neither the stored
account id nor the configured org id**. LinkedIn cannot be answering a question about the pairing,
because it was never asked one. A `403` is the exception and genuinely is a verdict: LinkedIn
evaluated THIS token against its authorization rules and refused it.

So the escape now splits **by who can act on the refusal**:

| Outcome | Sentinel | Result |
| --- | --- | --- |
| `403` | `ErrOrgVerificationFailed` | `OK: false`, message echoed to the operator |
| other non-`429` `4xx` | `ErrAccountDiscoveryRejected` (new, exported) | typed **500**, `reason=account_discovery_rejected` |
| dial, transport, `429`, `5xx`, guards | `ErrOrgVerificationInconclusive` | `OK: true` with a fixed advisory |

`internal/dispatch` converts the new platform sentinel to
`domain.ErrServiceDefect` wrapped **alongside** `domain.ErrAccountDiscoveryRejected`, and the same
review added `domain.ErrOrgVerificationUnwired` for the orchestrator's two wiring-defect arms,
which previously returned `ErrServiceDefect` with no reason sentinel at all and therefore logged
`reason=unclassified`. Both follow `ErrServiceDefect`'s stated contract: the status sentinel and
the reason sentinel are separate axes, and one is never wrapped instead of the other. The
`ErrAccountDiscoveryRejected` check is placed BEFORE `systemScoped` and before the
`ErrOrgVerificationFailed` re-tag, so a rejected request can never fall through into the echo.

A second, independent leak was in `SafeInconclusiveDetail`. Classifying by type, a token-exchange
failure that is not one of the three credential sentinels — an unreachable token endpoint, a `5xx`
from it, an unreadable body — matched no branch and inherited the generic completeness-guard
string, telling an operator to go inspect a discovery **response** on a host that was never
dialled for a request that was never built. It now has a first branch of its own, detecting
`*tokenRefreshError` and a new internal no-text tag `errTokenExchangeFailed` that `fetchToken`
attaches to its six otherwise-unclassified failures. The tag wraps rather than replaces, so
`errors.Is` still answers for the credential sentinels underneath.

A third leak came from the same tag, one axis over. `errTokenExchangeFailed` says whose
configuration is implicated; it says nothing about whether a RETRY could help — and
`VerifyAccountOrgReference` folds it into `ErrOrgVerificationInconclusive`, which reports
`OK: true` with a "try again later" advisory. True of a `429`. False of a `410`, of a `403` from
the token endpoint, of a `2xx` whose body carries no `access_token`: those never clear on their
own, so a permanently broken connection read as healthy forever. The same defect this branch
exists to close, reached through the token endpoint instead of the discovery walk.

The fix splits by **retryability**, a second axis independent of the credential question. A new
no-text tag `permanentTokenExchangeError` marks the failures that cannot succeed on a retry, and
its `Is` answers for two targets: `errTokenExchangeFailed`, so `SafeInconclusiveDetail`'s new
first branch still classifies it correctly, and `ErrTokenRequestRejected`, which does the routing
— `accounts.go:479`'s credential unwrap already tests for that sentinel, so the failure reaches
the operator as `reason=token_request_rejected` on a typed 500 through plumbing that already
existed. Rather than stretch a sentinel silently onto a mismatched contract, its documented
contract was WIDENED in both `token.go` and `internal/domain/errors.go` to cover a status outside
400/401/429/5xx, a 2xx yielding no usable token, and a request this service could not build.
`429`, every `5xx`, an unreadable or oversized body, and transport failures all stay inconclusive,
and the regression table asserts that retryable half alongside the permanent half so the two
cannot drift.

The review also flagged that only the `ErrCredentialsExpired` leg of the credential unwrap was
covered: reducing that line to `if errors.Is(err, ErrCredentialsExpired) {` left all three
packages green, because no test drove a token exchange at all — the leg where
`ErrApplicationCredentialsInvalid` (`invalid_client`) and `ErrTokenRequestRejected`
(`invalid_request`) actually come from. Both are now driven at the platform level through
`withTokenURL` against an `httptest` token server, and `VerifyAccountOrg` was added to the two
existing "tagged on every path" tables in `internal/dispatch`. The mutation now fails in both
packages. Note the fixture detail that makes this work: these subtests deliberately do NOT pass
`WithClock(fixedClock())` — that clock is pinned to 2020, which would make
`refreshableCreds()`'s hour-ago-expired token look valid and skip the exchange entirely.

Also renamed the eleven `2026-09-2{3,4,5}` log fragments from this branch to carry `LFXV2-2665` in
their slugs, as CLAUDE.md requires, and repaired the five in-bundle links that pointed at the old
names.
