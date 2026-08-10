# 2026-08-10 — HubSpot metrics: provenance remedy split + bounded portal lookup

Two review findings on PR #113 (Cursor + Copilot), both in the HubSpot email-metrics path.

## The remedy for a row with no recorded provenance was wrong

`ReadMetrics` returned bare `domain.ErrCampaignAccountMismatch` when a campaign row recorded
NO portal at all — not just a different one. `brief.go`'s status mapping turned that into
"reconnect the original account," which the operator cannot act on: there is nothing recorded
to reconnect to. This is every campaign staged before portal provenance existed.

Fix: added `domain.ErrCampaignProvenanceUnknown`, joined with the existing sentinel
(`errors.Join(ErrCampaignProvenanceUnknown, ErrCampaignAccountMismatch)`) so old
`errors.Is(err, ErrCampaignAccountMismatch)` callers keep matching. `brief.go` checks the new,
narrower sentinel first and returns a 409 telling the operator to re-dispatch instead.

## The best-effort portal lookup had no bound of its own

`Dispatch`'s provenance lookup (`client.AuthenticatedPortalID(ctx)`) rode the caller's context
directly. The HubSpot client's own retry policy can wait up to `retryMax*maxRetryWait` = 180s
under sustained throttling — longer than the whole 2-minute `providerCallTimeout` — which could
hand the mutating `CloneEmail`/`SetSendList` calls that follow an already-cancelled context.

Fix: added `portalLookupTimeout = 10 * time.Second` and wrapped the lookup in its own
`context.WithTimeout(ctx, portalLookupTimeout)`, cancelled immediately after. The mutating calls
still ride the caller's own context, unshortened.

## Tests

- `TestHubSpot_ReadMetricsRefusesWhenTheRowRecordsNoPortal` — added an assertion that the error
  wraps `ErrCampaignProvenanceUnknown` specifically, not just the general mismatch sentinel.
- `TestBriefService_GetCampaignMetrics_ProvenanceUnknownIs409WithReDispatchRemedy` (new) — the
  409 message names "re-dispatch," never "reconnect."
- `TestHubSpot_DispatchBoundsThePortalLookupBelowProviderCallTimeout` (new) — a custom
  `http.RoundTripper` (via `hubspot.WithHTTPClient`) captures each outgoing request's context
  deadline; asserts the account-info call's deadline lands within `portalLookupTimeout` of
  `Dispatch`'s start, and that the clone call's deadline is materially further out (it still
  carries A deadline — `doRequest` applies its own per-attempt `context.WithTimeout` to every
  call regardless of caller context — the point is that it isn't truncated to the short budget).

All three mutation-verified: reverting the corresponding fix makes each test fail with a
meaningful diagnostic, confirmed, then the fix was restored.
