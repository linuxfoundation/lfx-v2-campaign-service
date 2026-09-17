# 2026-09-16 — LFXV2-2770: the typed-nil discovery crash and the preview-count bounds

**Fix** — Two operational fixes made during this PR's review cycles
([[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]]) that were never given their own log
entry. Recorded here so the knowledge record reflects the shipped behaviour rather than only the
review history.

- **A typed-nil LLM client crashed discovery.** `container.newLLMClient()` returns a nil
  `*llm.Client` when the model is not configured, and assigning that concrete nil pointer to the
  explorer's interface field produced a NON-nil interface — so the `if x.llm == nil` guard in
  `enrichIdentity` read false and the call panicked on a portal that was working correctly. The
  constructor now normalises a typed nil to a true nil interface — for the page fetcher and
  parser as well as the model, since all three are optional and share the failure mode — so the
  optional-model path
  degrades as designed: no brand token, fewer brand-scoped suppression probes, and nothing else
  lost. The guard and the normalisation are one fix — the guard alone was never sufficient.

- **`preview-count` gained explicit list and deadline bounds.** The union sweep is two sequential
  HubSpot round-trips per list id, so an unbounded selection is a request that cannot finish: the
  list count is capped (a fan-out budget, not a form limit) and the whole sweep runs under a
  25-second deadline (`previewCountTimeout`). Above `AUDIENCE_UNION_EXACT_CAP` the sweep is skipped entirely and the summed list
  sizes are returned as an inexact UPPER bound — see
  [[2026-09-16-LFXV2-2770-fourth-review-cycle-hardens-the-untrusted-boundaries]] for the doc
  corrections that followed, since three documents described this as a `25,000+` floor, which
  inverts the guarantee.

Both are covered by tests in `internal/dispatch`; the typed-nil one pins that a nil concrete
client leaves the interface nil, because that is the assertion the original guard was missing.
