# 2026-09-28 — LFXV2-2665: Meta and X charged the provider for a probe they never sent

**Fix** — round 21 of review, raised by the general reviewer. No operator-facing verdict changes;
one upstream metric stops being wrong, and one create classification gets sharper.

`ProbeNotSent` exists for the metrics arm alone: it keeps this deployment's own network or
lifecycle fault off `campaign_upstream_call_duration_seconds` instead of booking it against the
provider's error rate. Google Ads, Microsoft and Reddit each answer a context that was already
done at their token path's ENTRY check with `errTokenContextAlreadyDone`, and their `ProbeNotSent`
reads it. Meta and X had no equivalent, because neither has a token leg for such a check to live
on — Meta carries a long-lived access token as a header, X signs every request with OAuth 1.0a.

So a caller that cancelled before the probe started reached `http.Client.Do` with a done context,
got the context error back, and both clients wrapped it as a `transportError` — a shape
`ProbeNotSent` does not recognise. `probeClass` therefore omitted
`domain.ErrConnectionProbeNotAttempted`, and `Orchestrator.ProbeConnection` recorded
`outcome="error"` against Meta or X for a request the provider never received. Cancellation before
a probe is not exotic: it is what a closed browser tab or an expiring request deadline produces,
so the inflation accrues on exactly the connections an operator was in the middle of testing.

Both clients now check `ctx.Err()` at the entry of their single shared request path — `Client.do`
and `Client.doRequestAbs` — and return `errRequestContextAlreadyDone` wrapped around it, which
each package's `ProbeNotSent` matches. The marker is attached at THAT check and nowhere else. A
context error observed out of `http.Client.Do`, or on a retry attempt after the first, can land
with bytes already on the wire, and the marker's whole value is that it PROVES the failing request
never left this process; widening it would turn the proof into a guess. It wraps rather than
replaces, so `errors.Is(err, context.Canceled)` and `context.DeadlineExceeded` keep answering for
every existing caller. `TestProbeNotSentIsEntryTimeOnly` pins that second half in both packages by
hanging the server until the caller's deadline elapses and asserting `ProbeNotSent` stays false.

On X the marker is deliberately NOT a `preSendError`, even though that type already means "before
the request was sent" and `ProbeNotSent` already matches it. `preSendError` names a DIAL failure
and carries a cause for `safeTransportCause` to strip a request URL out of; there is no dial and no
URL here, and reusing it would have made the type mean two things so that the next reader has to
guess which.

One knock-on, in the right direction: `createOutcomeAmbiguous` in both packages defaults to
"not applied" and had been reading the old `transportError` as "the mutation MAY have been
applied". A create that fails an entry-time context check reached neither provider, so the new
marker makes that classification correct rather than merely conservative.
