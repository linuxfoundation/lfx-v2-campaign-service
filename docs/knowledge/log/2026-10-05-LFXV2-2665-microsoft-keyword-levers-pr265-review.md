# 2026-10-05 — Microsoft keyword levers: PR #265 review fixes

**Fix** — the status toggle's live-keyword read, and two wording findings.

- **PAUSE no longer reads.** The best-effort `GetAdGroupKeywords` on the pause path could spend the
  toggle's 45s deadline (one 30s attempt plus 429 retries) before the cascade ran, so the campaign
  gate could be left un-paused. Pause now sends the recorded keyword ids with no read; a failure at
  the trailing keyword stage, after Microsoft confirmed the gate, ad group and ad Paused, is
  reported as a successful pause with a warning (`IsPausedBeforeKeywordStage`).
- **ACTIVATE's read is bounded** by its own 10s sub-budget; a failed or timed-out read is a
  definite refusal with nothing changed.
- The apply-keyword-actions description and `actions` attribute are platform-neutral (Google
  all-or-nothing, Microsoft per action, "before the ad platform is contacted").
- Merging main brought `keyword_report.go`'s identical `keywordStatusDeleted` and a
  `msKeywordDispatcher` test helper; the duplicate constant was dropped and the levers' helper
  renamed `msLeverDispatcher`.
