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
contract, and there are TWO axes, owned by different places.

The CREATE axis is the mutating caller's. `creds.resolve`'s error already carries
`NoUpstreamCreate`; the helper adds it to nothing else, because a read has no create to disown.
`Dispatch` passes an already-marked error through and wraps the rest in `notCreated`, the same
shape the reddit adapter uses; double-wrapping is what the `errors.As` check exists to avoid.

The AUDIENCE axis is the helper's, tagged at the point of DETECTION. The first cut of this
helper returned its three stored-connection defects — inactive, undecodable, incomplete — bare,
and that was a regression of a bug this repo had already fixed once: `googleads_test.go:1200`
records verbatim that "an inactive or incomplete connection came back off
`resolveGoogleAdsClient` bare, missed [the 409]". Bare, they fall to `GetCampaignMetrics`'
default arm and answer 503 — "the platform did not respond" about a platform that was never
contacted, with a remedy (retry) that no amount of waiting can satisfy, since only a human
editing the connection can fix it. Each now carries `domain.ErrConnectionNotUsable` alongside a
reason sentinel (`ErrConnectionInactive`, `ErrCredentialsUndecodable`, `ErrCredentialsIncomplete`),
which `unusableConnectionReason` maps onto the fixed log vocabulary and the service maps to 409.
The signature is a named return with `defer func() { err = res.systemScoped(err) }()`, copied
from `validateGoogleAdsCredentials`, so a return site added later cannot forget to re-attribute
the error to the LF system row when the credentials came from there; it is a no-op for
project-owned connections and idempotent.

The unmarshal error is DROPPED, not wrapped, for the same reason as the google ads validator.
It is the one error on this path derived from the DECRYPTED credential blob, and `encoding/json`
quotes its input: a `*json.SyntaxError` names the offending character and a
`*json.UnmarshalTypeError` names the field. Retaining it would put credential-derived bytes into
the 503 arm's `safeErrSummary` log line for exactly the connection whose credentials are
malformed. Nothing actionable is lost — the remedy is "re-save the credential", not "fix byte
41". `TestHubSpot_ReadMetricsRejectsUndecodableCredentialsWithoutQuotingThem` asserts the absence
via `errors.As` rather than a substring match on `Error()`, since a cause still in the chain is
reachable by any `errors.As`-walking logger even when the top-level string looks clean.

The token is `TrimSpace`d ONCE in the helper and the trimmed value is what reaches
`hubspot.NewClient`. Not for the wire — the client trims again, so padding could never get
out — but so the emptiness check is made against the value the client will use. Reverting the
trim leaves `TestHubSpot_ReadMetricsRejectsAWhitespaceOnlyToken` failing with "hubspot:
missing private-app token", which is precisely the generic late failure the check exists to
pre-empt. The sibling padding test is honest about not being binding on this layer: it pins
the end-to-end property and says so, rather than implying a guard it does not provide.

## Three places still assumed every metrics read is an ad-platform read

Adding a channel to a shared endpoint makes prose written for one channel false rather than merely
incomplete, and all three cases were user-visible.

`platform_campaign_id` was documented as "ID returned by the ad platform". For an email campaign it
is the HubSpot marketing-email id of the cloned draft — the value the metrics read queries by — and
a client that read the description would have looked for an ad platform that never held it. The
attribute now states both.

The 503 default arm logged "campaign metrics read failed on the ad platform" and answered "could not
be read from the ad platform". HubSpot reaches that arm, and the message directs the caller to check
a system they never connected. Both now say channel. (The status-toggle arm keeps "ad platform" —
HubSpot has no toggle.)

`docs/api-catalog.md`'s canonical metrics row said "Everything else here is Google-Ads-only" and
that no other adapter emits the three `ErrConnectionNotUsable` reasons. `resolveHubSpotClient` in
this change emits all three, so the row contradicted the endpoint's own behaviour. Corrected, with
the part that IS still Google-only kept separate: only Google Ads verifies account identity, and
there is no ad account in an email connection to mismatch, so HubSpot emits neither
`account_not_selected` nor an account mismatch.

## Related

- `docs/knowledge/log/2026-08-09-lfxv2-3058-hubspot-email-metrics.md` — part 1, the client
- `docs/knowledge/code/internal-dispatch.md` — the `MetricsReader` capability
- `docs/knowledge/code/internal-service.md` — status mapping for the metrics read

## The canonical 409 sentence was falsified by this PR's own 409

`docs/api-catalog.md` opened the metrics row's status discussion with "**409** always means the
read was refused before the platform was contacted". That was true of every adapter before this
one. HubSpot's `ErrNoMetricsInWindow` is a 409 raised AFTER a successful upstream call returned
nothing, so the row now describes two different moments under one status: a pre-contact
configuration refusal and a post-contact "no data". They call for opposite operator responses —
repair the connection versus wait for someone to press send — and the response body cannot
separate them, since `ConflictError` carries only `code` and `message`. The row now says so
explicitly and points at the message text as the only discriminator.

## The generated example claimed a mapping the adapter forbids

`campaign-metrics.email` is optional, but Goa emits every attribute into the generated example,
so the published example is always an email-channel response whether or not that was intended.
With no explicit values Goa fabricated independent integers, and the result advertised a nonzero
`cost_micros` on an email response plus `impressions`/`clicks` that did not equal
`email.opens`/`email.clicks` — contradicting, in the same document, the three invariants the
descriptions directly above it state and the adapter enforces. Both types now carry explicit
examples that satisfy the mapping, and `sent = delivered + bounces` besides, so the example is
a legible email response rather than six unrelated numbers.

## LFXV2-3073 — the portal guard, done properly

The first attempt shipped and was reverted the same day: it compared the portal in
`Result` against `client.PortalID()`, and both are `providerConfig["portal_id"]` —
an optional operator-supplied string used only for app URLs. The token decides
which portal a request reaches, and `SetCredentialHubspot` swaps the token without
touching `providerConfig`, so the guard fired on the DECLARED move and stayed
silent on the undeclared swap it claimed to block.

The sound version asks the token: `Client.AuthenticatedPortalID` reads
`/account-info/v3/details`, `Dispatch` records it, `ReadMetrics` re-resolves and
compares before contacting HubSpot. The two callers treat a failed lookup
differently on purpose — `Dispatch` logs and proceeds (account-info may be outside
a private app's scopes, and a campaign that sends and cannot be measured beats one
that does not send), `ReadMetrics` refuses, since an unestablished identity is not
permission to report numbers it cannot attribute. An unrecorded portal is refused
for the same reason, which leaves every campaign staged before this change
unreadable until re-dispatched — accepted rather than worked around, because
nothing about such a row says which portal its bare-numeric email id means.

`ErrCampaignAccountMismatch` was reworded off "ad account", the other half of the
finding: the sentinel now reaches a channel whose operator has no ad account to
reconnect. The status-toggle arm keeps the old wording — no email dispatcher
implements a toggle, so there it really is ads-only.
