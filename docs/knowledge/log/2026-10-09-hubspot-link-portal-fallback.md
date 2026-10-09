# 2026-10-09 — HubSpot app links fall back to the token's portal when portal_id is unset

**Fix** — a HubSpot connection with no `portal_id` built every list and email app link as `""`.
Nothing that checks a connection reads `portal_id` (the connection test answers OK with it blank,
and API calls authenticate on the token alone), so prod's LF system row looked healthy while the
LFX BFF refused every composed or attached master list for its missing link: compose and
attach-existing returned `500` ("blank required field `master.hubspot_url`") while list search
and UTM lookups worked.

- `hubspot.Client.WithLinkPortalFallback` resolves the token's own portal via the existing
  token-info lookup when the row stores none. A stored `portal_id` still wins and is not looked
  up. A failed lookup leaves links blank, as before, and never fails the request.
- The answer is cached process-wide by a SHA-256 digest of base URL + token, since clients are
  built per request: an hour on success, a minute on a definitive failure (4xx, no `hubId`),
  never on a transient one (transport, 429, 5xx, timeout). The lookup ignores the caller's
  cancellation, concurrent cold misses share one call, and the map evicts expired entries and is
  capped at 256.
- Wired only where links are built: the audience builder (all but `Capabilities`) and the
  dispatcher's `ResolveEmailClient*`. `Dispatch` reuses the portal `assertAudiencePortal` verified
  (`Client.WithLinkPortal`), so it still makes one token-info call. `ReadMetrics`,
  `PreflightCreate`, `ProbeConnection`, the monitor, `SearchEmails` and the email-reference
  resolver build no links and make no lookup.
- Operators can still set `portal_id` explicitly (`bootstrap-system-account -config portal_id=…`);
  this change only stops its absence from breaking link-dependent callers.

See [internal/platform/hubspot](../code/internal-platform-hubspot.md).
