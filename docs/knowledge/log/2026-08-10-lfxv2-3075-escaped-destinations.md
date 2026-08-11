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

The destination character class now excludes backslash, and both parentheses:
`[^()\s<>\\]+`. The OPENING paren is there because CommonMark admits `(` in an
unbracketed destination only as part of a balanced pair, so `[Thing](thing(foo.md)` is
not a link — accepting it made this validator the only reader that saw one.
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

**Fix** — the entity guard
asked "is there an ampersand?" and reported the answer as "is this an entity?", so a concept
file genuinely named with a bare `&` was refused for an entity it does not contain.

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

## Follow-up 2: the fix for the over-rejection over-rejected somewhere else

**Fix** — moving
the check onto the raw destination made the numeric entity visible, but dragged the query and
the fragment in with it, refusing entities in the two regions that never reach path resolution.

Cursor Bugbot, on the commit that fixed the bare-`&` over-rejection above. Moving the check off
`destinationPath` and onto the RAW destination made the numeric form visible — it hides behind
the same `#` that truncation cuts at — but dragged the query and the fragment in with it. Neither
reaches path resolution: `checkBulletDescription` strips both before looking anything up, so
`thing.md?a=1&amp;b=2` resolves exactly as `thing.md` does. Refusing it refuses nothing.

That is the same argument as the round before, applied to the region the round before moved the
check into. Worth naming as a shape rather than an incident: **a fix that widens a guard's INPUT
to catch a missed case widens what it can wrongly catch by exactly the same amount.** Both rounds
here were one guard reaching for a cheap boundary — first `destinationPath`, then the whole
string — when neither boundary is the one the question needs.

The boundary that is: `pathRegion` cuts at the first `?`, then at the first `#` that does not
itself begin a character reference. `destinationPath` is deliberately left alone; the only
destinations where the two differ are ones this validator is about to refuse, so resolution never
sees the distinction and its callers do not need it.

**Regression Guard** — `TestValidateIndexBulletAcceptsAnEntityOutsideThePath` covers an entity in
the query and one in the fragment, both with DRIFTED descriptions so the assertion is that the
link was resolved and compared rather than merely not format-rejected.
`TestValidateIndexBulletRejectsANumericEntityBeforeAFragment` pins the case that rules out the
easy boundary in the other direction: `thing&#46;md#sec` carries both a numeric reference and a
real fragment. Revert-verified both ways — matching on the raw destination fails the accept test
with the "has an HTML entity" diagnostic on a link that resolves fine; matching on
`destinationPath` fails the numeric cases with `Validate() = []`, the silent skip the whole guard
exists to prevent.

## Round: what a destination DENOTES decides scope; how it is SPELT decides workability

**Fix** — `classifyDestination` judged the RAW text, so a destination that DENOTES a site-root or
absolute URL through a character reference was classified local and refused by a guard that has
no business there.

The fifth over-rejection, and the first that was hiding a second, worse bug behind itself.

Copilot: a character reference is decoded by CommonMark inside a link destination, so
`&sol;spec.md` renders as the site-root `/spec.md` and `https&colon;//example.com/spec.md` renders
as an absolute URL. `classifyDestination` was judging the RAW text, `url.Parse` saw no scheme, no
authority and no leading slash in either, and both landed in `destLocal` — where the entity guard
refused a link that renders perfectly. The previous round's fix had made the literal spellings
(`/spec&amp;notes.md`, `%2Fspec&amp;notes.md`) skip correctly; the encoded spellings of the
external marker itself were still rejected.

This is settled by this file's own argument, not by the reviewer's. The guard is deliberately NOT
conditioned on the `.md` suffix because "an undecoded reference is exactly the case where the
suffix cannot be trusted." That reasoning does not stop at the suffix. If a reference can make
`notes&#46;txt` and `notes&#46;md` indistinguishable, it can make a scheme and a leading slash
invisible too — and the answer is to **decode before deciding**, not to trust less. Hence the
split the code now states: what a destination DENOTES decides whether it is in scope, and how it
is SPELT decides whether this validator can work with it. Classification runs on the decoded text,
the guard on the raw.

`decodeCharRefs` is built on `charRefSpans` rather than on `html.UnescapeString`, so what counts
as a reference for the guard is exactly what counts as one for classification. Go's decoder also
honours the HTML5 legacy semicolon-less forms, which CommonMark does not, and using it directly
would have rewritten the entity-SHAPED literal that `charRef` exists to protect.

**The half the review did not mention.** `checkBulletDescription` runs its own external tests on
its own copy of the destination, and those were on the raw text too. So the entity guard had been
the ONLY thing stopping `&sol;spec.md` from reaching `filepath.Join` as a bundle-relative path —
the wrong-file comparison that function's own doc comment calls the failure it is least able to
notice. Removing the rejection without decoding there as well would have converted an
over-rejection into a silent comparison against a file the bullet never named: strictly worse than
the bug being fixed, and it would have passed a review that only checked the reported symptom.

Worth naming as a shape: **a guard that wrongly rejects something may be the only thing standing
between that input and a real defect downstream. Removing the rejection is not the whole fix —
find out what the rejection was incidentally protecting.** Nothing had ever exercised the path,
because nothing had ever reached it.

**Regression Guard** — three rows added to `TestValidateIndexBulletSkipsExternalDestinations`:
`&sol;spec.md`, `&#47;spec.md` and `https&colon;//example.com/spec.md`. Each decoy is written at
the location `filepath.Join` would produce if the RAW spelling were treated as a bundle path, so
the table asserts both halves at once. Revert-verified independently: undoing the classification
decode fails all three with "has an HTML entity in its link destination path", and undoing the
`checkBulletDescription` decode fails the two with resolvable decoys with `bullet for
"&sol;spec.md" ... but the file's frontmatter description is "The decoy the bullet never named."`
— the wrong-file comparison, caught only because the decoy was placed at the trap rather than
somewhere harmless.

## Round: a separator can be spelt without being denoted, and denoted without being spelt

**Fix** — `pathRegion` looked for `#` and `?` in the raw spelling, so an entity that denotes
either one left the path region overrunning into a fragment or query the validator never resolves.

Two more over-rejections, both about the boundary between a destination's path and everything
after it, and both found by a reviewer rather than by the tests.

`pathRegion` looked for `#` and `?` in the RAW spelling. `&num;` and `&quest;` denote those
characters without spelling either, so CommonMark resolves `thing.md&num;usage` exactly like
`thing.md#usage` — the reference lives in the fragment, the path is a bare `thing.md`, and the
bullet resolves and compares with nothing decoded. Reporting a path entity there refused a link
that works. This is the entity-in-the-query round again, and it opened the moment the previous
round decided to decode for classification: deciding what a destination DENOTES raises the
question of separators that are denoted rather than spelt.

The mirror case is `&#00000046;`. CommonMark admits 1–7 decimal digits and 1–6 hexadecimal ones;
`html.UnescapeString` decodes longer runs too. Eight digits therefore stay literal in a renderer
and decoded here — so the validator saw a reference, and the renderer sees a `#` beginning a
fragment over the path `thing&`, which names no concept file. The bullet was never this
validator's business, and it was refused rather than skipped. `entityRefPattern` now carries
CommonMark's bounds. Named references need none: `charRef` confirms them against the HTML5 table,
which is finite.

One scan settles both, because they are the same question asked of each position: does a
separator begin here, by spelling or by denotation? The two directions cancel — a reference is
skipped whole where it merely SPELLS a `#`, and cuts the path where it DENOTES one.

## Round: the classifier read a region resolution throws away

**Fix** — `classifyDestination` was handed the WHOLE destination, so a `%` in a fragment made
`url.Parse` fail and the bullet was refused as "not a URL reference" over text no later step reads.

The sixth over-rejection, and the one that reached furthest past the path. `url.Parse` rejects a
malformed percent-escape anywhere in its input, fragment included:

```
"thing.md#100%"   ERR  parse "thing.md#100%": invalid URL escape "%"
"thing.md?pct=1%" ok   path="thing.md"
"thing%.md"       ERR  parse "thing%.md": invalid URL escape "%.m"
```

`thing.md#100%` is a legal CommonMark link whose path is a bare `thing.md`, and
`checkBulletDescription` strips the fragment before resolving — so the region that caused the
refusal is one it discards a line later. Classification now runs on `pathRegion(decodeCharRefs(…))`.
Nothing is lost: scheme, authority and leading slash all live before the first separator, so every
arm of the switch still sees what it needs.

The distinction the fix rests on, and the reason this is not a loosening: a `%` in the PATH really
does defeat resolution and stays rejected — `thing%.md` still reports, pinned by
`TestValidateIndexBulletRejectsAnUnparseableDestination`. The two tests have to be read as a pair.

Revert-verified, and the measurement corrected the test rather than merely confirming it: of the
three cases, only the two FRAGMENT ones fail with the old classification. The query case passes
either way, because `url.Parse` tolerates a bare `%` in a query and never refused it. It is kept
as a boundary marker — the query is the other region resolution discards — and labelled as one,
because a subtest that cannot fail looks like evidence until someone checks.

That is the sixth round of the same shape: a guard whose SCOPE was wider than the thing it guards.
Each round so far narrowed scope by one region — path vs. query, denoted vs. spelt, raw vs.
decoded — and none of the six was found by a test. They were found by reading what the next
function does with the value.
