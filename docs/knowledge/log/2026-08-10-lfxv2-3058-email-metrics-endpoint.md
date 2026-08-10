# 2026-08-10 — Email on an ad-shaped endpoint: what the window doesn't mean

**Update** — part 2 of LFXV2-3058. `HubSpotDispatcher.ReadMetrics` makes the email channel a
`MetricsReader`, and `campaign-metrics` gains an optional `email` object. HubSpot is now the
first non-ad-platform on
`GET /projects/{p}/briefs/{b}/campaigns/{c}/metrics`. Part 1 (LFXV2-3058, PR #105) built the
client and its contract; nothing in `internal/platform/hubspot` changes here.

## The endpoint was shaped for ad platforms, and email breaks two of its assumptions

Wiring the adapter is four lines. Everything that took thought is about the two places where
email does not behave like an ad campaign, and where publishing the ad-shaped answer would be
a false one.

**The window does not scope the counters.** HubSpot's statistics span selects WHICH EMAILS are
in scope, by SEND date; the counters that come back are that email's totals to date. Asking
for `today` and `last_30_days` on an email sent this morning returns identical numbers. The
result's `window` field therefore records what was ASKED, not a period the numbers cover, and
a UI that renders "opens in the last 7 days" from it is lying. Genuine event-time windowing
needs a different HubSpot source (the email-events API, which timestamps each open and click)
and is deliberately not attempted. This is documented on the client, in the api-catalog's
per-platform table, and in `internal-dispatch.md`, because it is the kind of caveat that
survives only where a consumer will actually trip over it.

**Zero cost is not free.** `CostMicros` is always 0 for email — HubSpot bills no per-send
cost. Blended into a cross-channel cost-per-acquisition that 0 divides real ad spend across
email conversions and understates CPA. The `email` object is a separate OPTIONAL type rather
than six more attributes on `campaign-metrics` for the same class of reason: an ad-platform
response carrying six structurally-zero counters is indistinguishable, to a client, from an
email that sent nothing.

## The 503 default was wrong for the ordinary case

`hubspot.ErrNoSentEmailInWindow` is what an empty match produces, and left unmarked it took
the service's 503 default — "campaign metrics could not be read from the ad platform".

That is backwards for this channel. `Dispatch` stages the cloned email as a DRAFT for a human
to send, so *every* metrics read between staging and the send lands here. The dominant state
of a healthy integration was reporting itself as an outage.

New sentinel `domain.ErrNoMetricsInWindow`, joined onto the platform cause by the adapter and
mapped to 409 by the service. Two details worth keeping:

- It claims only what the response establishes. Sent-outside-the-window, never-sent, and
  no-such-id arrive in one indistinguishable shape, so the message names all three rather
  than picking the likeliest. Naming the first would send an operator hunting for the right
  window for an email no window will ever find.
- It is not zeros. Zeros here are indistinguishable from the other zero — an email that WAS
  sent and earned no opens — and collapsing the two answers "no engagement" about a campaign
  that may be getting plenty. That reasoning is the client's (part 1); this change is what
  stops the service from throwing it away at the last step.

`TestGetCampaignMetrics_NoDataInWindowIs409NotAnOutage` asserts the ABSENCE of the 503 as well
as the presence of the 409, so a future refactor that reaches the default again fails on the
symptom that matters.

## resolveHubSpotClient, and who owns the mark

`ReadMetrics` was the second caller of `Dispatch`'s credential sequence, so it came out into
`resolveHubSpotClient` rather than being inlined a third time. The subtle part is the error
contract: the helper returns its own errors UNMARKED. `creds.resolve`'s error already carries
`NoUpstreamCreate`, and marking everything else is the MUTATING caller's job — a read has no
create to disown. `Dispatch` passes an already-marked error through and wraps the rest in
`notCreated`, the same shape the reddit adapter uses; double-wrapping is what the `errors.As`
check exists to avoid.

The token is `TrimSpace`d ONCE in the helper and the trimmed value is what reaches
`hubspot.NewClient`. Not for the wire — the client trims again, so padding could never get
out — but so the emptiness check is made against the value the client will use. Reverting the
trim leaves `TestHubSpot_ReadMetricsRejectsAWhitespaceOnlyToken` failing with "hubspot:
missing private-app token", which is precisely the generic late failure the check exists to
pre-empt. The sibling padding test is honest about not being binding on this layer: it pins
the end-to-end property and says so, rather than implying a guard it does not provide.

## Related

- `docs/knowledge/log/2026-08-09-lfxv2-3058-hubspot-email-metrics.md` — part 1, the client
- `docs/knowledge/code/internal-dispatch.md` — the `MetricsReader` capability
- `docs/knowledge/code/internal-service.md` — status mapping for the metrics read
