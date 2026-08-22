# 2026-08-21 — LFXV2-3281 a signal only a log carries, and one repeated to death

**Fix** — three defects raised after this branch merged main. Two are about the same thing
from opposite ends: a diagnostic that is the ONLY evidence a fault occurred, logged too
quietly to be seen; and a diagnostic that is correct but repeated until it is filtered out.

## A successful response makes the log the entire signal

`classifyBriefMetricsErr` gained a `domain.ErrServiceDefect` arm, mapping a defect in this
service to the `failed` row status. The per-row log-level switch a few lines below was not
extended to match, so the fan-out logged it at WARN while `GetCampaignMetrics`,
`ToggleCampaignStatus` and the discovery handler all log the identical sentinel at ERROR
beside a 500.

An ordinary level inconsistency would be a nit. This one is not, and the reason is the
endpoint's shape: `GetBriefMetrics` returns a SUCCESSFUL aggregate. The defect row lives
inside a 200. There is no status code carrying the alarm, no error returned to the caller,
nothing for a client to branch on — the log line is the whole of the evidence that a defect in
this service occurred. Emitted at WARN it sits below the threshold anybody watches, and the
caller has already been told the request succeeded.

The generalisation worth keeping: **when a path degrades a failure into a successful
response, the diagnostic's LEVEL stops being a formatting preference and becomes the
signal itself.** Ask what else would carry the alarm if the log did not. On the synchronous
paths the answer is "a 500"; here the answer is "nothing".

The arm's comment also read "All three are OPERATOR-scope defects…" — an enumeration that
adding a fourth sentinel falsifies with nothing failing. It now states the property (a defect
on a scope no caller owns) instead of counting the members.

## A 30-day window that re-fires per operation is not a warning, it is noise

`warnIfRefreshTokenNearExpiry` fires whenever a refresh exchange lands inside the final 30
days. That reads as once-ish. It was not: `internal/dispatch/linkedin.go` constructs a Client
PER OPERATION at four call sites, the token cache is per-`Client`, and no access-token expiry
is persisted — `accessTokenValue`'s own comment already said "EVERY refresh-capable client
performs a token exchange on its first request … a brief-level fan-out is one OAuth exchange
per campaign". So the warning fired once per campaign per operation, for a month. A test
constructing 25 per-operation clients logged 25 identical lines.

Deleting it was not on the table — an expiring credential dying silently is the defect this
branch exists to prevent. Three shapes were weighed:

- **Narrow the window** — fewer lines, but the SAME per-operation shape, bought by deleting
  most of the notice period. The 30 days exist so a reconnect can be scheduled.
- **Move it to a once-per-process path** — the right long-term home, but this package never
  reads the database and receives credentials injected per operation, so nothing enumerates
  connections to sweep. That is a service-layer change.
- **Dedupe per process per connection** — chosen. Keeps the full 30-day notice and full
  per-connection coverage for one map lookup; a restart re-arms it, which is a sensible floor.

Two details the implementation turns on. The key is NOT `ConnectionLabel()` alone: it falls
back to a shared constant for any connection whose operator set no name, so keying on it would
let the first unnamed connection to warn silence every other — the exact failure per-connection
coverage exists to prevent. And it uses `LoadOrStore`, not check-then-set, because concurrent
dispatches for one connection reach the guard in parallel and a read-then-write would let
several of them all observe "not yet warned".

The test asserts the COUNT across independently constructed clients, and asserts a SECOND
connection still warns. A single-client test would have passed against the defect, since one
client exchanges once anyway.

## Neither test could have been written as "a log happened"

Both defects are invisible to a presence assertion. A WARN and an ERROR are the same record
with the same message; one warning and twenty-five warnings are the same line. Both tests had
to bind the fixture to the classification first — the row's status is `failed`, the warning
fired at least once — or the real assertion would have passed vacuously against a fixture that
never reached the arm.

## A count in prose is falsified silently

`docs/api-catalog.md` documented `token_request_rejected` as a service defect while three
separate 500 enumerations still said there were exactly two cases: the toggle row, the metrics
row, and — found only by sweeping rather than by following the report — the account-discovery
row, whose handler `internal/service/connection.go` gained the same third arm. All three now
state the property (defects on a scope the caller cannot address) instead of a number.
