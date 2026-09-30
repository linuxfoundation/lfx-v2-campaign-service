# 2026-09-30 — LFXV2-2665: URL-path redaction is a per-consumer call, not a blanket rule

**Docs** — no code change. The round-19 review of PR #231 raised two findings about secrets in a
URL PATH. Both were verified against the code and both are being accepted rather than fixed; this
entry records why, so the next round does not re-raise them as open.

## What was raised

1. `repo_code`, Critical — `displayTwitterUtmURL` (`internal/platform/twitter/client.go`) strips
   userinfo, fragment and the brief's pre-existing query but KEEPS `u.Path`, and the new
   tweet-authoring degrade path reaches the persisted Steps entry that uses it. A registration URL
   shaped `https://events.example/reset/SECRET` therefore reaches the unencrypted
   `campaigns.result`. It quotes `internal-dispatch.md`'s own snapshot paragraph as the rule
   broken.
2. `general`, conf 94 — `rejectCredentialQueryParams` screens query KEYS only, so the same
   path-borne secret reaches published tweet text.

## Why neither is fixed

**The quoted rule's own two-part test gives the opposite answer here.** `sanitizeSnapshotURL` drops
the path because `config_snapshot`'s only reader is a human reconstructing what a campaign was
configured with, and the path is not load-bearing for that. `displayTwitterUtmURL`'s Steps entry is
the destination link an operator PASTES INTO a tweet they post by hand. Reduced to scheme+host it
points at the site root rather than the registration page, and the operator either rebuilds it from
the brief or ships the wrong URL in a real ad. Same rule, different consumer, opposite call — the
test has to be re-run per consumer rather than generalized from the snapshot column, and
`internal-dispatch.md` now says so in place.

`displayTwitterUtmURL` is also PRE-EXISTING at `ceca731b`; this branch newly routes into it, which
is what surfaced it, but the helper and its path handling are not a regression from this work.

**The publication gate cannot screen a path the way it screens a query.** The query gate works
because it matches a bounded list of key names. A path segment offers no equivalent: nothing
separates `/reset/abc123` from `/blog/kubecon-recap` except a heuristic that either catches nothing
or fails a create over an ordinary deep link. Refusing to guess is the deliberate position, not an
oversight.

The residual exposure is real and is accepted on that basis — a brief whose registration URL hides
a secret in its path — not dismissed as impossible.

## Also raised and not acted on

- **`as_user_id` retained on soft-deleted connections** (`general`, conf 92) — true, and it is the
  repo-wide soft-delete pattern, applying equally to `account_id` and Meta's `app_id`. A retention
  ticket, not a defect this range introduced.
- **The fail-open branch, `text`/`as_user_id` as query parameters, host masking in the redactors,
  the real email address in intermediate commit `11445c3e`** — all declined previously, on
  unchanged grounds.
- **Replay authoring a second tweet** — known; the returned `AuthoredTweetID` is the recovery
  handle, and a real idempotency guard is owed by separate work.

Round 19 confirmed the whitespace-`tweetId` bypass
(`2026-09-30-LFXV2-2665-whitespace-tweetid-bypassed-the-identity-check.md`) is closed: no reviewer
repeats it, and its two regression tests stand.

Refs: LFXV2-2665
