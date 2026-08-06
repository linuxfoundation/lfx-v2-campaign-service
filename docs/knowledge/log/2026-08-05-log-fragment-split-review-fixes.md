# 2026-08-05 — log/dispatch split: review fixes

**Update** — Local-review follow-up on the `docs/knowledge/log.md` /
`internal-dispatch.md` split. `internal-dispatch.md` still enumerated each
platform's toggle-cascade shape (reddit/meta/linkedin CASCADE, twitter scoped,
googleads PAUSE-only, microsoft unwired) after the split — exactly the
per-platform-PR hot spot the split was meant to remove — so that summary was
replaced with a link to each platform's own "Dispatch adapter" section.
`internal-platform-microsoft.md`'s moved dispatch-adapter prose also
misstated the `AlreadyExisted` contract, implying campaign-level reuse alone
sets it; corrected to match the existing "Scope" section (true only when the
campaign, ad group, AND ad all pre-existed). Also carries the archive
fragment's `# Log` → dated-H1 rename, written in the original split but never
staged.
