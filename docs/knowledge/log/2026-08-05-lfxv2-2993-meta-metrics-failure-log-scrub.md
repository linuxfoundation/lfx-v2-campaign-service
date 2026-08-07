# 2026-08-05 — LFXV2-2993 Meta metrics: scrub the failure-path log

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
