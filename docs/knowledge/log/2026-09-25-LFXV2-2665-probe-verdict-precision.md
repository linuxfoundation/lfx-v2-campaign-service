# 2026-09-25 Connection-probe verdicts: saying the right thing, not just the right OK

**Fix** — A second local review of the connection-probe change (see
`2026-09-25-LFXV2-2665-probe-verdict-corrections.md`, same day) found six more defects. Only one
changes an `OK` value. The rest change what the operator is TOLD, which is the whole product of
this endpoint: a connection test that answers `OK: false` and names the wrong field has not
helped anyone.

## A throttled token refresh was a permanent credential rejection

`fetchToken` in the Google Ads, Microsoft and Reddit clients split non-2xx on `>= 500` alone. A
`429` is a 4xx, so it landed in `ErrTokenRequestRejected` — which `ProbeCredentialRejected`
matches first, and `probeClass` evaluates rejection before inconclusive by design. A rate-limited
refresh therefore answered `OK: false`, "the platform rejected the stored credential", about a
credential the platform had declined to even look at.

Each of those three packages writes the opposing rule down in its own probe file — *a rate limit
is the platform declining to answer, not answering* — and `ErrTokenRequestRejected`'s godoc
promises the refusal is permanent and survives a retry. Both were false for a `429`. Reddit is
the one that matters most in practice: `/api/v1/access_token` throttles routinely. `429` now goes
with the 5xx side in all three, pinned per platform by a test that drives a real 429 through
`fetchToken` rather than asserting on a synthetic error — the defect was in the classification,
not in the predicates, so a synthetic error would have tested the wrong half.

## Two comments still described the behaviour that was just removed

The previous fix corrected the false HubSpot provenance rationale in four docs and a log entry
and missed the two Go comments — `ProbeConnection`'s own godoc, which still said a `portal_id`
mismatch "is a failed test" fifteen lines above the code that logs a warning and returns `nil`,
and `TestHubspot`'s, which still described the cross-check. That left the Go source as the only
remaining statement of the rule the change existed to delete, which is how a correct piece of
code gets "fixed" back into a defect by the next reader.

## HubSpot's verdict named a portal as though it were an account

`probeSubject{accountID: configured}` fed `portal_id` into `where()`, which renders "for account
X" on a confirmed verdict. So the one field this service documents as routing nothing appeared in
the one message an operator reads as naming the thing that failed. HubSpot's subject is now
account-free — the only one, for the same reason it is the only probe that checks no account.

## A 404 blamed a credential the platform had accepted

Reddit's and X's probes name the configured account IN the request path, so a `404` is the
platform answering the question asked rather than evidence an endpoint moved — that part was
right, and both predicates folded it in with 401/403. But the sentence it produced was "rejected
the stored credential", which sends the operator to re-authorise a credential the platform had
just honoured. The account id is the broken half.

Simply dropping it from the predicate would have been worse in a different direction: an
`apiError` is not inconclusive either, so it would match NEITHER predicate and become
`ErrServiceDefect` — a typed 500 paging us about a connection the operator can repair themselves.
Both packages now export a third predicate, `ProbeAccountUnreachable`, and the dispatchers answer
`accountNotReachable`. It lives in the platform packages because `apiError` is unexported in
both; the dispatcher cannot read a status code it has no type for. Both wrong placements were
reproduced by deleting each arm before the test was trusted.

## Google Ads accepted an account id it can never address

`account_id` is the one provider config declared with no `Pattern` at the design layer, so the
dashed form the Google Ads UI displays — `866-674-6580` — is storable. `ListAccessibleCustomers`
answers undashed and can never contain it, so the membership check missed and the verdict read
"the credential authenticates but does not reach account 866-674-6580" about a credential that
reaches that account perfectly well under the id Google actually uses. The probe now validates
the shape through the client's own exported `ValidateCustomerID` before sending anything, and
answers `accountIDNotUsable`.

## Two guard gaps

`service.ConnectionProber` had no compile-time conformance assertion for its six implementations.
`Orchestrator.ProbeConnection` reaches them through a type assertion, so a drifted receiver or
signature misses silently and turns all six connection tests into typed 500s logged
`reason=probe_unwired` — the endpoint exists and always fails. The source-derived AST guard next
to it proves a `ProbeConnection` method EXISTS and resolves owned credentials; it says nothing
about the signature. The assertions sit beside that guard's roster, which names the same six.

And `unreachableUpstream`'s `hit` flag was written in the `httptest` handler goroutine and read
by the test goroutine with no happens-before edge, while being the assertion the test turns on.
`make test` runs `-race`, so that is a detected failure rather than a theoretical one. Now an
`atomic.Bool`.
