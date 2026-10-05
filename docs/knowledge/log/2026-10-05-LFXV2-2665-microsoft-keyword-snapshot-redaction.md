# 2026-10-05 — Microsoft config_snapshot redacts links inside keywords

**Fix** — Follow-up to the Microsoft `config_snapshot` scrub (#256). That change kept
`keywords[].text` verbatim to match Google's policy, but a keyword is caller text that reaches
the UNENCRYPTED snapshot: `validateKeywords` only trims, length-checks and validates the match
type, so `https://example.test/reset/SECRET?token=VALUE` is a valid keyword and was stored as
written — against the knowledge-base rule that caller URLs are redacted before
`config_snapshot` (copilot review on #256).

Every keyword now goes through the full `sanitizeSnapshotText`, the same redactor as `timeZone`.
A first cut of this fix used a keyword-specific variant without the path-only pass, to keep
targeting terms like `k8s.io/docs tutorial` intact; pre-PR review showed it let a token in the
PATH through (`example.org/reset/SECRET`, `www.example.org/reset/SECRET`, `host:8443/reset/S`,
`10.0.0.5/reset/SECRET`) — the shape the path-only pass exists to close, and "it kept the path"
is itself a finding under that rule. So no exemption is made: the snapshot is a redacted
record, a path-like keyword is stored reduced (`k8s.io/docs tutorial` → `k8s.io tutorial`), and
Microsoft still receives every keyword exactly as written. `microsoftSnapshotConfig` reallocates
the keyword slice before rewriting it. Pinned by `TestMicrosoftSnapshotConfig_KeywordLinksRedacted`
(nine secret-bearing shapes; all leak without the fix).
