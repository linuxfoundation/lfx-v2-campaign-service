# 2026-08-05 — split docs/knowledge/log.md into per-entry fragments

**Update** — Replaced the single append-at-top `docs/knowledge/log.md` with a
`docs/knowledge/log/` fragment directory, one file per entry
(`YYYY-MM-DD-<slug>.md`). A `git merge-tree` replay of every merge commit
since 2026-08-02 showed `log.md` was the sole conflicting file in 17 of 28
conflicted merges — every branch appended at the same spot in the same
newest-first file. Fragments make that structurally impossible: each
branch's entry is its own file, named with its own ticket. The historical
`log.md` moved verbatim to `docs/knowledge/log/2026-07-09-archive-through-2026-08-05.md`
rather than being shredded into 139 files. Also split the per-platform
credential/config and status-toggle prose out of
`docs/knowledge/code/internal-dispatch.md` into each platform's own concept
under a `## Dispatch adapter (internal/dispatch)` heading, for the same
reason (6 of the historical conflicts were platform PRs colliding on that
file's shared per-platform lists).
