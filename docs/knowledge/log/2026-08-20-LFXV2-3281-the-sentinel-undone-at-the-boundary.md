# 2026-08-20 — LFXV2-3281 a sentinel undone one layer down

**Fix** — four defects from a triage of SUPPRESSED Copilot comments on PR #148 (invisible to
`reviewThreads`; the PR reads `unresolved=0` while they sit unread). The first is the one
worth the title: a reason sentinel created to stop misdirecting operators was wrapped, at the
next layer down, in the very status sentinel that misdirects them.

## The reason axis and the status axis are not the same axis

`domain.ErrTokenRequestRejected` exists because LinkedIn's RFC 6749 §5.2 request/protocol
codes (`invalid_request`, `unsupported_grant_type`, `invalid_scope`) describe something THIS
SERVICE built, not either stored credential. Its docstring is unambiguous about the remedy:

> there is no field on a connection whose editing makes a malformed refresh request
> well-formed … The correct remedy is … "this is a service defect, file a bug".

`linkedinExpiry` then wrapped it like this:

    return fmt.Errorf("%w: %w: %w", domain.ErrConnectionNotUsable, domain.ErrTokenRequestRejected, err)

Every consumer of `ErrConnectionNotUsable` answers a CALLER-FAULT status. Verified at each,
by asserting the response rather than the error value:

| consumer | status | what the operator reads |
| --- | --- | --- |
| `brief.go` metrics | **409** | "…repair the connection before reading metrics" |
| `brief.go` toggle | **409** | "…repair the connection before changing campaign status" |
| `brief.go` adoption | **409** | "…repair the connection before adopting a campaign" |
| `connection.go` discovery | **400** | "the stored linkedin ads connection cannot be used as configured: check that it is active and that the stored credential is valid json with access_token set" |
| `brief.go` brief-metrics row | `connection_problem` | "…not usable — reconnect it to read metrics" |
| `orchestrator.go` async | *(log only)* | "platform dispatch failed before upstream create" |

So the sentinel built to stop sending operators to audit a correct configuration sent them
there on every path, and `reason=token_request_rejected` — the one token in the vocabulary
that points at us — reached only `unusableConnectionReason` and the log. The classification
was right the whole time; the MAPPING was the defect, which is why an `errors.Is` test would
have stayed green through all of it. **Getting the reason axis right does not settle the
status axis.**

`domain.ErrServiceDefect` now carries the status, wrapped ALONGSIDE the reason (the
`ErrSystemConnectionNotUsable` arrangement), and is matched ABOVE the general arm at all six
consumers. A 5xx, because "retry" and "fix your request" are both wrong and the only actor who
can act reads the log.

**A test encoded the bug.** `TestLinkedinExpiryTagsTokenRequestRejected` REQUIRED
`ErrConnectionNotUsable`, reasoning that the tag keeps the error off the retryable 503 arm.
It does — and it also selects a caller-fault 4xx. Both sentinels keep it off the 503; only one
tells the truth about who has to act. Rewritten to require `ErrServiceDefect` and to refuse
`ErrConnectionNotUsable`.

## A count a cache can satisfy is not a measurement

The single-flight test asserted `exchanges == 1` across 25 concurrent callers, with a
`time.Sleep(50ms)` in the handler and a comment claiming the delay "guarantees the followers
arrive while the leader's fetch is in flight". It guarantees nothing: nothing forces the
followers to arrive during the fetch, and a caller that arrives late finds the CACHE the
leader populated and makes no exchange. Both causes produce 1.

Demonstrated rather than argued. With the `inflight` join deleted outright — no coalescing
whatsoever — and callers staggered past the leader's completion, the sleep-based assertion
reported `exchanges = 1` and **passed**. The replacement has every caller signal arrival, waits
for all N, and only then releases the handler, so no exchange can complete before the last
caller arrives. Under the same deletion it reports `exchanges = 25`.

The residual gap — a goroutine descheduled between signalling and calling — is closed by the
DIRECTION of the remaining race, not by more synchronization: a late arrival finds the cache
still empty, so it can only ever produce an EXTRA exchange, never a spuriously coalesced one.
The test can now fail only in the direction that indicates a real defect.

## Supplied-but-empty is a value, not an absence

`validateLinkedInRefreshCredentials` counted presence as "non-nil and non-blank", then read
`present == 0` as "all three omitted", the supported bearer-only case. So:

    {"refresh_token": ""}   ->  present == 0  ->  accepted, stored

`CanRefresh()` reads the blank as absent, the connection is bearer-only, and the operator —
who watched the field be accepted — believes renewal is configured until the ~60-day expiry
this feature exists to prevent. That is verbatim the failure the function's own docstring
claims to stop. `present == 0` was carrying two incompatible meanings: three nil pointers
(legitimate) and supplied-but-empty (a defect).

`internal/bootstrap/sysacct.go` already had this right — "A supplied-but-blank string (`""` or
`"   "`) is a supplied key holding no credential" — and faults on it. Two boundaries write the
same trio; one refusing while the other canonicalizes to absence is a difference no operator
can see until renewal silently never happens. The API boundary now distinguishes nil from
non-nil-but-blank and refuses the latter before the all-or-none verdict.

## A shared sentinel is a claim about the owner, not the remedy

`oauthAppFaultCodes` holds `invalid_client` AND `unauthorized_client` under
`ErrApplicationCredentialsInvalid`, and they shared one message: the stored
`client_id`/`client_secret` "are wrong or unknown to LinkedIn". True of the first; false of
the second, which this branch's own taxonomy comment identifies as *the app lacking
refresh-token grant (a Marketing Developer Platform approval)* — where both credentials are
entirely correct. An operator handed that text re-pastes a correct pair and never hears about
the approval.

The sentinel split is right and is kept: both are permanent, both name the application
registration, both have the same owner. Only the ACTION differs, so only the text does.
`applicationCredentialsError` now carries the §5.2 code and selects the message from it. The
code is safe to render because it is matched against `oauthAppFaultCodes` before being stored,
so only one of the file's own constants ever reaches the field — no upstream free text travels
with it.

## Also

`docs/knowledge/code/internal-bootstrap.md` claimed "padding on a key the provider does not
require [is] left alone". This branch falsified it: `validateConditionalGroups` refuses padding
for every member of a conditional group, and LinkedIn's trio is exactly such a group. A
preserved context line, not one this branch wrote — but invalidating it makes it ours.
