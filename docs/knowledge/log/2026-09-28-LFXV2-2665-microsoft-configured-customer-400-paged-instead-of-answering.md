# 2026-09-28 — LFXV2-2665: a refused Microsoft customer_id paged us instead of answering

**Fix** — round 18 of review, raised by Copilot on PR #228 in code the previous rounds had left
untouched. A behaviour change, and the last instance of this PR's own bug living inside the PR.

A configured `customer_id` that does not exist, or that the stored credentials cannot reach,
comes back from `AccountsInfo/Query` as an ordinary `400`. `microsoft.ProbeCredentialRejected`
claims `401`/`403`; `microsoft.ProbeInconclusive` claims `429`/`408`/`5xx`. A `400` matched
neither, so `probeClass` fell to its default arm and the connection test answered
`domain.ErrServiceDefect` — a typed `500` that pages the service team — for a stale value on the
operator's own connection row.

That is exactly the failure class this branch exists to remove, surviving in one corner of the
branch that removes it. `customer_id` is settable through the connection config API, and access
to a customer can be revoked long after the value was stored, so it is a reachable state rather
than a theoretical one. The connection-test endpoint's whole purpose is to turn "your connection
is misconfigured" into an answer the operator can act on; here it turned it into an incident.

Both obvious repairs are wrong, and that is why this needed a fourth predicate rather than a
wider arm on an existing one. Adding `400` to `ProbeCredentialRejected` renders "microsoft ads
rejected the stored credential" and sends the operator to re-authorise a credential Microsoft
honoured well enough to answer with. Adding it to `ProbeInconclusive` names an outage to wait out
instead of a field to correct. The remedy is `customer_id`, and no existing verdict says so.

`ProbeConfiguredCustomerRejected` is that predicate, and `customerNotReachable` is its verdict —
the post-call sibling of `customerIDNotUsable`, which already answered for the same field when
the value is not a customer identity at all. One says the value cannot name a customer; the other
says it names one these credentials do not reach. Same field, different authority, different
sentence. It carries no not-attempted marker, because the enumeration was sent and answered, and
it names no account, because reaching it means the enumeration never completed — under a
corrected customer the configured account may well be reachable.

The gating is the part worth recording. The predicate claims a `400` only when the customer id
came from the connection ROW: with none configured the id is one the client read out of
`User/Query` moments earlier, and since the request body is otherwise composed entirely by this
service, a `400` there is the shape of a request only we build. Marking both provenances would
have told an operator to repair a field that is not broken AND silenced the one class of `400`
that genuinely is ours — trading a false page for a missing one. `discoveredCustomer` now carries
a `configured` flag for no other purpose than making that distinction available on the error path.

No error-code allowlist sits on top of the status check, deliberately. `apiError` already carries
parsed `ErrorCodes` and a `hasErrorCode` matcher, so narrowing by code looked available — but the
Customer Management codes for a missing or unreachable customer are pinned by nothing in this
repo and by no test against the live API. A guessed literal would never have matched, which
restores the paging `500` while reading, in the diff and in the doc, as though the case had been
handled. That is a worse state than not narrowing: a gap that looks closed stops being looked
for. The status gate plus the provenance gate is what is actually known to be true, and it
follows `classifyTokenRefusal`'s existing discipline in this package — status first, body only
where an allowlist has earned it.

Both new tests were confirmed to fail against the pre-fix code before the fix was kept:
`TestListAdAccounts_AConfiguredCustomerRefusedIsClaimedByItsOwnPredicate` on the predicate, and
`TestMicrosoftProbe_RefusedConfiguredCustomerIsAVerdictNotAServiceDefect` on the
`ErrServiceDefect` itself, which it reproduced verbatim.
`TestListAdAccounts_ADiscoveredCustomerRefusedStaysADefect` passes both before and after by
design: it is the guard against the fix over-claiming, not a demonstration that it works.
