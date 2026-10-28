# 2026-10-28 — review knowledge base: replace drifted line-number anchors with symbol anchors

**Docs** — the review knowledge base under `docs/reviews/knowledge-base/` cited code and
docs as `path:line`. Checked against `main` at 9e86e4aa, more than 40 of those anchors had drifted:
the cited line now holds unrelated text. One example is
`internal/service/orchestrator.go:88`, cited for `isReusableCampaign`, which is now a map
entry. The function itself is at line 103. Issue #425 lists every drifted anchor with
where its target lives now.

Each anchor is now a symbol anchor:

- Go: the function, type or field, with its file. Examples: `safeCause` in
  `internal/platform/hubspot/client.go`, `CampaignResult.Steps`, and the two inline
  orphan checks in `dispatchPlatform`.
- Markdown: the section heading, as `§ "Request layer"`.
- Makefile: the target or variable.
- YAML: the key.
- SQL: the object name, as in the `uq_*_connections_project` indexes.

Anchors that were still accurate were converted too, so they do not drift next. Two line
anchors stay as written: `docs/api-catalog.md:272` and `CLAUDE.md:17-26`. Both are inside
quoted reviewer comments, and the KB README forbids editing a path inside quoted text.

The KB README's Maintenance section now asks for symbol anchors, so new entries don't
bring the problem back. No pattern's detect condition, severity or false-positive floor
changed.

Re-reading each cited location turned up two sentences that no longer matched the code.
Both are corrected:

- The Meta raw-body fallback in `credentials-and-untrusted-text.md` used to be quoted
  as `truncate(strings.TrimSpace(string(raw)), 300)`. The client now calls
  `redactSecrets` before it truncates. The entry says so with a dated note, rather than
  dropping the old quote.
- The meta and reddit `CreateCampaign` doc comments do not *open with*
  `PARTIAL-RESULT CONTRACT:`. They carry it as a later paragraph.
