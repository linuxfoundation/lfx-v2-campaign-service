# 2026-08-05 — log/dispatch split: second review fix

**Update** — Second local-review pass on the `docs/knowledge/log.md` /
`internal-dispatch.md` split flagged that the status-toggle section still said
"every adapter's `ToggleStatus` below follows this contract" after the
per-platform descriptions moved out — the content below is now only a list of
links, and microsoft/hubspot don't implement `StatusToggler` at all. Reworded
to scope the sentence to adapters that implement the interface and point at
the linked platform concepts for detail.
