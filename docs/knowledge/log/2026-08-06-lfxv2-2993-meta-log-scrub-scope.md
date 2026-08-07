# 2026-08-06 — LFXV2-2993 Meta log scrub: scoped to the log call, not APIError

**Update** — Scoped to the log call, not to Meta's `*APIError.Error()`. That method renders the
Graph API's `Message` field verbatim, and the non-Graph fallback fills that field from the raw
response body (`internal/platform/meta/client.go:589-612`). Changing it is out of scope here: the
raw-body behavior is pre-existing, deliberately tested, and relied on for campaign-creation
diagnostics. The metrics log site is the right place because it is platform-agnostic — every
`ReadMetrics` implementation funnels through it, so it cannot assume each platform client has
already scrubbed its own response text.
