# 2026-08-05 — Meta metrics reads

**Update** — Added campaign metrics reads for Meta: a new optional `MetricsReader` dispatcher
capability is wired for Meta in `internal/dispatch/meta.go`'s `ReadMetrics`, backed by a new
`internal/platform/meta/metrics.go` Graph API insights client method (`GetCampaignMetrics`):
campaign id and metrics window (fixed allow-list of supported date_preset values, e.g.
`LAST_30_DAYS`) are both validated before string interpolation into the request, since Meta's
insights endpoint has only fixed preset values. Platform-agnostic domain type `model.CampaignMetrics`
(Impressions/Clicks/CostMicros/Ctr) is distinct from the platform-level `meta.CampaignMetrics`,
converted at the dispatcher boundary — mirrors the Google Ads GA-5 pattern exactly. Cost is
expressed in micros of the ad account's currency (multiplying Meta's spend value by 1,000,000),
matching Google Ads' unit so a platform-agnostic dispatcher can normalize all platforms to the same
scale. The existing platform-agnostic `/metrics` endpoint (wired via the orchestrator's optional-capability
type assertion) now works for Meta campaigns, same as Google Ads (LFXV2-2993).

**Update** — Resolve outstanding Copilot review thread on PR #72 Meta metrics reads
(`internal/platform/meta/metrics.go:160`).

The spend-overflow guard compared the UNROUNDED `spend * 1_000_000` product against
`math.MaxInt64` with `>`. `math.MaxInt64` is not exactly representable as a `float64` —
`float64(math.MaxInt64)` rounds UP to `2^63`, one past the real `int64` max — so a scaled
value in `[2^63-0.5, 2^63)` is not `> 2^63` and slips past the guard, then overflows on
`int64(math.Round(spend))`, corrupting the reported cost. Mirrors the existing correct
pattern in `client.go`'s `applyBudget`: round FIRST, then compare the rounded value with
`>=` (and `<=` on the low end) against `float64(math.MaxInt64)` /
`float64(math.MinInt64)`, so the rounded boundary itself is rejected.

Added `TestGetCampaignMetrics_SpendAtInt64BoundaryOverflows`, pinning a spend value whose
scaled product rounds to exactly `2^63`; verified it fails without the fix and passes
with it.

**Update** — Copilot flagged that `GetCampaignMetrics`'s failure-path log (`internal/service/brief.go`)
wrote `merr` — the ad-platform read error — into structured logs unbounded and unscrubbed.
Meta's `*APIError.Error()` renders the Graph API's `Message` field verbatim (the parsed error
message, or the raw response body when the envelope isn't a Graph error), which is untrusted:
it can echo request material back, and it is unbounded. Note the log-forging half of this
is handler-dependent, not universal: slog's TextHandler and JSONHandler both quote attribute
values, so a raw newline in the value does not by itself split a record under either. What no
handler does is BOUND the value -- a multi-kilobyte upstream body lands in the sink whole. This call site is platform-agnostic (shared by every
`ReadMetrics` implementation) and can't assume every platform's `Error()` has already scrubbed
its response text the way LinkedIn/Reddit/Twitter/Google Ads/Microsoft's client errors
deliberately do. Added `safeErrSummary` in `brief.go`: replaces every non-graphic rune with U+FFFD
(visible substitution rather than a silent drop) and caps the result to 200 runes plus an
ellipsis marker before it's logged. The cap counts RUNES, not bytes, so a multi-byte body is
truncated at a boundary instead of being split mid-rune. Scoped to this one
log call rather than changing Meta's `APIError.Error()` itself, which is pre-existing,
deliberately tested behavior relied on elsewhere (surfacing the raw Graph body for campaign
creation diagnostics) — out of scope for this metrics PR.
