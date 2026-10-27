# 2026-10-27 — give each half of the brief-lock argument exactly one home

**Docs** — the brief row lock that `ReplaceCampaign`, `CreateAsset` and `DeleteAsset` all take was
argued in full in six places. The largest copy sat above the lock in `ReplaceCampaign`: **86 lines
of comment before the first line of code**, restating the lock-order proof, the cost paragraph and
the whole predicate rationale that `lockBriefRowByIDQuery`'s own comment already carried. No claim
changed and no statement changed. Refs #392.

**What moved where.** Each half of the argument now has one home, and the others point at it:

- **Why no cycle is reachable** — the two named claims, (1) the brief lock is the opening statement
  of the replace's transaction so it holds no `campaigns` row lock while waiting, and (2) nothing
  anywhere acquires a slot or per-campaign lock while already holding the brief's — lives above
  `lockBriefForAssetWriteQuery`, which already owned lock ORDER for all three sites.
- **Why the predicate is `brief_id` alone, and what the caller's scan adds** — including the TWO
  legs the unreachability of `pgx.ErrNoRows` rests on — lives above `lockBriefRowByIDQuery`.
- **Why the lock is needed and what it costs** — the sibling contention against
  `ToggleCampaignStatus`'s 5 s `persistResultTimeout` — lives in the
  `internal-infrastructure-postgres` concept under `ReplaceCampaign`, where most of it already was.
- **The call site** keeps only what is local to it: that the lock is the opening statement, that it
  is taken unconditionally rather than only when `config_snapshot` changed (the statement cannot
  tell — it writes the column either way), and that the error is checked while `briefStatus` is
  deliberately never consulted.

**The trap, and the order the work had to run in.** Two claims that read like duplicates were not
duplicated anywhere — they existed ONLY in the call-site copy being cut:

- the race narrative, that a replace committing just after the delete's reference check re-publishes
  a snapshot naming an asset whose bytes are already gone while one committing just before it is
  correctly seen;
- the design trade, that a diverged status is recoverable by reconciliation and an asset whose bytes
  are gone while a snapshot still names it is not.

Both were grep-confirmed single-copy and moved into the concept doc **before** any cut was made.
**The general rule: before deleting a sentence from one of N copies, confirm it is actually in one
of the other N−1.** A consolidation that assumes redundancy is a deletion wearing a tidier name.

**One wording drift fixed on the way.** `lockBriefForAssetWriteQuery` said "nothing acquires a SLOT
lock while holding this one", while the call site and the concept both said "slot OR PER-CAMPAIGN
lock". The narrow wording was the stale one — the per-campaign advisory lock is the one a replace's
caller may actually hold — so the three now agree, and the claim is stated once rather than
restated at each site where it could go stale independently.

**Not a behaviour change.** No SQL, no control flow and no exported surface moved; `git diff`
touches comments and the concept doc only.
