# 2026-08-10 — LFXV2-3075 index bullets must match the concept's description

**Update** — `okfvalidate` now requires every index bullet's description to be
verbatim the linked concept's frontmatter `description`, and the twelve bullets
that had already drifted are back in sync.

The two texts are written at different moments. A concept file is edited by the
PR that changes the behaviour it documents; its index bullet is edited by
whoever remembers step 2 of the CLAUDE.md checklist. Nothing was checking the
pair, so they drifted in both directions — sometimes the bullet was the current
text and the frontmatter stale (`internal/platform/googleads`, `meta`,
`hubspot`, `microsoft`, `eventurl`), sometimes the reverse (`grype` in the
MegaLinter doc, `SNOWFLAKE_*` in the config and Deployment docs).

That asymmetry is why the fix could not be mechanical in one direction. Each of
the twelve was resolved by checking the claim against the code and keeping the
true side: the Meta bullet's "status toggle cascade over ad set and ads" is
real (`updateAdSetAndAds`), the HubSpot bullet's fail-closed statistics guards
are real, `eventurl` really does refuse redirects and bound the body — so those
descriptions were promoted into the frontmatter. Where the frontmatter was the
newer text, the bullet was rewritten from it.

The index is the surface a reader consults FIRST to decide which concept file
is worth opening, which makes a stale bullet worse than a stale concept: it
does not merely say something out of date, it routes the reader away from the
file that would have corrected it.

The check compares only bullets that resolve to a readable `.md` file inside
the bundle declaring a frontmatter description — not directories, sub-indexes
(which carry no frontmatter by rule 3), external links, or broken links, whose
defect belongs to a link check rather than this one.

"Inside the bundle" turned out to be a scope the first cut described but did
not enforce. Nothing constrains a relative link, so `../../notes.md` resolved
to a real file outside the bundle, was read, and had its frontmatter
description quoted back in the validation error — a file the checker was never
pointed at, partly disclosed by it. Containment is now checked against the
bundle root with symlinks resolved on both sides, because a lexical
`..`-scan is defeated by a symlink inside the bundle pointing out of it, and
because temp-dir roots are themselves symlinked (macOS `/var/folders`), so
resolving only one side reports every target as an escape.

The comparison is also raw rather than trimmed. Trimming was the one tolerance
the check claimed not to have: a frontmatter description of `" Summary. "`
satisfied a `Summary.` bullet, while the diagnostic printed both sides trimmed
and so could show two identical strings as a mismatch. The index line is
stripped of LEADING whitespace only, so padding on one side alone is visible
drift and the fix is to unpad whichever side carries it — usually the
frontmatter, since markdownlint already forbids a bullet from ending in
space. Padding that genuinely matches on both sides is not drift and is not
what this check is for.
