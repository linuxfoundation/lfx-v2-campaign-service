# 2026-08-31 — LFXV2-2775: a named arm returned the wrapper

**Fix** — `buildLogError` was default-DENY in structure and still leaked, because its named arms
returned the caller's error rather than the sentinel they matched.

`errors.Is` matches through the whole chain. So
`fmt.Errorf("decode hubspot credentials: bad value %q: %w", token, context.Canceled)` satisfied
the `errors.Is(err, context.Canceled)` arm, and the arm returned `err` — the outer text, token
included. The default-deny fallback below it never ran. A credential decode failure cancelled by
its context takes exactly this path, which is the one case the redactor exists to guard.

Each context/package sentinel arm now returns the bare sentinel. HubSpot API errors still return
their own text: they render as method/path/status, never quote a response body, and unlike a
sentinel they name the call that failed.

The existing `TestBuildLogError_DefaultDeny` passed throughout, because it tested a BARE
`context.DeadlineExceeded`. Bare sentinels have no wrapper to leak. The test now wraps each
sentinel around a token and asserts both that the class survives and that the wrapper does not;
reverting the fix fails it on all three sentinels with the token visible.

**The general shape:** a default-deny fallback is only as good as the arms above it. An arm that
matches through a chain must return what it MATCHED, not what it was given — and a test that
feeds it an unwrapped value cannot tell the difference.

Related: `docs/knowledge/log/2026-08-31-LFXV2-2775-a-fix-that-inverted-its-own-bug.md`.
