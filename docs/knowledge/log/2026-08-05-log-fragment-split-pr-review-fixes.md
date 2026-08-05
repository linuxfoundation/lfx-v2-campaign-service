# 2026-08-05 — log/dispatch split: PR review fixes

**Update** — Addressed three Copilot findings on PR #80 for the
`docs/knowledge/log.md` / `internal-dispatch.md` split. `validateLogFragment`
checked only the digit shape of a fragment's date, so a filename like
`2026-99-99-ticket.md` with a matching heading passed; it now parses the date
with `time.Parse("2006-01-02", ...)` and rejects impossible calendar dates.
`cmd/okfgen`'s root index generator only emitted Architecture/Kubernetes/
Code/Specs, so re-running it would silently drop the checked-in Log entry and
its OKF-deviation note; it now emits the Log bullet and appends the deviation
note. That note also overstated what regenerating a conformant `log.md` takes
("by concatenation") when the fragment and legacy `log.md` heading formats
differ; reworded to "by normalizing each fragment's H1 into an `## YYYY-MM-DD`
heading" in both `docs/knowledge/index.md` and the generator's copy.
