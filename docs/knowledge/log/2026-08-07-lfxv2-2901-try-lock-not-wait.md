# 2026-08-07 — LFXV2-2901: the campaign claim is a try-lock, and the docs said otherwise

**Update** — `ClaimCampaignVersion`'s prose, `internal-service.md`,
`internal-infrastructure-postgres.md` and the `toggleCampaignRepo` test fake now all describe
the claim the way it is implemented: `pg_try_advisory_lock`. A losing writer is refused
immediately with `domain.ErrCampaignWriteInProgress` → a retryable **409**. It does not queue.

**Fix** — Five pieces of documentation described a WAITING lock ("a second caller blocks here",
"delete waits for the toggle, then observes the bumped version and returns an actionable 412").
`campaign_repo.go:546` and `:1000` are both `SELECT pg_try_advisory_lock($1)`, so the code was
right and the prose was wrong. The POOL COST note had also been reasoning about waiters, which is
what makes the try-lock the right choice in the first place: a waiter pins a service-wide pool
connection for the length of the holder's platform call — up to 45s, plus a 30s UNCONFIRMED
cooldown. With a try-lock the bound on pool consumption is the number of campaigns being written
concurrently, not the number of in-flight requests.

**Fix** — The `toggleCampaignRepo` fake modelled the INVERSE of production: `ClaimCampaignVersion`
blocked on `claimMu.Lock()`. That is not a cosmetic mismatch, it changes what every test using the
fake asserts. A blocking loser parks, resumes after release, re-reads a version the holder has
bumped, and reports **412**; the real service refuses it before it looks at the row and reports
**409**. Retrying is the right response to exactly one of those — 412 says "your ETag is stale,
refetch first", 409 says "your request is fine, send it again" — and only the second is true of a
caller that arrived while someone else held the lock. Worse, tests written against the blocking
fake keep passing if the production try-lock is changed back into a wait, which is precisely the
regression that matters.

The fake now uses `TryLock`, and the three tests built on it assert the real contract:

- `ConcurrentTogglesSerialize` — the loser gets a `*briefs.ConflictError`.
- `UnconfirmedHoldsTheClaimForTheCooldown` — the second toggle is refused inline and never
  reaches the platform; after the cooldown elapses a FRESH toggle succeeds. Nothing was queued on
  the refused caller's behalf, so nothing resumes by itself.
- `UpdateCampaignBlocksDuringAToggle` → `UpdateCampaignIsRefusedDuringAToggle`. "Blocks" was the
  wrong verb. It now checks the refusal, then the stale-ETag 412 the retry legitimately earns once
  the toggle has bumped the version, then the same retry succeeding at the current version — the
  claim excludes a writer for one platform call, not permanently.

Reverting the 409 mapping in `mapBriefErr` fails all three with the intended diagnostic.
