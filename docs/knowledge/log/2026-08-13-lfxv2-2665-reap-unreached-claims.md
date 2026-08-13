# 2026-08-13 — reaping only the claims that never reached the provider

**Update** — `internal/infrastructure/postgres/campaign_repo.go`, `internal/container/container.go`
(LFXV2-2665). The periodic sweep now DELETES stranded dispatch claims that provably never
reached the provider, instead of only reporting them. Claims that might have created something
upstream are still reported and never touched.

## Why the obvious fix is a money bug

The ticket asks for stuck-claim recovery, and the obvious implementation — delete `'pending'`
rows older than a threshold — would eventually **authorize a duplicate paid campaign create**.

`'pending'` is overloaded, and `stuckClaimReportAge` already said so in a comment nobody had
acted on: it marks a claim merely in flight AND an ambiguous dispatch outcome, which the
orchestrator persists as `'pending'` precisely because the provider MAY already have created a
paid campaign. No status distinguishes them. Reaping on age alone deletes the second kind, the
next dispatch wins the freed unique index, and the platform gets a second campaign for the same
brief.

That is the exact failure the claim exists to prevent, which is why the original change shipped
report-only and said "safe automatic recovery needs provider idempotency keys or an
authoritative reconcile first".

## The subset that IS safe, and why

Status cannot distinguish them, but two other columns can.

`claimCampaignDispatchQuery` writes `campaign_name ''` and `status 'pending'` and **nothing
else** — neither `platform_campaign_id` nor `result`. Every path that touches the provider
populates at least one:

- a created upstream id lands in `platform_campaign_id`;
- the ambiguous-create / group-orphan case persists a reconcile blob in `result` with the id
  deliberately empty (`orchestrator.go` documents both shapes as the "retained partial orphan"
  it refuses to skip).

So a row with **both still empty** is one where the dispatcher died before any provider call
completed. Nothing exists upstream to duplicate, and nothing will ever revisit it. Those are
safe to delete; everything else stays for a human.

This is narrower than the ticket's title ("lifecycle ownership, recovery, upstream reconcile").
It closes the part that does not need idempotency keys — the crash-stranded claim that
permanently blocks a `(brief, platform)` pair — and leaves reconciliation where it was.

## Verified against a real database, and that mattered twice

The safety argument is a SQL predicate, so a fake repository would only have proved that Go
calls Go. Both tests run against live Postgres via the `TEST_DATABASE_URL` harness.

The first run failed on `invalid input syntax for type uuid: "job-reap"` — `job_id` is a UUID
and the fixture passed text. A mocked repo would have accepted it happily.

The second was the revert check. Collapsing the predicate to `status = 'pending'` — the naive
age-only reaper — fails with:

```
reaped 5 rows, want exactly 1
a claim carrying a platform_campaign_id was reaped; a retry can now duplicate a real paid campaign
a claim carrying a reconcile result was reaped; the provider may already hold that campaign
```

That is the money bug, reproduced on demand. The test exists to keep it reproducible.

## Ordering inside the sweep

Reap runs BEFORE the scan, so the scan reports what actually remains for a human rather than
listing rows the same tick is about to remove.

A young claim is left alone by the same `stuckClaimReportAge` threshold the report uses, and the
reason is not tidiness: reaping an in-flight claim would delete it out from under a running
dispatcher mid-provider-call, and the next caller would win the freed index and create a second
campaign. The age threshold is load-bearing on this path in a way it was not when it only gated
a log line.
