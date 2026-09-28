# 2026-09-25 The inconclusive bucket, narrowed — and made safe by construction

**Fix** — A local review trio on the whole PR #223 branch, plus two leaks found alongside it,
resolved into one change with a single theme: every remaining way this connection test could
report a broken connection as HEALTHY, and every remaining way it could echo text it did not
write. `TestLinkedinAds` checks the inconclusive sentinel FIRST and maps it to `OK: true`, so
anything wrongly folded into that sentinel is silently a healthy verdict on a broken connection —
the failure class this PR exists to close, and the same class as the earlier 403, credential and
early-page cases (see
[2026-09-23-LFXV2-2665-linkedin-org-verify-403-decrypt-defect-fix.md](2026-09-23-LFXV2-2665-linkedin-org-verify-403-decrypt-defect-fix.md)).

Four things were folded in that should not have been, all in
`internal/platform/linkedin/accounts.go`:

1. **A stored `account_id` of the wrong shape.** `orgIDRE` already guarded the org id; nothing
   guarded the account id, though `targeting.go` refuses the same value when binding a campaign.
   `VerifyAccountOrgReference` now applies `ValidateAccountID` before the walk starts. It is
   deliberately NOT wrapped in `domain.ErrAccountIDMalformed`: that sentinel is documented as a
   **caller-supplied** account id, and this one is stored, so reusing it would have mapped an
   operator-fixable configuration defect onto a request-validation status.
2. **An ABSENT account id or org id.** These previously returned `nil` early — a third meaning for
   `nil`, and the one that made four doc sources' "exactly two" claim false. A stored pairing that
   is half-absent is not a pairing, and campaign creation on it cannot succeed, so the empty
   string now falls into the shape guards above. `nil` means exactly two things again, which makes
   those four claims true without rewording any of them.
3. **A non-429 `4xx`.** Only `403` escaped before. But LinkedIn RECEIVED a `400` or a `404` and
   refused it on the merits — a `400` means this service built a malformed request, a `404` that
   the endpoint is gone — and neither starts succeeding on its own. Calling those an incomplete
   walk answers "healthy" for a permanently broken cross-check forever. `429` stays exempt (a rate
   limit says nothing about the pairing) and so does `5xx`.
4. **A pre-send dial failure, mis-described rather than misclassified.** `doRequest` deliberately
   does not wrap one as a `*transportError` — that type means "may have been sent" — so a plain
   network outage, the most common real cause of an inconclusive walk, matched neither branch of
   `SafeInconclusiveDetail` and fell through to the completeness-guard string, telling an operator
   to inspect a LinkedIn response that was never received. It now has its own branch, checked
   first.

The two leaks were both in the service layer's `default` arm, which echoes the error's own text
into the caller's message — safe only for a CONFIRMED verdict whose text this service wrote:

- A failure to READ the connection row could carry a driver error rendering the query. It is also
  not a verdict at all, and is the one outcome here that retrying can fix, so `connLoadFailed`
  (`internal/dispatch/creds.go`) now additively wraps a new `domain.ErrConnectionLoadFailed`
  beside `notCreated`, and `TestLinkedinAds` answers **503**. `NoUpstreamCreate` was rejected as
  the discriminator: it means "no campaign was created", which is not a claim about the repo, and
  a read-only caller never created anything to begin with. The wrap is purely additive — every
  existing consumer matches `NoUpstreamCreate` or a default arm, and neither moves.
- An unusable stored connection is detected partly by decoding the DECRYPTED credential blob, so
  its chain can carry credential-derived bytes. A new arm matches `domain.ErrConnectionNotUsable`
  and fails the test with a FIXED remedy message quoting no part of the error.

The third leak is the one worth the design note. `internal/service` was importing
`internal/platform/linkedin` purely to match that package's inconclusive sentinel — a layering
violation `internal/domain/errors.go` states three times and
[internal-dispatch.md](../code/internal-dispatch.md) states again — and the error it matched wraps
a chain that renders the full discovery request URL, pagination cursor included. Rather than add
another redaction arm, a convention every future caller must remember, the fix makes safety a
PROPERTY of the sentinel: new `domain.ErrOrgVerificationInconclusive`, converted in
`LinkedInDispatcher.VerifyAccountOrg` — the only layer that knows both the service's contract and
this client's types — carrying `SafeInconclusiveDetail`'s fixed string and DROPPING the original
chain. An error carrying the domain sentinel is safe to log verbatim by construction, and
`internal/service` no longer imports the platform package at all.

Coverage added at all three layers: table-driven confirmed-error cases for absent, URN-shaped and
stray-character ids asserting no request is made; `400`/`404`/`409` failing and `429` staying
inconclusive; a `*net.DNSError` inside a `*url.Error` carrying a cursor canary; a dispatch subtest
pointing the client at a closed server that asserts the domain sentinel, NOT the platform one, and
no URL in the text; and two service subtests with `DO-NOT-LEAK` markers pinning the 503 and the
fixed-remedy `OK: false`.

The same design note then applied a second time, to the other half of the switch. Two of the
three leaks above were found in the service layer's `default` arm, and the third would have
been — which says the arm itself was the defect, not the three classes. `default` echoed the
error's own text, so every class that reached the switch without matching an arm above it
inherited the echo by accident, and each was caught only after it could already reach a
response. So the echo became an ALLOWLIST: the platform client now MARKS its confirmed verdicts
with `linkedin.ErrOrgVerificationFailed`, `VerifyAccountOrg` converts that to
`domain.ErrOrgVerificationFailed` (and tags its own missing-id verdict the same way, so the one
message naming the field to repair survives), and `internal/service` echoes only on that
sentinel. The new `default` fails the test with fixed text and sends the detail to
`slog.ErrorContext` — which also retires the self-contradicting message a repo `ErrNotFound`
from a connection deleted mid-test used to produce.

Both tags attach through a small unexported error type whose `Error()` forwards to the wrapped
error and whose `Is` answers for the sentinel. `fmt.Errorf("%w: %w", …)` would render a second
"verification failed" sentence in front of the facts, next to the service's own prefix, and
`errors.Join` would put a newline in a single-line API message. The tag exists to be matched,
not read.

Docs updated in the same change: the outcome tables and inconclusive-bucket descriptions in
[internal-platform-linkedin.md](../code/internal-platform-linkedin.md),
[internal-service.md](../code/internal-service.md) and
[internal-dispatch.md](../code/internal-dispatch.md), and the LinkedIn `/test` row in
[`docs/api-catalog.md`](../../api-catalog.md), which gained the stored-`account_id` clause, the
widened `4xx` wording and the two new outcomes.
