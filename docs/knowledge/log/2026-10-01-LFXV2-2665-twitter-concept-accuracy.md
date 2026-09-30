# 2026-10-01 — LFXV2-2665: two stale claims in the twitter concept file

**Docs** — no code change. Round 21 of the PR #231 review returned no critical or important
findings and no knowledge-base matches; `repo_code` raised two documentation-accuracy items, both
verified against the code and both correct.

## 1. The scanner was documented with a `\b` it does not carry

The overview paragraph gave `tweetURLRe` as `(?i)\bhttps?://[^\s<>。、！？，：；]+`. The actual
expression has no `\b`, and the same file explains two sections later exactly why: Go's `\b` is
defined over `\w`, which includes `_`, so `_https://…` had no boundary and the whole run went
unseen. The file therefore contradicted both itself and the code.

Corrected to the boundary-free expression, with a pointer to where the boundary actually lives:
`findTweetURLRuns` applies it in code for linkification, and the credential screen
(`findScreenURLRuns`) deliberately does not apply it at all.

## 2. The dispatch summary predated both of this round's fixes

It said the adapter "maps `tweetText`/`asUserId` straight into `CampaignInput` alongside
`tweetId`". True when written; false since. `tweetId` is now trimmed once in dispatch so the
authoring gate and the client's emptiness test cannot disagree, and `AsUserID` is not the caller's
value at all — it is what `authorizedTwitterAsUserID` returns, with the connection's declared
identity winning, a caller value required to match it, and a caller sending none inheriting it.

Worth noting as a pattern rather than a one-off: this is the SUMMARY section going stale while the
detailed section was updated in step. A concept file that describes the same thing at two depths
has two places to fix, and the shallow one is the easier to forget — it is also the one a reader
consults first.

## Round 21 otherwise

- `repo_learnings` — no findings. First clean pass; host masking was not raised, after thirteen
  prior rounds.
- `repo_code` — no critical or important findings.
- `general` — four items, all repeats on unchanged grounds: the fail-open branch on an unpinned
  connection (7th time, by construction), `as_user_id`/`text` as query parameters (9th), and the
  two intermediate-commit privacy items in `11445c3e` (a real corporate email in a test fixture)
  and `95a4471` (promotable-user IDs interpolated into errors). Both of those last two are history
  rather than code — no review round can close them, and only a rewrite plus force-push can.

Refs: LFXV2-2665
