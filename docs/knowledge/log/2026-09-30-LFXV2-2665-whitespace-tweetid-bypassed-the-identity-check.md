# 2026-09-30 — LFXV2-2665: a whitespace-only `tweetId` bypassed the publishing-identity check

**Fix** — a regression introduced by the previous entry's own fix
(`2026-09-30-LFXV2-2665-as-user-id-scope-and-bound.md`), caught by the round-18 review of PR #231
and reported independently by two reviewers.

## What broke

That entry narrowed the `as_user_id` authorization check to requests that actually author a tweet,
gating it in `internal/dispatch/twitter.go` on:

```go
if cfg.TweetID == "" && strings.TrimSpace(cfg.TweetText) != "" {
```

The client does not test the raw value. `internal/platform/twitter/client.go` runs
`in.TweetID = strings.TrimSpace(in.TweetID)` BEFORE the emptiness test the gate was copied from.
So `"tweetId": "   "` was non-empty at the gate and empty at the client: the authorization check
was skipped, and the client then took the authoring path anyway — auto-resolving a promotable user
while the connection's declared identity was neither inherited nor enforced. On an account with a
single promotable user the client selects it silently, even when the connection pins a different
one.

Whitespace is the cheapest possible input on a caller-controlled field, and the gate decides
WHETHER AN AUTHORIZATION CHECK RUNS — so it has to agree with the client's condition on every
input, not merely on the inputs a test happens to pass.

## Correcting the previous entry

One file per log entry means that entry is not edited, so the correction is recorded here. It
states:

> The gate is a COPY of the client's condition, `TweetID == "" && TrimSpace(TweetText) != ""`, not
> a restatement of it.

**That claim was false when written.** It was a restatement, and it differed from the client by
exactly one `TrimSpace`. The same claim was made in a code comment and in
`docs/knowledge/code/internal-dispatch.md`; both have been corrected in place. Everything else in
that entry — the scope narrowing itself, and the `maxValueLens` per-key bound — stands.

## The fix

Normalize ONCE in dispatch and hand the same value on:

```go
tweetID := strings.TrimSpace(cfg.TweetID)
var asUserID string
if tweetID == "" && strings.TrimSpace(cfg.TweetText) != "" {
```

with `TweetID: tweetID` in the `twitter.CampaignInput` literal, so the client's own trim is
idempotent and the two conditions cannot disagree about emptiness. Restating `TrimSpace` at the
gate would have fixed this input and moved the next drift one edit further out; computing the
value once and passing it over removes the second opinion entirely.

**The general rule:** when a gate is described as "mirroring" a condition in another package, the
mirror is only as good as the normalization on both sides. The safe form is to compute the value
once and hand it over, not to write the same expression twice.

## Coverage

Two regression tests in `internal/dispatch/twitter_test.go`, pinning both directions — a fix aimed
only at the refusal would have left the other half broken:

- `TestTwitter_WhitespaceTweetIDDoesNotBypassTheIdentityCheck` — connection pins `222`, request
  sends `tweetId: "   "`, `tweetText`, and a mismatched `asUserId`; asserts a pre-create refusal
  and zero upstream requests.
- `TestTwitter_WhitespaceTweetIDInheritsTheDeclaredIdentity` — connection pins `222`, request
  sends `tweetId: " "` and `tweetText` with no `asUserId`; asserts the authored tweet carries
  `as_user_id=222`. Its fake `promotable_users` returns TWO candidates deliberately: with the pin
  carried through the client uses `222`; with the pin dropped the client refuses to guess between
  two and authors nothing — so the assertion fails whichever way the identity is lost, not only
  when it lands on the wrong handle.

Both revert-verified: restoring `cfg.TweetID` at the gate fails them on the refusal that arrives
too late and on an authored query with no `as_user_id`.

`docs/api-catalog.md`'s scope sentence now states that both halves of the condition are judged
after trimming — the reviewer quoted that paragraph as the rule the code violated, so the contract
had to say which values the narrowing covers.

Gates: `make check-fmt`, `golangci-lint run`, `go test -race ./...`, and
`go run ./cmd/okfvalidate ./docs/knowledge`.

## Not fixed, and why

- **Unicode/IDNA scheme-less hosts** (round 18, conf 90) — the credential screen and the snapshot
  redactor recognize ASCII and punycode scheme-less hosts only. Real, but a separate surface from
  this range and a behaviour change to the screener; recommended as a ticket rather than a fix on
  this branch.
- **`text` and `as_user_id` as query parameters** — declined again; X Ads v12 create endpoints take
  their parameters as query parameters.
- **Host masking in the redactors** — declined again; scheme+host is the chosen position.
- **The fail-open branch when no `as_user_id` is declared** — by construction; seeding the shared
  system row is the operator task that closes it.

Refs: LFXV2-2665
