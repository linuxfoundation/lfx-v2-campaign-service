# 2026-09-30 — LFXV2-2665: an authorization check with the wrong scope, and a length bound that was not mirrored

**Fix** — two items from the round-17 review of PR #231, both on `as_user_id`, the X
publishing-identity control that the previous entry
(`2026-09-30-LFXV2-2665-as-user-id-never-reached-storage.md`) made reach storage at all. Having
made it work, this pair corrects WHERE it applies and WHAT it accepts.

## 1. The check ran on a path that never reads the value

`authorizedTwitterAsUserID` was called unconditionally in `internal/dispatch/twitter.go`'s
`Dispatch`. The client reads `in.AsUserID` in exactly one place — inside
`tweetID == "" && composedTweetText != ""` — because an explicit `tweetId` wins over `tweetText`:
the supplied tweet is promoted as-is, no tweet is authored, and no promotable user is resolved.

So a request carrying `tweetId` plus a stale or mismatched `asUserId` was refused pre-create over
a field nothing downstream would have read. `docs/api-catalog.md` already said the field is "only
meaningful with `tweetText`", and `tweetId` "takes precedence"; the code was the half that
disagreed. The client itself had already settled the principle for the neighbouring field — an
unused, possibly-malformed `tweetText` must not fail an otherwise-valid campaign — and this is the
same rule applied to the identity that would have signed it.

The gate is a COPY of the client's condition, `TweetID == "" && TrimSpace(TweetText) != ""`, not a
restatement of it. The two have to stay identical: drift one way and dispatch authorizes text the
client does not publish; drift the other and the client publishes text dispatch never authorized.

Narrowing an authorization check is the edit most able to go too far quietly — remove it entirely
and most suites still pass, because tests overwhelmingly assert the ALLOWED path. Two things guard
that here:

- `TestTwitter_MismatchedAsUserIDIsPreCreate` still pins the refusal, and now provokes it through
  `tweetText`. It used to send `tweetId`, which no longer reaches the check — a test that would
  have passed forever on a gate that never opened.
- `TestTwitter_AsUserIDIsIgnoredWhenTweetIDWins` asserts the OUTCOME of the ignored case rather
  than the absence of an error: the supplied tweet is still promoted, the authoring endpoint is
  never POSTed, and the promotable-users read never happens.
  `TestTwitter_UnusedAsUserIDIsIgnoredWithNoDeclaredIdentity` covers the same gate on a connection
  declaring no identity, where the old and new behaviour differ only in what is resolved.

## 2. `maxValueLen` was a shared ceiling standing in for a per-key rule

`internal/bootstrap/sysacct.go`'s `requireShapes` held every value to one `maxValueLen = 64`.
`as_user_id` is `MaxLength(32)` at `design/connection.go`, so a 33–64 digit value passed bootstrap
while the HTTP contract refused it. `valueShapes` mirrored the design's `Pattern` and silently
dropped its length — half the rule.

This is the precise case `internal-bootstrap.md` already warns about in the abstract: the
installer writes past Goa straight to the repository, so bootstrap is the one door such a value
can come through, and `as_user_id` is seeded ONLY by bootstrap. It would have installed ACTIVE on
the shared LF fallback row — which every project without its own X connection dispatches through,
and which has no second opinion downstream — surfacing as a tweet-authoring failure far from the
operator who typed it.

`maxValueLens` now overrides the default per provider/key, read through `maxLenFor`.
`TestTwitterAsUserIDIsHeldToTheDesignLength` asserts both sides of the bound (32 accepted, 33 and
64 refused), so a later tightening cannot pass a test that only checks 33.
`TestMaxValueLensOverridesAreLive` fails an override naming a key `valueShapes` does not carry, or
one that does not actually tighten the default — a bound that is never looked up reads as enforced
and is not.

## Coverage

Both fixes revert-verified. Removing the gate fails
`TestTwitter_AsUserIDIsIgnoredWhenTweetIDWins` on the refusal it must no longer produce; restoring
the shared `maxValueLen` fails `TestTwitterAsUserIDIsHeldToTheDesignLength` on the 33- and
64-digit rows.

Gates: `make check-fmt`, `golangci-lint run`, `go test -race ./...`, and
`go run ./cmd/okfvalidate ./docs/knowledge`.

Concept files updated: `internal-dispatch.md` (the gate, and why it is a copy of the client's
condition) and `internal-bootstrap.md` (per-key length overrides). `docs/api-catalog.md` gained
the scope sentence beside the `as_user_id` connection paragraph, so the contract states the
narrowing rather than leaving it inferred from "only meaningful with `tweetText`".

## Not fixed, and why

- **The fail-open branch when no `as_user_id` is declared** (round 17, Critical) — by
  construction, and recorded as such in `internal-dispatch.md`: an unpinned connection behaves
  exactly as it did before the feature, and seeding the shared system row is the operator task
  that closes it. Failing closed would refuse tweet authoring for every existing connection on
  deploy.
- **`text` and `as_user_id` as query parameters** — declined in rounds 4, 6, 7 and again here: X
  Ads v12 create endpoints take their parameters as query parameters.
- **Host masking in the redactors** — declined again; reducing to scheme+host is the chosen
  position.
- **The real corporate email address in the round-14 log entry** — a real finding, left alone here
  for two reasons: one file per log entry means never editing another entry's file, and the
  address is also in an intermediate commit, so the full remedy needs a history rewrite and a
  force-push. Both halves are the maintainer's call, together.
- **The emoji-length validator's twitter-text divergence** — pre-existing, outside this range, and
  a behaviour change to a validator rather than a fix to this work.

Refs: LFXV2-2665
