# 2026-09-17 monitor account endpoints — local review round 2

**Fix** — A second local pre-PR review round (general reviewer) on the
account-monitor-endpoints branch found five more defects, on top of the
Copilot-review fixes already logged in
[2026-09-17-monitor-account-endpoints-pr-review-fixes.md](2026-09-17-monitor-account-endpoints-pr-review-fixes.md):

1. All four rule engines' `FetchFailed` branch emitted
   `PacingLabel: MonitorPacingNormal` — asserting a computed "normal" pacing
   verdict for a row whose metrics fetch never succeeded, the same
   fabrication-of-a-finding defect class the earlier `FetchFailed`-skip fix
   was meant to eliminate, just relocated into the pacing label. Each now
   sets `PacingUnknown = true` on the row instead, per
   `AccountCampaignMetrics.PacingUnknown`'s own doc comment
   (`internal/domain/model/monitor.go`): "a rule engine MUST NOT compute a
   pacing percentage against a fabricated flight window when this is true."
2. `internal/platform/linkedin/monitor_test.go`'s clock-injection test used
   an already-UTC injected clock, so it could not actually distinguish a
   `.UTC()` call from its absence — reverting the normalization left the
   test green. Fixed by injecting a `time.FixedZone` clock whose local
   calendar date differs from its UTC one, so the test now fails without
   the normalization.
3. `connection_monitor.go`'s totals-fallback failure path logged the raw
   `error` value for a call that can carry `domain.ErrConnectionNotUsable`,
   violating this repo's no-raw-error-leak rule
   (`classifyDiscoveryError`'s `ErrConnectionNotUsable` arm,
   `internal/service/connection.go`) — one of that sentinel's detection
   paths decodes a decrypted credential blob. Now logs
   `unusableConnectionReason(terr)`, the same fixed-vocabulary token that
   arm already uses, at `Warn` rather than `Error` (a handled/recovered
   condition, not one that pages anyone).
4. The same call seeded its row-count argument from `len(metricsRows)`
   (pre-rule-engine-filter) instead of `len(rows)` (post-filter, the slice
   actually returned in the response's `campaigns` array) — the same
   mismatch already fixed once for the sibling fallback path 11 lines
   below it in this function.
5. Google and Reddit's `account_id` design attributes carry
   `MinLength`/`MaxLength` only, no `Pattern`, so a malformed-but-nonempty
   id (e.g. non-digit for Google) passed Goa's own validation and reached
   `gaqlSearchForCustomer`'s (or Reddit's `ListAccountCampaigns`'s) own
   unsentineled shape error, which `classifyDiscoveryError`'s default arm
   mapped to an opaque 503 — unlike LinkedIn/Meta, whose `Pattern` refuses
   the same malformed id at the HTTP boundary with a clean 400. Fixed with
   a new sentinel, `domain.ErrAccountIDMalformed`, mapped to 400 in
   `classifyDiscoveryError`; Google's and Reddit's
   `ListAccountCampaignMetrics` dispatchers now validate the id shape
   themselves (`googleads.ValidateCustomerID`; Reddit's existing
   `accountIDRe` check) and wrap a shape failure in the new sentinel. See
   [Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md)
   for the full writeup.

**Docs** — The same review also caught that the Meta wall-clock-to-injected-
clock fix landed in commit `1061e662` had never been recorded in the
knowledge base at all. Added to
[Account-Monitor Endpoints](../architecture/account-monitor-endpoints.md)'s
Meta section.
