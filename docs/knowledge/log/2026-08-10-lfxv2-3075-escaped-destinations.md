# 2026-08-10 — LFXV2-3075 okfvalidate rejects CommonMark-escaped link destinations

**Defect** — The `okfvalidate` bullet-format check excludes whitespace and angle
brackets from link destinations, but allowed backslash (`\`) and ampersand (`&`),
which are CommonMark escape sequences: `thing\.md` renders as `thing.md`,
`thing&#46;md` renders to the same. The validator's naive suffix check
`strings.HasSuffix(target, ".md")` does not decode these escapes, so both cases
passed the format regex, then silently failed downstream when `filepath.Join` and
`os.ReadFile` looked for a file with the literal spelling (e.g., `thing\.md`)
that does not exist — a working link that silently opted out of the
description-sync invariant with no diagnostic.

**Fix** — The regex now excludes backslash and ampersand: `[^)\s<>&\\]+`.
Both escaped forms are rejected at the format stage with the existing "must be a
bare path" diagnostic, same as angle brackets and titles. A bullet using either
spelling will now report an error instead of silently skipping the
description-sync check.

**Regression Guard** — `TestValidateIndexBulletRejectsEscapedDestinations` covers
both `thing\.md` and `thing&#46;md`, verified via mutation: without the exclusion,
the test fails (both cases silently skip); with the exclusion, it passes (both
cases are rejected with the format diagnostic).

**Bundle Check** — The repo's entire knowledge bundle contains no bullets with
`&` or `\` in their destinations (verified via grep), so this change carries no
impact on existing documentation.
