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
The entity form is caught instead by `hasEntityRef`, which matches an HTML character
reference — `&amp;`, `&#46;`, `&#x2e;` — and reports its own diagnostic: "has an HTML
entity in its link destination path".

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

## Follow-up: the entity guard rejected every literal ampersand

**Kind:** Fix

The guard above was `strings.ContainsRune(destinationPath(link), '&')`, which asks "is
there an ampersand?" and calls the answer "is this an entity?". Copilot found the gap in
a suppressed comment: `&` is a legal PATH character, so a concept file genuinely named
`research&development.md`, linked correctly and bare, was refused with a diagnostic about
an entity it does not contain.

Over-rejection is a defect of the same guard, and it is the more tempting mistake because
it reads as conservative. It is not. This guard exists to stop a link SILENTLY opting out
of description-sync; refusing a link that would have PASSED sync serves nothing and costs
a real filename.

The discriminator is the closing `;`, which an entity has and a bare ampersand does not:
`&(#[0-9]+|#[xX][0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]*);`. It runs against the RAW destination
rather than `destinationPath`, because a numeric reference hides behind the same `#` that
truncation uses — `thing&#46;md` reaches the old check as `thing&`.

Scoping it to numeric references alone would have been the mirror error. A NAMED reference
fails silently too, by the other route: nothing truncates `thing&period;md`, so it reaches
the ".md" suffix test whole and misses it. Both forms are rejected.

**Regression Guard** — `TestValidateIndexBulletAcceptsALiteralAmpersandInThePath` accepts
`research&development.md` and still reports drift against it, proving the path is resolved
rather than merely tolerated. Verified via revert: with the old one-rune check restored it
fails with the exact false diagnostic. `TestValidateIndexBulletRejectsEscapedDestinations`
gains `thing&period;md` and `thing&#x2e;md` alongside the decimal case.
