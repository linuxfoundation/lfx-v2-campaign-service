# 2026-08-06 — LFXV2-2993 Meta log scrub: correct the 2026-08-05 fragment's threat model

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
