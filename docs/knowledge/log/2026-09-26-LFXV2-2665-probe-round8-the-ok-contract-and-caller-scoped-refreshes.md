# 2026-09-26 — LFXV2-2665: the `ok` contract moves, and one-shot probe clients stop outliving their probe

**Update** — Round 8 of the connection-probe work, plus a contract change raised in review.
Five changes landed together because four of them are the round-8 findings and the fifth is the
one that rewrites what every other one of them says.

## `ok` now means what it is declared to mean

`ok` is documented as *whether the credential authenticated against the provider*. An inconclusive
probe — a rate limit, a `5xx`, a transport failure, an error no platform adapter recognises —
established no such thing, and it was answering `OK: true` with an advisory in the `message`. That
put the whole verdict in a field a caller is free to ignore: anything branching on `ok` alone (a
badge, a gate on "can this connection run a campaign") read "fine" for a connection nothing had
verified.

Both inconclusive sentinels moved together — `domain.ErrConnectionProbeInconclusive` in
`testConnUpstream` and `domain.ErrOrgVerificationInconclusive` in `TestLinkedinAds` — so that `ok`
cannot mean one thing on LinkedIn and another on the other six. What keeps the change honest
rather than merely strict is the MESSAGE, which is now the axis that separates the two failing
classes: an inconclusive outcome says the platform could not be reached and says nothing about the
stored credential or pairing; a confirmed rejection names the credential or the field on the row.
Both are `OK: false`, neither is mistakable for the other, and only the rejection class echoes the
platform-derived text.

The consequence for the prose was larger than the consequence for the code. Roughly forty comments
and doc sentences across twenty-two files justified a decision by naming the harm as "reported as
healthy" — and under the new contract the harm is different, not absent: misclassifying a provable
verdict as inconclusive now sends the operator to wait out an outage that is not happening,
instead of naming the field on their own row they could fix. Every one of those sites was
rewritten to the harm that is actually there. Sites where `OK: true` is genuinely correct — a
`nil` probe, HubSpot's unchecked `portal_id`, the historical "answered `OK: true` the moment a
credential blob existed" statements — were left alone deliberately, and dated entries in this log
were not touched at all.

## Round-8 findings

**Stale `TestMetaAds` godoc** (`internal/service/connection.go`) — described the pre-LFXV2-2665
baseline for an endpoint that now probes upstream.

**Microsoft ids validated with the old digits-only rule in the installer**
(`internal/bootstrap/sysacct.go`) — `valueShapes` held `account_id` and `customer_id` to
`^[0-9]+$` while the design declares `^[1-9][0-9]*$` with `MaxLength(19)` and the runtime holds
them to positive int64. `0`, `007`, a twenty-digit value and an above-`MaxInt64` value all
installed onto the SHARED fallback row that every project without its own Microsoft connection
dispatches through. Fixed with a `positiveID` pattern plus a new `valueValidators` second pass
calling `microsoft.ValidateAccountID` / `ValidateCustomerID` — the int64 RANGE is the half no
regexp can express, and the pattern still runs first so the error names the simpler violation when
both apply.

**Pre-send failures lost their provenance before the metrics gate** (`internal/dispatch/probe.go`)
— each platform package now exports a third predicate, `ProbeNotSent(err) bool`, and `probeClass`
consults it after the two that decide the outcome. It changes nothing an operator sees; it has to
be asked at that boundary because the platform error chain is DROPPED there, so past it nothing
can tell a provider that answered badly from one never contacted. The answer reaches exactly one
caller, `Orchestrator.ProbeConnection`'s metrics arm, keeping a local DNS or dial failure off
`campaign_upstream_call_duration_seconds` rather than charging it to the provider's error rate.
Its default runs OPPOSITE to `ProbeInconclusive`'s on purpose — `false`, so an unrecognised error
stays on the upstream series instead of vanishing from it. The outcome is wrapped in
`notSentInconclusiveError`, kept separate from its sibling `preSendProbeVerdictError` because each
enforces the opposite invariant about what it may wrap.

## One-shot probe clients no longer outlive the probe

`googleads`, `microsoft` and `reddit` coalesce the token refresh behind a single-flight whose
leader detaches the call with `context.WithoutCancel`. That is right for a client that OUTLIVES a
request and is shared: one caller's cancellation must not tear down a refresh the other waiters
are parked on, and the token is reused long after that caller is gone.

Neither reason holds for the client `internal/dispatch` builds for one connection probe and drops.
Nothing else will ever read its cache, so the detach buys nobody anything — and it OUTRUNS its
caller, running on the platform's own request timeout, which is longer than the bound
`ProbeConnection` puts on the whole probe. Cancel the probe and the goroutine, its socket and its
file descriptor stay alive to finish work whose result is already unreachable: per probe, on every
connection, on platforms whose token endpoint is the slow part.

Each of the three packages now exports `WithCallerScopedTokenRefresh()`, chosen over a new
constructor or a context-value flag because it keeps ONE construction path shared between the
probe and the monitor read while letting exactly one caller change the refresh lifetime. The probe
call sites opt in: `resolveOwnedGoogleAdsDiscovery` grew variadic `extra` options for it (the
monitor read keeps the detached default), `MicrosoftDispatcher.ProbeConnection` appends it, and
`resolveRedditClientWithCredsCache`'s `!useCache` branch — already the one that neither reads nor
writes the cache — passes it too.

Every one of those call sites COPIES `d.opts` (`append(append([]X(nil), d.opts...), extra...)`)
rather than appending in place. Appending to a slice the dispatcher shares can publish one caller's
extra option to the next through a reused backing array, which would silently give a shared client
the caller-scoped refresh and reintroduce exactly the tear-down the single-flight exists to
prevent. The option's doc says plainly that it is safe only on a client no other caller shares.

`token_refresh_scope_test.go` in each of the three packages asserts BOTH directions — that the
option cancels the in-flight token request, and that the default does not — because flipping the
default would be as much a regression as the leak.

One `net/http` detail cost a debugging cycle and is worth recording: the server does not start the
background read that notices a client hang-up, and so does not cancel `r.Context()`, until the
request body has been consumed. The test handler therefore drains the form body with
`io.Copy(io.Discard, r.Body)` before it blocks; without that the caller-scoped row failed while
the code under test was correct.
