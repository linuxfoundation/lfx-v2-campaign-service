# 2026-10-07 — LFXV2-2665 HubSpot monitor auth errors and as-of timing

**Fix** — PR #290 review threads on `monitor-hubspot-account`.

- **401/403 is a connection defect, not a retryable 503.** A permission rejection on token-info or
  statistics is tagged `domain.ErrConnectionNotUsable` through `res.systemScoped`, exactly as
  SearchEmails, SearchCampaigns and CreateCampaign tag theirs: 400 for the project's own token,
  500 attributed to the operator-owned row for the LF fallback token. Still no partial result.
- **`metrics_as_of` is the end of collection.** It was captured before token-info and the fan-out;
  it is now the client clock at the LAST upstream response (`hubspot.Client.Now`, new
  `hubspot.WithClock` for pinning), an upper bound on when every counter was read.
  `MonitorSpan` no longer returns an as-of.
- **Truncation comments** on the repo port, repo query and cap constant now state the
  conditional rule the orchestrator applies rather than "an extra row means truncated".
