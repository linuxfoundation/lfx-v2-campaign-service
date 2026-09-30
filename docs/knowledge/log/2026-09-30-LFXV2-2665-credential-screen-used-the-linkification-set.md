# 2026-09-30 — LFXV2-2665: the credential screen was reading the linkification set

**Fix** — from the round-20 review of PR #231. The reviewer's stated shape turned out to be
already refused; probing it found a narrower one that was not. Both are recorded, because the
difference is the whole lesson.

## What was reported, and what was actually true

`general` (conf 96) reported that `rejectCredentialQueryParamsInText` iterates
`findTweetURLRuns`, which drops any run preceded by an ASCII letter or digit, so
`ahttps://example.test/?access_token=…` is published unscreened.

Probed directly before changing anything — that exact input **was already refused**. The
scheme-ful run stayed visible to the scheme-less pass (the mask only blanked *bounded* runs), and
`events.example` inside it starts after `//`, a bounded position, so `schemelessScreenRunRe`
caught the authority as a second line of defence.

That rescue depends on the host containing a DOT. Removing that assumption found the real gap:

| input | refused before |
| --- | --- |
| `ahttps://example.test/?access_token=SEC` | yes (scheme-less rescue) |
| `Register herehttps://events.example/r?access_token=SEC` | yes (scheme-less rescue) |
| `foohttps://intranet/r?access_token=SEC` | **no** |
| `foohttps://localhost:8080/r?access_token=SEC` | **no** |
| `herehttps://sup3r-s3cret/?access_token=SEC` | **no** |

A dotless host gives `schemelessScreenRunRe` nothing to match on, so nothing rescued it. The last
row is the `https://sup3r-s3cret/` shape `credentials-and-untrusted-text.md` names outright: a
well-formed absolute URL whose entire content is the token, in the host.

## The root cause

The same one as the whitespace-`tweetId` bypass two commits earlier: **a gate reusing a helper
whose contract answers a different question.**

`findTweetURLRuns` answers *"what will X wrap in a t.co link"*. That is correct for
`weightedTweetLen` and `textCarriesURL`. The screen asks *"what bytes are we about to PUBLISH"* —
and an unlinkified credential is published just the same, visible in the copy, in X Ads Manager,
and to everyone the ad reaches. The comment above the screen asserted the two sets were the same
by construction ("a run X treats as a link is a run X publishes as a link"), which is true about
linkification and irrelevant to exposure.

The divergence is one shape, and it is a shape real copy has: a missing space after a word.

## The fix

`findScreenURLRuns` returns every `tweetURLRe` match with no boundary rule; the screen reads it.
`findTweetURLRuns` keeps the boundary rule and keeps its two linkification callers. Both read the
one regexp, so the difference between them stays exactly the boundary rule, in one place.

`schemefulRunMask` now masks the same unbounded set. The two have to move together: mask less than
the screen covers and the scheme-less pass re-reports an authority already checked; mask more and
a run nothing screened is hidden from the pass that would have caught it. The old bounded mask put
it in the first category, which is where the incidental dotted-host rescue came from.

Widening the screen is safe in the only direction that matters — it widens what is CHECKED, so the
sole new outcome is a refusal, never a publication. That is the trade `urlRunStartIsBounded`'s own
comment already accepts for `caféhttps://…`.

## Coverage

`TestCredentialScreenCoversGluedURLRuns` keeps both kinds of row. The dotless ones fail if the
screen is ever re-narrowed; the dotted ones fail if the scheme-less pass breaks as well. It also
pins two negatives, since a screen that refuses everything would pass every positive row.

Revert-verified: pointing the screen back at `findTweetURLRuns` fails the dotless rows, the
digit-prefixed row and the missing-space row.

Gates: `make check-fmt`, `golangci-lint run`, `go test -race ./...`, and
`go run ./cmd/okfvalidate ./docs/knowledge`.

## Note on the method

Two rounds running, the finding as written was not quite the defect. Round 19's path finding was a
rule misapplied and was declined; this one was a real defect reported through an input that does
not reproduce it. Both needed the claim run against the code before either fixing or declining —
conceding a reported shape without testing it would have produced a fix aimed at the wrong
condition, and declining on the reported shape alone would have left the dotless host open.

## Also raised in round 20, not acted on

- **Fail-open when no `as_user_id` is declared** (6th time) — by construction; seeding the shared
  system row is the operator task that closes it.
- **`text`/`as_user_id` as query parameters** (8th) — X Ads v12 create endpoints take query
  parameters.
- **Host masking in the redactors** (`repo_learnings`, 14th) — scheme+host is the chosen position.
- **Migration `000034` retention** (2nd) — true, and it is the repo-wide soft-delete pattern that
  applies equally to `account_id` and Meta's `app_id`; a retention ticket, not a defect of this
  range.
- **The real email in intermediate `11445c3e`, and X user IDs in errors in intermediate
  `95a4471`** — both need a history rewrite plus force-push; the maintainer's call, together.

`repo_code` returned **no findings** this round — the first clean repo-rules pass on this branch,
after round 19's path finding was answered in `internal-dispatch.md` rather than in code.

Refs: LFXV2-2665
