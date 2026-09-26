# 2026-09-26 — LFXV2-2665: an X probe that could verify the wrong account, and a Microsoft account id held to the weaker of two rules

**Fix** — the sixth local-review round on the connection-test branch. Two identifier-validation
gaps and one stale concept.

## 1. X's `VerifyAccount` interpolated an unvalidated account id into the path

`VerifyAccount` checked only that the stored id was non-empty before building the account-scoped
URL. `CreateCampaign` applies `accountIDRe` at its own top; the probe did not. A stored
`18ce54d4x5t/promoted_tweets` therefore made the probe GET a DIFFERENT account subresource — and
a `2xx` from whatever that turned out to be reported the connection as healthy on the strength of
a request that answered a different question, one campaign creation would then refuse on the same
id. That is the "tests clean, fails on dispatch" shape this endpoint exists to remove.

`VerifyAccount` now applies the charset guard and `accounts.go`'s `maxAccountIDLen` bound before
building the path, so a STORED id is held to the same rule as a discovered one, and raises the
new `twitter.ErrInvalidAccountID`. `TwitterDispatcher.ProbeConnection` answers that sentinel as
`accountIDNotUsable` — the pre-send verdict, beside its `ErrAccountNotConfigured` arm — because
the rejection arm would blame a credential X never saw and `ProbeInconclusive`'s default would
answer `OK: true` for an id no X request can address.
`TestVerifyAccountRejectsAnUnusableAccountIDBeforeAnyRequest` asserts the CALL COUNT as well as
the error: a test that only checked the error would still pass if the request were made and
discarded.

## 2. Microsoft's `account_id` was validated as header bytes, not as an identity

Its Goa pattern was `^[0-9]+$` with `MaxLength(64)` — the transport rule — so the API could
persist `0`, or a 64-digit number, on an active connection. `ListAdAccounts` already runs
`numberID` (positive `int64`) over every id the platform hands BACK, and `customer_id` on the
same row already carried that stricter rule. Holding a stored id to a weaker rule than a
discovered one is backwards, and is the generalisation the microsoft concept already states: a
validation borrowed from a transport concern is not automatically the right one for an identity
claim.

The design now declares `^[1-9][0-9]*$` with `MaxLength(19)`, matching `customer_id`. The new
exported `microsoft.ValidateAccountID` closes the one thing a pattern cannot express — a
19-digit value above `MaxInt64` — and `Client.validateAccountIDs` calls it instead of
`accountIDRE`, which only narrows: everything `numberID` accepts `accountIDRE` accepts too.
`TestValidateMicrosoftAdsConnectionConfig_IDPatterns` gained a drift guard for `account_id`
beside the `customer_id` one, and asserts the overflow case directly so the runtime check is not
later deleted on the grounds that the design already bounds the length.

## 3. The LinkedIn concept still called every non-`429`/`5xx` status permanent

Round 5 made `408` retryable in `linkedin/token.go` along with the three other clients, and the
dated fragment recorded it, but `internal-platform-linkedin.md`'s token-exchange prose and table
still listed only `429` and `5xx` as retryable — a concept contradicting the code it describes.
Both now name `408`, with the reason beside them.

Refs: LFXV2-2665
