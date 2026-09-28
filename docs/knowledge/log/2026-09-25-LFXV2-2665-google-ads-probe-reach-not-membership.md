# 2026-09-25 The Google Ads probe was reading the account picker's filtered list as membership

**Fix** — `GoogleAdsDispatcher.ProbeConnection` answered "does this credential reach the
configured account" by checking whether the id appeared in `Client.ListAccessibleCustomers`. In
manager mode that enumeration is not a membership list at all: it is the account **picker's**,
and `listManagerClients` narrows it twice — `WHERE customer_client.status = 'ENABLED'`, and a
drop of every row where `customer_client.manager` is true — because those are the accounts a
campaign may be created in.

Presence in that list is sound and stays sound: it proves the connection can dispatch. **Absence
proves nothing.** A suspended, cancelled or closed account, and a sub-manager account, are each
reached perfectly well by the credential and each missing from that list. Every one of them came
back as the confirmed, operator-facing verdict "the google-ads credential authenticates but does
not reach account 1234567890" — false, echoed verbatim, and pointing the operator at an account id
that is correct. The remedy the message implies (repoint the connection) is the one thing that
cannot help.

The probe now asks its own question through `googleads.ProbeAccountReach`, which returns the
four-valued `AccountReach`: `AccountReachable`, `AccountIsManager`, `AccountNotEnabled`, and
`AccountUnreachable` — the latter deliberately the ZERO value, so a reach returned alongside a
non-nil error, or from a caller that forgot to assign one, can never read as reachable.
`internal/dispatch/probe.go` gains `accountIsManagerAccount` and `accountNotEnabled` beside
`accountNotReachable`; they are two verdicts rather than one because the remedies differ — a
manager account means the connection names the wrong LEVEL of the hierarchy, a disabled one means
the account needs reinstating in the Google Ads UI — and neither is "check your credential". Both
sentences are authored in `probe.go` like every other verdict; the platform's own `status` string
is compared against in the client and never travels to the operator.

**The picker is byte-identical to before, and that is the shape of the fix.** Rather than widening
the shared query, the decoder was extracted as `queryCustomerClients` and each caller given its own
constant: `campaignCapableClientsQuery` (the picker's, with the `WHERE`) and `allClientsQuery` (the
probe's, unfiltered, with the `manager`/`status` reading done in Go). The picker therefore sends
exactly the bytes it sent before, so its row set, its id-validation failures and its output are
unchanged — pinned by the existing assertion in `accounts_test.go` that the `WHERE` clause is on
the wire. Direct mode (no `login_customer_id`) is untouched: `ListAccessibleCustomers` is
unfiltered there, so its answer was never wrong, only narrower — it cannot distinguish a manager
or disabled account, because the flat endpoint returns neither flag.

`TestGoogleAdsProbe_ReachedButNotCampaignCapable` pins all three verdicts and additionally asserts
the picker's `status = 'ENABLED'` predicate is NOT on the probe's wire. Both halves were proved
non-vacuous: restoring the `WHERE` into `allClientsQuery` fails all three subtests on the query
assertion, and collapsing the two new arms back to `AccountUnreachable` fails the first two with
the production defect's sentence verbatim.

The same pass fixed a race in the neighbouring
`TestGoogleAdsProbe_DashedAccountIDDoesNotBlameTheCredential`: its `reached` flag was a plain
`bool` written from the handler goroutine and read from the test goroutine. The passing case hides
it — the probe short-circuits before any request, so the write never happens — which is exactly the
trap, since the moment the guard earns its keep `-race` reports a race on top of the real assertion
failure and buries the regression the guard exists to name. It is now an `atomic.Bool`, matching
the sibling helper in `probe_no_account_test.go`.
