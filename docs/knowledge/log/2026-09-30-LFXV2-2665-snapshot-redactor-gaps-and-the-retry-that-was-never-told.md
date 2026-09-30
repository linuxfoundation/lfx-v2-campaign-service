# 2026-09-30 — LFXV2-2665: two redactor gaps one character wide, and a retry nobody was told how to make

**Fix** — three items from the PR #231 review. Two are snapshot-redactor gaps in
`internal/dispatch/creds.go`; the third is an operator-facing message that named a fact and
withheld the instruction that made the fact useful.

## 1. `http:/reset/SECRET` — a leak one slash wide

`sanitizeSnapshotURL` reduces an http(s) value to scheme+host and fails CLOSED on anything
that announces the scheme and will not reduce, precisely so a malformed link cannot fall
into the truncating fallback and keep its path. The check that decided "announces the
scheme" required `http://`.

One slash short of that, everything lined up wrong at once. `http:/reset/SECRET` parses with
an EMPTY host, so the reduction declined it; the scheme test declined it; and the truncating
branch found no `?`, `#` or `@` to truncate at and returned it whole into the UNENCRYPTED
`config_snapshot`. The opaque form `http:reset/SECRET` behaves identically. Free text was
worse: `snapshotURLRunRe` also required the `//`, so inside a `tweetText` the run was not
matched at all.

The scheme alone now decides it, on both the field helper and the run pattern. Nothing
legitimate is given up — every caller supplies a URL, so an http-announcing value that will
not reduce is malformed input — and the pattern cannot swallow prose, because something has
to follow the colon with no space between. A sentence ending a clause with `http:` is not a
candidate.

## 2. The password-reset link with nothing to fire on

Four shapes reach the snapshot: scheme-ful, scheme-less with a query or fragment,
scheme-less `user:password@host`, and — the one nothing matched — scheme-less with the
secret IN THE PATH and no query, fragment or userinfo at all. `events.example/reset/SECRET`.
One pass needs a scheme, one needs a `?` or `#`, one needs a userinfo colon; none of them
fires on that, and `sanitizeSnapshotURL`'s own doc comment names a password-reset link as
the realistic shape it exists for.

A fourth pass covers it. It REDUCES to the host rather than blanking, which is where it
parts company with the userinfo pass: there is no userinfo to split wrongly, the run begins
at the host by construction, and blanking would make the same link redact differently
depending on whether the operator typed `https://` in front of it. The field helper applies
the same reduction through the same helper, so a field and the same link inside `tweetText`
cannot drift.

It runs LAST, which is now the userinfo pass's old seat, and for the same reason: it is the
least discriminating of the four, so it must only ever see what the others had nothing to
say about. An `@` still standing when it runs belongs to something an earlier pass kept on
purpose — a clock, a colon-less email — and a matched run carrying one is returned
untouched. Go's `regexp` has no lookbehind, so consuming the `@` prefix is the only way to
recognise that shape in order to leave it alone.

### The divergence, recorded on both patterns

This is the one point where the redactor deliberately reaches further than
`internal/platform/twitter`'s pre-publication screen, and the note on each pattern says so.
The two are kept in step on what a link LOOKS like — host production, letter-initial TLD,
punycode and IPv4 forms — and not on what to do about one, because the cost directions are
opposite. This pattern's only discriminator is a slash. Over-matching a slash on the
redactor's side loses a fragment of the operator's own copy from a snapshot no one diagnoses
anything with; over-matching it on the publication side refuses a brief X would have
accepted, and no retry fixes a refusal. The screen also has nothing to READ in a path-only
run, so mirroring the pattern would buy refusals and no new detection.

## 3. The abort message named the tweet and not the retry

`authoredTweetStatus` said ` / authored tweet 123 PUBLISHED, not yet promoted`. True, and
half the sentence. Authoring in Step 4 is unconditional — the code says so in its own
comment, with the guard deferred to the LFXV2-2665 idempotency work — so the operator's next
move decides between two very different outcomes: pass that id back as `tweetId` and
authoring is skipped and the live tweet is promoted; retry without it and a SECOND tweet
goes out under the LF handle.

The message now says both. The evidence that the missing half mattered is that PR #231's own
description read the returned id as meaning "a retry reuses the tweet rather than posting a
second one" — which the code has never done. The code and the concept file were both
accurate; only the prose around them inferred the instruction that was not there. It is
there now.

## Coverage

Each fix was verified by reverting it alone from a scratchpad copy:

- Restoring the `http://` requirement on `isHTTPScheme` and the run pattern fails
  `TestSanitizeSnapshot_MalformedHTTPAuthority`, printing `http:/reset/SECRET` back
  verbatim as the stored value — the leak in the failure message.
- Dropping the fourth pass fails `TestSanitizeSnapshot_SchemelessPathOnlyLink` on every
  path-only row and on the field row, which is what pins the two paths together.
- Reverting `authoredTweetStatus` to the id-only text fails
  `TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID` on the two new
  assertions while the id assertion beside them keeps passing — the split that says the
  regression would be in the instruction, not the id.

The path-only pass's first draft did regress one deliberately-pinned row:
`session 9:30@main.stage/agenda` reduced to `main.stage`, undoing the clock exemption
`sanitizeUserinfoSnapshotRun` makes one pass earlier. That is what the `@` rule above exists
for, and the existing round-14 row caught it.

Gates: `make check-fmt`, `golangci-lint run`, `go test -race ./...`, and
`go run ./cmd/okfvalidate ./docs/knowledge`. `make check-fmt` is in that set now because it
was NOT in it when the previous commit was pushed, and PR #231's `Build and Test` check has
been red on a stray blank line ever since. `golangci-lint` does not enable `gofmt` in this
repo, so a clean lint run says nothing about formatting.

Concept files updated: `internal-dispatch.md` (the scheme-alone rule and the fourth pass)
and `internal-platform-twitter.md` (the retry instruction, and the screen's side of the
divergence note).

Refs: LFXV2-2665
