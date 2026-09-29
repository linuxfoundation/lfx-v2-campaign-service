# 2026-09-26 — LFXV2-2665: a token-endpoint timeout that paged us, and the pixel verdict's true scope

**Fix** — the fifth local-review round on the connection-test branch. Two fixes, one of them a
correction to an explanation rather than to behaviour.

## 1. `408` from an OAuth token endpoint was a service defect

Every token-refresh path routed `5xx` and `429` to `errTokenEndpointUnavailable` — the
inconclusive arm — and sent everything else to `classifyTokenRefusal`. A `408 Request Timeout`
therefore fell through to `ErrTokenRequestRejected`, which matches neither probe predicate, and
the connection test answered a typed `500` that pages us.

A `408` is the endpoint, or an intermediary in front of it, giving up waiting for the request.
Nothing evaluated the credential, and the same refresh can succeed on a retry: it is a timeout
wearing a status code, and the probe contract has always said a timeout is inconclusive. It now
joins the `5xx`/`429` arm in `googleads`, `microsoft` and `reddit`, and in `linkedin/token.go`,
which had the same gap under a differently-shaped condition and was not named by the review.
`TestTokenRefresh408IsInconclusive` pins it beside the existing `429` test in the three packages
that have one.

## 2. The pixel verdict's rationale claimed more than the code does

Round 4's `requiredConfigMissing` verdict was written up as catching a connection that "cannot
dispatch anything". That overstates it. `reddit.CampaignInput.ConversionPixelID` is preferred
over the account config when set, and `redditConfig.conversionPixelId` on a brief reaches it, so
a caller who supplies the pixel by hand dispatches fine on a connection that names none.

**The verdict itself is unchanged and still `OK: false`.** The pixel identifies the advertiser and
belongs to the ad account; the service's own create path never fills the override in; so for every
campaign the product actually builds, a connection without an account pixel is rejected by Reddit
before any upstream call. Calling such a connection healthy on the strength of a field only a
hand-written brief can set is the false positive this endpoint exists to remove. What changed is
the writing: the comments in `internal/dispatch/reddit.go` and `internal/dispatch/probe.go` and
the `internal-dispatch` concept now scope "every create" to the campaigns this service builds and
name the override explicitly.

`TestCreateCampaign_CampaignPixelOverridesAnAccountWithNone` pins the override so the distinction
cannot rot: if the override is ever dropped, the probe's reasoning has to be re-read rather than
the test quietly deleted.

Refs: LFXV2-2665
