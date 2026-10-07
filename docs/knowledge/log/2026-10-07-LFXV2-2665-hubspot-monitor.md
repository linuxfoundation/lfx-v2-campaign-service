# 2026-10-07 — LFXV2-2665 HubSpot email account monitor

**Update** — New `campaign_manager` read `monitor-hubspot-account`,
`GET /projects/{project_id}/connection-hubspot/account-monitor?days=`, the HubSpot sibling of the
six `monitor-*-ads-account` reads.

- **Scope.** Project-scoped, not account-scoped: the emails this service recorded for the
  project (`CampaignReader.ListRecentProjectPlatformCampaigns`, newest first, 50 campaigns +1 for
  `emails_truncated`), each row's email plus its recorded A/B variant. Empty scope → 200 empty
  with no connection lookup and no HubSpot call. Rows whose creating portal is unrecorded or not
  the token's, or whose id is malformed, are counted in `emails_unattributable` and not read.
- **Upstream.** Token-info once, then the existing per-email
  `GET /marketing/v3/emails/statistics/list` (one `emailIds` value, ≤4 in flight). New
  `hubspot.Client.GetEmailCounters`/`MonitorSpan`/`ValidateEmailID`; `GetEmailMetrics` and the
  new read share `readEmailCounters`, which now runs `identityjson.Check` on the raw bytes and
  refuses an explicit null counter (`ErrNullCounter`) — both apply to the per-campaign read too.
  `AuthenticatedPortalID` also runs `identityjson.Check`. `spamreport` is read by the monitor.
- **Definite or nothing.** Any upstream failure (incl. 401/403 and a 429 after retries) → 503,
  no partial rows; `ErrNoSentEmailInWindow` is counted, never zeros.
- **Rules.** `rules.EvaluateHubSpotMonitor` + `HubSpotRates` (fractions, nil on zero
  denominators, totals from sums); heuristic thresholds documented in `monitor_hubspot.go`.
- **API.** New `HubSpotEmailMonitor`/`…Email`/`…Totals` types with type-level examples and
  `TestPublishedHubSpotMonitorExamplesArePossible`; no cost fields.
- Chart: HTTPRoute hubspot branch, RuleSet entry and parity rows; api-catalog row; concepts
  updated.
- Verified only against HubSpot's published v3 statistics contract and this client's existing
  behaviour, not against a live portal.
