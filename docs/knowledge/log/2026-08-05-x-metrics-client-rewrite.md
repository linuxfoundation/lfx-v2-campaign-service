# 2026-08-05 — X Ads metrics client rewrite (LFXV2-2996)

**Update** — Rewrote the `internal/platform/twitter` metrics read after review found the
original implementation non-functional against the real X Ads API: it requested `stats` under
the account-scoped path (`/accounts/{id}/stats`) when the real endpoint is
`/stats/accounts/{id}`, and its request/response contract (flat `campaign_id`/`spend` string
fields, `granularity=ALL`, missing `metric_groups`/`placement`) didn't match X's documented
nested `id`/`id_data`/`metrics` shape (with `billed_charge_local_micro`) — a successful
response would have decoded to all zeros.

Extracted `doRequest`'s retry/OAuth core into a new `doRequestAbs` so the stats call can target
its own non-account-scoped URL while keeping the same 429 backoff and OAuth1 signing behaviour;
added a `statsURL()` helper.

Added typed `ErrInvalidCampaignID` / `ErrUnsupportedWindow` sentinels (`errors.Is`-discriminable),
returned directly without `fmt.Errorf` wrapping so callers can tell window-validation failures
from upstream/transport failures.

Fixed hour-alignment of the stats `end_time` parameter: changed from `endDate+"T23:59:59Z"`
(non-hour-aligned, rejected or silently rounded by X's API) to an exclusive next-midnight bound
`(endDate+1)+"T00:00:00Z"`, matching X's whole-hour-aligned timestamp requirement.

Added `accountIDRe` validation before interpolating `c.account.AccountID` into the stats URL
path, mirroring the guard already applied by the other Twitter client paths, so a malformed
stored account ID cannot inject path segments.

See `2026-08-05-x-metrics-platform-aware-default-window.md` for the platform-aware
omitted-`window` default this client's 7-day range cap forced onto the shared handler.
