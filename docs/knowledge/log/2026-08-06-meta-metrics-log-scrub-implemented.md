# 2026-08-06 — the claimed metrics log scrub actually lands

**Update** — Both bot reviewers independently caught that the 2026-08-05 fragment claimed a
`safeErrSummary` helper had been added to `internal/service/brief.go`, and that no such helper
existed anywhere in the tree. The claim was wrong: `GetCampaignMetrics`'s two failure-path logs
were still writing `merr` verbatim. Two bots reporting the same thing is strong signal, and here
they were simply right — the doc described work that was never done.

`safeErrSummary` now exists and both call sites use it. It replaces every non-graphic rune with
U+FFFD and caps the result at `errSummaryMaxRunes` (200) plus a trailing ellipsis. The cap counts
runes rather than bytes so a multi-byte upstream body truncates at a boundary instead of splitting
a rune into replacement characters.

**Update** — Scoped to the log call, not to Meta's `*APIError.Error()`. That method renders the
Graph API's `Message` field verbatim, and the non-Graph fallback fills that field from the raw
response body (`internal/platform/meta/client.go:589-612`). Changing it is out of scope here: the
raw-body behavior is pre-existing, deliberately tested, and relied on for campaign-creation
diagnostics. The metrics log site is the right place because it is platform-agnostic — every
`ReadMetrics` implementation funnels through it, so it cannot assume each platform client has
already scrubbed its own response text.

**Update** — Corrected the 2026-08-05 fragment's description of the threat while implementing it.
It claimed a control-character-laced error "could forge extra log lines". That is handler-dependent,
not universal: slog's `TextHandler` and `JSONHandler` both quote attribute values, so a raw newline
does not split a record under either of the handlers this service actually uses. The guarantee that
no handler provides is a BOUND on the value — a multi-kilobyte body reaches the sink whole.

This distinction changed the test. The first version of
`TestGetCampaignMetrics_PlatformErrorIsScrubbedBeforeLogging` asserted that no raw newline reached
the sink; it passed with the fix reverted, because `TextHandler` was closing that vector on its own.
Rewritten to assert what `safeErrSummary` alone guarantees — the record stays bounded, and the
control character is normalized — it now fails on revert with `the unbounded upstream body reached
the log sink: record is 5243 bytes`. `TestSafeErrSummary` covers the helper directly, including the
rune-versus-byte cap.
