# 2026-09-30 — LFXV2-2665: the fix that refused the customer's own copy

**Fix** — the fourteenth `/lfx-skills:lfx-local-review` cycle ran against `11445c3e`. Three
items are fixed; three are judgment calls surfaced to the maintainer rather than decided
here; one is a standing decline at its ninth raise.

The most important item in this round is **not** a reviewer finding. It came out of a
probe written to check the reviewer's userinfo-grammar claim, and it is a regression the
round-13 commit introduced.

## 1. A clock is the userinfo production byte for byte

Round 13 added `schemelessUserinfoRunRe` and recorded that "the colon is the entire
discriminator". It is not.

Measured, on `11445c3e`:

```
"session 9:30@main.stage tomorrow"   -> REFUSED
"keynote 14:00@events.example"       -> REFUSED
```

Both are ordinary copy for an events platform, and both are `user:password@host.tld`
exactly. This is a **refusal** path, so the cost is the expensive direction the twitter
file's own asymmetry rule exists to avoid: the brief is blocked before anything is
created, and no retry and no error message explains it.

Round 13's negative rows did not catch it because every one of them put punctuation
between the clock and the host — `"doors 9:30, ask bob@events.example"` passes on the
comma, not on the rule. Hard against the host is the shape real copy has.

`userinfoRunIsClockShaped` now skips a run whose colon carries ASCII digits on BOTH sides.
Both sides is the deliberate choice: the narrower "numeric username is not a credential"
spelling was the first thing written and it gives up `9:hunter2@events.example` for
nothing. A digits-only password behind a digits-only username is a shape nothing in this
service produces, and the scheme-ful screen still catches it the moment the operator
writes the `https://`.

The same test is mirrored into `sanitizeUserinfoSnapshotRun` on the dispatch side. The two
patterns are documented as kept in step, and a discriminator on only one of them puts the
screen and the redactor back out of agreement — the exact defect the third pass was added
to fix. The cost direction there is milder (a lost line of diagnostic copy, not a blocked
create) but not nothing, and the test gives up no credential shape to buy it.

## 2. U+FE0E ended up inside the emoji cluster it denies

`emojiClusterLen` absorbed U+FE0E alongside U+FE0F. U+FE0E requests **TEXT** presentation
— it is the codepoint that says *do not render the one before me as an emoji* — so a
sequence carrying it is not an emoji sequence. No RGI sequence contains it and
twitter-text's generated data has none, which makes ending the cluster before it a
statement of what twitter-text does rather than a guess about it: `U+1F5A5 U+FE0E` weighs
4 there, and absorbing the selector charged 2.

This is the narrow half of round 13's clustering decline, and it separates cleanly from
it: **the fix needs none of the generated sequence table.** The broader question — whether
to import that table so a non-emoji supplementary base followed by U+FE0F stops being
charged 2 where twitter-text charges 4 — stays declined on the same cost-direction
reasoning round 13 gave.

## 3. A real corporate domain in a test fixture

`hello@lf.org` in the round-13 negative rows is now `user-1@example.com`. The fixture
needs an address-shaped string, not a live mailbox.

## Surfaced, not decided

Three items are real and are the maintainer's call, not a fix commit's:

- **`cfg.AsUserID` is passed to the client without an authorization check**
  (`internal/dispatch/twitter.go:188-192`). `resolvePromotableUser` proves the ID is
  promotable by the shared ad account; it does not prove this project may publish under
  that handle. On the shared LF system connection that is a plausible confused-deputy
  path. It is an authorization design question and likely its own ticket.
- **`promoted_tweets` passes `idempotent: true`**, so `doRequest` retries it on 429,
  against two written rules. The provisional read is that X dedupes the
  (line_item, tweet) pair, so the cost is a spurious manual-verification warning rather
  than a double-spend — but it touches paid-ads mutation and is not settled here.
- **Widening the userinfo grammar** to empty usernames (`//:pw@host`) and bracketed IPv6,
  as the review asks. Item 1 is the argument against: this pattern has now produced one
  live false refusal, and both widenings add match surface in exactly that direction for
  shapes that are contrived rather than observed.

## Declined

- **Mask the host in the redactors.** Ninth raise, three framings, unchanged answer. The
  knowledge-base rule is a two-part test — reproduce a component only when it is BOTH
  structurally incapable of holding a secret AND load-bearing for the diagnosis. "Which
  site did this link point at" is the whole of what a redacted URL still tells the human
  reading a snapshot.

## Coverage

Each fix was verified by reverting it alone from a scratchpad copy:

- Dropping the clock skip from `findSchemelessScreenRuns` fails
  `TestRejectCredentialQueryParamsInText_ClockAgainstHost` on all four negative rows,
  printing the refusal that would have blocked the brief. Restoring U+FE0E to the absorb
  case fails `TestWeightedRunLen_TextPresentationSelector` at 2 against a want of 4.
- Restoring `ReplaceAllString(out, "")` on the snapshot pass fails
  `TestSanitizeSnapshotText_SchemelessUserinfo` on both new clock rows.

Both tests pin the positive side as well as the negative: `9:PLAINTEXT@events.example` and
`ops9:PLAINTEXT@events.example/portal` are still refused, and `see 9:SECRET@events.example
now` is still blanked from the snapshot, so the narrowing did not buy its false-positive
fix with a credential shape.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (the clock
discriminator and U+FE0E) and `docs/knowledge/code/internal-dispatch.md` (the mirrored
test and why the cost directions differ).

Refs: LFXV2-2665
