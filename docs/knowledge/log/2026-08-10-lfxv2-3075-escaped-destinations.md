# 2026-08-10 — LFXV2-3075 okfvalidate rejects CommonMark-escaped link destinations

**Fix** — The `okfvalidate` bullet-format check excludes whitespace and angle
brackets from link destinations, but allowed backslash (`\`) and the entity form
of the same escape: `thing\.md` renders as `thing.md`, and so does `thing&#46;md`.
The validator decodes neither, so both were a working link that quietly opted out
of the description-sync invariant — each for its own reason, which is why they
needed two different guards:

- `thing\.md` passes the format regex and the naive `strings.HasSuffix(target,
  ".md")` test intact, then fails downstream at `filepath.Join`/`os.ReadFile`,
  which find no file spelled with the backslash.
- `thing&#46;md` never reaches a file lookup at all. `checkBulletDescription`
  strips the anchor and query before resolving the path, and the entity's literal
  `#` reads as the start of a fragment — so the target is truncated to `thing&`,
  which fails the `.md` suffix test and is dropped without a word.

The destination character class now excludes backslash: `[^)\s<>\\]+`.
It deliberately does NOT exclude `&`. A legitimate destination carries one in a
multi-parameter query (`thing.md?v=1&lang=en`), which `checkBulletDescription`
strips before resolving the path — so banning every `&` in the class would reject,
at the format stage, a destination the resolver goes out of its way to support.
The entity form is caught instead by `hasEntityInPath`, which looks for `&` only
in the path component, where it cannot be a query separator, and reports its own
diagnostic: "has an HTML entity in its link destination path".

**Regression Guard** — `TestValidateIndexBulletRejectsEscapedDestinations` covers
`thing\.md` and `thing&#46;md`, asserting the distinct diagnostic each guard
produces, and `TestValidateIndexBulletAcceptsAMultiParameterQuery` pins the
behaviour the narrower class preserves: `thing.md?v=1&lang=en` is accepted, and
still reports description drift, proving the query is stripped rather than merely
tolerated. Verified via mutation: dropping either guard fails its own case, and
restoring `&` to the character class fails the multi-parameter query case.

**Bundle Check** — The repo's entire knowledge bundle contains no bullets with
`\` or an entity in their destinations (verified via grep), so this change carries
no impact on existing documentation.
