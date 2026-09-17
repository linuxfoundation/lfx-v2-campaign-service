# 2026-09-17 monitor account endpoints — PR review fixes

**Fix** — Copilot review on the account-monitor-endpoints PR found and fixed
three more real defects, on top of the differential-verification fixes
already logged in
[2026-09-17-monitor-account-endpoints.md](2026-09-17-monitor-account-endpoints.md):
all four rule engines ran a `FetchFailed` row's placeholder zero-value
metrics through pacing/action-item evaluation instead of skipping it,
fabricating findings against campaigns whose metrics fetch actually failed
(each now checks `FetchFailed` at the top of its loop and returns the row
unevaluated); Google's underspending action-item text hardcoded a 30-day
expected-spend window instead of using the caller's real `days` parameter;
and `monitorAccount` aborted the whole endpoint whenever Reddit's separate
account-totals call failed, instead of falling back to summing the
already-fetched per-campaign rows the way the capability-absent path already
does. See
[Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md)
for the full writeup.

**Docs** — `docs/api-catalog.md` catalogued the four new
`account-monitor` endpoints, which had shipped without an entry there.
