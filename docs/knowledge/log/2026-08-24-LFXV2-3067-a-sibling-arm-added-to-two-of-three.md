# 2026-08-24 — the same sentinel arm added to two handlers out of three

**Fix** — settings readback: `GetCampaignMetrics` and `ToggleCampaignStatus` both gained an
`ErrSystemConnectionMissing` arm above their `ErrNotFound` arm. `GetCampaignSettings`, which
resolves its connection through the same `resolveExisting` and carries a switch with the same
shape, did not: it jumped from `ErrSystemConnectionNotUsable` straight to `ErrNotFound`.

## Why the missing arm changes the answer

`resolveExisting` reports the case as `errors.Join(ErrSystemConnectionMissing, ErrNotFound)`.
Both sentinels match, so **whichever arm comes first wins**. With no specific arm, the readback
answered:

> 404 — this project has no connection for the campaign's channel; connect it before reading
> settings

Under force-system mode the project's own connection is the thing being ignored, so that is
advice the caller cannot act on, and the operator who must install the LF system row gets no
ERROR log and no page. Two sibling endpoints answer 500 for the identical error; a third
disagreeing about who owns the repair is worse than any single answer, because the same
underlying fault now presents differently depending on which endpoint the user happened to hit.

## What made it findable, and what the test must pin

The existing table test `TestGetCampaignSettings_PermanentConnectionDefectsAreNot503` already
existed to pin this switch's ORDER and even documents that "five sentinels here are wrapped
ALONGSIDE a broader one". It had no case for this sentinel. **A table test that pins an
ordering class does not cover a sentinel absent from the table** — the table's own existence
reads as coverage and is the reason the gap survived review.

The added case asserts the STATUS CLASS (not-404, is-500) rather than the message string. The
mutation that proves it: leave the new arm in place but move it BELOW the `ErrNotFound` arm.
It compiles, the arm is still present, and the test fails — so the test pins the ORDERING the
comment claims is load-bearing, not merely the arm's existence.

**When a fix adds an arm to a switch, grep for every sibling switch resolving through the same
helper.** The three handlers here shared a resolver but not a reviewer's attention.
