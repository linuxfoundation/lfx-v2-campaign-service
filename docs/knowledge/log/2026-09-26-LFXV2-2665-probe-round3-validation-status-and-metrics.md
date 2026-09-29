# 2026-09-26 — LFXV2-2665: id validation at the design layer, a 503 for an unreadable row, and probe verdicts that were never upstream calls

**Fix** — the third local-review round on the connection-test branch. Five findings fixed; a
sixth, the wording of `ok` in the Goa contract, is held for a product decision and is not
included here.

## 1. A malformed `customer_id` reported a broken Microsoft connection as healthy

`discoveryCustomerIDs` always refused a `customer_id` that is not a positive `int64` — correctly,
since no request can be built from it — but it refused with an unsentineled error. Neither probe
predicate recognised it, so `ProbeInconclusive`'s unrecognised-error default answered `true` and
the service reported `OK: true` for a connection that can never dispatch. `customer_id` is
operator-settable through the connection config API, so that is a reachable state, not a
theoretical one.

`microsoft.ValidateCustomerID` and `microsoft.ErrInvalidCustomerID` are now exported — one rule,
two callers, neither writing its own — and `internal/dispatch/microsoft.go` consults the
validator before sending anything, answering the new `customerIDNotUsable` verdict.
`TestMicrosoftProbe_MalformedCustomerIDIsAVerdictNotInconclusive` covers `abc`, `0`, `-1`, `1.5`
and an int64 overflow.

## 2. A Graph code was read under statuses Meta never uses to deliver it

`meta.ProbeCredentialRejected` matched codes `190`, `200` and `10` whatever the HTTP status. It
is evaluated BEFORE the inconclusive predicate, so a `429` or a `5xx` that happened to carry code
`190` — a shed or failed request that evaluated nothing — became a CONFIRMED credential
rejection, sending an operator to reauthorize a credential Meta never looked at.

**The status gates the code, never the reverse** — the round-2 rule, now applied on the Graph
side too. Only HTTP `400` reaches the code switch; `401`/`403` are still rejections on the status
alone.

## 3. An unreadable connection row answered 500

`testConn` routed a failed `repo.Get` through `mapErr`, whose default arm is
`InternalServerError`. `docs/api-catalog.md`'s `/test` row says the opposite and says why: a
failure to READ the row is a **503** — nothing was learned, and it is the one outcome on this
endpoint that retrying can fix. So a dropped connection or a statement timeout answered a
permanent-looking status for the one transient condition here, and paged whoever owns the code
instead of telling the caller to try again.

The read failure is now answered directly with `ConnServiceUnavailableError`. `ErrNotFound` keeps
its 404: that read succeeded and returned the absence. `TestTestConn_UnreadableRowIs503NotAn500`
covers both entry points and asserts the prober was never called — nothing may be sent upstream
on the strength of a row nobody could read — and `TestTestConn_AbsentRowIsStill404` is the
regression half.

## 4. The design layer accepted ids the runtime refuses

`Required` on a connection-config attribute checks that the KEY is present, nothing more. So
`{"account_id": ""}` was storable on an active connection, and Google Ads' dashed UI form
`866-674-6580` — which no Google Ads API response can ever contain — was storable too, which is
why the probe needs a runtime shape check at all.

`Pattern` + `MaxLength` now sit on every operator-settable id: Google `account_id` and
`login_customer_id`, Reddit `account_id`, Microsoft `account_id` and `customer_id`. The audit
that produced that list ran over all six providers rather than the two flagged — Microsoft
carried the identical defect, and its own source comment named the gap ("its Goa design only
checks presence … control characters could inject a header", for a value sent as a request
header). HubSpot is correctly left alone: its `portal_id` has no runtime shape rule stricter than
presence.

Each pattern mirrors a rule its platform client already enforces, and the runtime checks stay —
Goa validates the HTTP transport, and bootstrap, migrations and rows written earlier bypass it.
Where `""` is a supported runtime state (Google's credentials-first `account_not_selected`,
Microsoft's unscoped customer) the pattern admits it; a design stricter than the runtime is the
mirror image of the defect being fixed.

`internal/apivalidation` gained a drift guard per platform, and writing it changed the Microsoft
pattern: the first version disagreed with `ValidateCustomerID` on three ids. Two of the three
disagreements are deliberate and are now asserted by name rather than dropped from the table — a
padded id is refused at the transport although the platform validator trims, and a 19-digit value
above `MaxInt64` is the residual gap no regex can express, which is why `ParseInt` must stay.

## 5. Verdicts decided before any request were counted as upstream calls

`Orchestrator.ProbeConnection` recorded an upstream call for every probe outcome. Three of them —
the connection names no ad account, its account id is not usable, its customer id is not usable —
are decided by the dispatcher before a request is built, so a platform with a few misconfigured
rows showed an upstream error rate and near-zero latency samples for calls it never received.
That is exactly the local refusal `recordUpstream`'s own contract says it is called after the
guards to avoid; these guards simply live inside the dispatcher and cannot be hoisted above the
timer.

`domain.ErrConnectionProbeNotAttempted` is a MARKER, never a status: it rides alongside
`ErrConnectionProbeFailed` on those three verdicts (`preSendProbeVerdict`), renders the same
sentence, and has one reader — the metrics arm, which skips recording when it matches. The
operator-facing answer is unchanged.

`probeMembership` renders the same no-account sentence WITHOUT the marker, because reaching that
line means the enumeration completed and a real call belongs in the series. Both halves are
pinned verdict by verdict in `probe_not_attempted_test.go`, and the orchestrator side in
`orchestrator_metrics_test.go` — including the regression half, that dropping the metric must not
drop the verdict.

Refs: LFXV2-2665
