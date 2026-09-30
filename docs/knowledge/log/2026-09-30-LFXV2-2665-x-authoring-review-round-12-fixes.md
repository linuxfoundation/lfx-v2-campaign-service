# 2026-09-30 — LFXV2-2665: the three gaps round 11 opened by half-fixing

**Fix** — the twelfth `/lfx-skills:lfx-local-review` cycle on the X authoring work ran
against `1b19db3b` and returned six items. Three are fixed here, two are old declines
standing for the sixth and seventh time, and one is a documented deferral being re-read as
a defect.

What is worth recording is that all three live findings are **asymmetries the round-11
commit itself introduced**. Round 11 taught the publication screen that X linkifies
scheme-less links; it did not teach the snapshot redactor, and it did not correct the
catalog sentence that described weighting as if the two scanners agreed. A fix that
teaches one side of a pair a new shape has to say what it is doing to the other side, out
loud, or the next review finds the gap.

## 1. `[a-z]{2,}` reads as "a TLD is a word", and a TLD is not

`schemelessScreenRunRe`'s final label accepted only alphabetic characters, minimum two.
Measured, before the fix: `198.51.100.7/r?access_token=PLAINTEXT` and
`events.xn--p1ai/r?access_token=PLAINTEXT` were both unscreened and would have been
published, while `events.example/r?access_token=PLAINTEXT` beside them was refused.

Neither shape is exotic. `xn--` is how EVERY internationalized TLD is spelled on the wire,
so the old pattern was not missing an edge case — it was missing the entire non-Latin web,
and a dotted-quad host besides. The label is now a dotted-quad alternative or
`[a-z][a-z0-9-]+`. Requiring the label to START with a letter is what keeps `v1.2?` and
`3.2?` out, and it does that job better than the old two-character minimum did.

Scheme-less bracketed IPv6 is deliberately left uncovered: X does not linkify it, and a
leading `[` collides with the markdown-link shape operators actually paste.

## 2. The snapshot redactor still required a scheme

`internal/platform/twitter` screens scheme-less links before authoring. `tweetText` is
ALSO persisted, and `sanitizeSnapshotText` matched only `http(s)://` — so a scheme-less
link the screen merely did not object to (its parameters not on the denylist, or the
campaign written before the screen existed) kept its full query and fragment in the
UNENCRYPTED `config_snapshot`. Fixing the publication side alone moved the exposure rather
than closing it.

`sanitizeSnapshotText` now runs a second pass with the same host grammar, reducing each
scheme-less run to its authority and adding no scheme back — the snapshot should say what
the operator wrote, and they wrote none.

Two details are the whole of why this stayed small:

- **Order replaces bookkeeping.** The scheme-ful pass runs first and strips every query and
  fragment it rewrites; the scheme-less pattern only ever matches a run that still HAS one.
  A reduced `https://a.example` is therefore invisible to the second pass with no masking
  and no byte offsets, which is what the twitter side needed and this side does not.
- **One deliberate divergence: a userinfo prefix.** The screen only READS a run, so where
  it begins costs nothing there. This one REWRITES the run, and a pattern that starts at
  the host leaves `user:pw@` behind as bare text — the password surviving beside the
  redacted token. Consuming the userinfo makes `url.Parse` see it and fails the whole run
  closed, which is the answer `sanitizeSnapshotURL` already gives.

## 3. The catalog claimed t.co weighting the counter does not do

`weightedTweetLen` weights `http(s)` runs only; the catalog said "any URL costs 23 whatever
its length". After fix 2 that sentence was wrong about a shape the service now handles in
two other places, which is exactly the state
`docs-must-not-advertise-what-the-code-rejects` exists to catch.

The code half of this finding stays declined, with round 11's reasoning unchanged: deciding
which dotted token X actually linkifies needs twitter-text's TLD registry, and every token
guessed wrong near the 280 boundary is a create refused for copy X would have accepted.
Screening a run X does not linkify costs a refusal the operator fixes by deleting a
parameter; weighting one costs a working brief. The catalog now states that split plainly
— scheme-ful URLs at the fixed 23, scheme-less links at raw length, and why.

## Declined, with reasons

- **Move `text`/`as_user_id` out of the request URL.** Sixth time. Unchanged: X's v1.1
  endpoints take these as query parameters, the transport is the client's own, and the
  remedy proposed each round is a body-POST path X's API does not offer for this call.
- **Mask the host in the snapshot redactor.** Seventh time. The knowledge-base rule is a
  two-part test — reproduce a component only when it is BOTH structurally incapable of
  holding a secret AND load-bearing for the diagnosis. "Which site did this link point at"
  is the whole of what a redacted URL still tells the human reading a snapshot; masking the
  host leaves `https://xxxxx`, which tells them nothing and closes nothing the path and
  query reduction has not already closed.

## Not a finding

**"Authoring is unconditional after campaign/line-item reuse."** This is a DOCUMENTED
deferral, not an oversight, and the rationale is already written at
`internal/platform/twitter/client.go:3285`: the guard needs a `promoted_tweets` read on the
reused line item, which belongs with the LFXV2-2665 idempotency work rather than bolted on
here. The reviewer quoting the comment that describes the behaviour is not evidence the
behaviour is unintended.

## Coverage

Each code fix was verified by reverting it alone from a scratchpad copy:

- Restoring the old `schemelessScreenRunRe` fails
  `TestRejectCredentialQueryParamsInText_IPv4AndPunycodeHosts` on all four refusal rows —
  the dotted-quad, the punycode TLD, the ported dotted-quad and the uppercase all-punycode
  host all go unscreened.
- Restoring the single-pass `sanitizeSnapshotText` fails
  `TestSanitizeSnapshotText_SchemelessLink` on seven of eight rows, each printing the
  surviving credential — including the mixed row, where the scheme-ful link reduces
  correctly and the scheme-less one beside it keeps `?token=S2` whole.

Both tests also pin the negative side: dotted prose with no query (`agenda.md`, `Node.js
v1.2`) is left untouched, and a version string with a query-looking tail (`section 3.2?`,
`2026.10?`) is not a candidate.

Concept files updated: `docs/knowledge/code/internal-platform-twitter.md` (why the host
grammar widened, and what is still not covered) and
`docs/knowledge/code/internal-dispatch.md` (the second pass, why order suffices, and the
userinfo divergence). `docs/api-catalog.md` corrected for the weighting split.

Refs: LFXV2-2665
