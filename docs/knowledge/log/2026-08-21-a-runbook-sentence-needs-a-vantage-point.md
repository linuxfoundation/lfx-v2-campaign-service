# 2026-08-21 — a runbook sentence needs a vantage point, not just a true claim

**Fix** — the review follow-up to
[2026-08-21-absent-is-not-unprovable](2026-08-21-absent-is-not-unprovable.md). That fragment's
rules are intact and unchanged; this records a defect **introduced by** its third fix.

## The fix for a missing claim created a premature one

That fragment fixed an authoring-failure runbook whose definite arms said only "author the post
and add the ad manually", dropping the fact that a campaign and ad group already existed. An
operator following it literally builds a SECOND campaign beside the PAUSED one.

The fix wrote the context into the Step 1.5 arms: *"campaign + ad group created (PAUSED)"*.
That claim is **not true where it was written**. Step 1.5 authors the post; the campaign POST is
Step 2 and the ad group is Step 3, both below it. The sentence becomes true only when both
creates go on to succeed.

When the campaign create returns an ambiguous partial, the persisted `Steps` read:

```
Promoted-post authoring failed: Reddit rejected the post (HTTP 400)
    -- campaign + ad group created (PAUSED) without an ad; add the ad to this campaign
Campaign creation is UNCONFIRMED ... a PAUSED campaign MAY exist -- verify by name
```

Two adjacent steps, one asserting a campaign exists and the next saying it might. The operator
is told to attach an ad to a campaign that may never have been created.

This is the **same defect class** the original fix targeted — a runbook asserting something the
code does not guarantee — reached from the opposite direction. Removing a false absence
introduced a false presence.

## Both failure modes are the same question asked at the wrong place

A runbook line has two requirements, and the first round satisfied only one of each in turn:

- the claim must be **needed** (the missing-context version failed this);
- the claim must be **known at the point it is emitted** (the premature version failed this).

Reverting to the earlier wording trades the second failure for the first. The resolution is to
keep the sentence and move it: Step 1.5's arms now describe only the POST that actually
happened ("continuing without an ad"), and the campaign-state sentence — naming the real
`campaignID` and `adGroupID` — is appended at **Step 4**, guarded by `postWarning != ""`.

**Reaching Step 4 is the proof.** Every path between the campaign POST and Step 4 returns
early: the campaign create returns on an ambiguous partial, on a malformed 2xx and on a definite
failure; the ad-group create does the same, including its own malformed-2xx arm. So control
arrives only when both POSTs succeeded AND both ids decoded non-empty, and `CreateCampaign`
PAUSES both. The vantage point, not the wording, is what makes the sentence safe.

The generalisation: **emit a runbook sentence from a point where its claim is already
established, not from where it is convenient to write.** A string appended early in a function
is a claim about the future, and every later early-return is a way for that claim to be wrong.

## Verification had to cover the paths, not the finding

The reviewer named the ambiguous partial. Testing only that would have missed the malformed-2xx
campaign create, which reaches the same contradiction through a different return, and would not
have proved the deferred sentence still appears where it IS true. The test asserts all three
outcomes after an authoring failure: absent on ambiguous, absent on malformed-2xx, present
**exactly once and naming both ids** on success.

The mutations separate the two halves cleanly, which is the evidence they are independently
pinned:

- moving the claim back into a Step 1.5 arm → all three subtests fail;
- deleting the deferred Step 4 block → only the success subtest fails;
- firing the deferred block unconditionally (dropping `postWarning != ""`) → killed by a
  PRE-EXISTING test, `TestCreateCampaign_NoPostURLHasNoAdWarning`, confirming the clean-run
  contract (no ad requested → empty `AdWarning` → clean `created`) is untouched;
- dropping the `AdWarning` suffix → only the success subtest fails.

No survivors. Also checked, because the previous round's assertions were written by the same
hand that wrote the premature strings: **no test pinned the old wording.** The prior test
asserts `UNCONFIRMED` / `DELETE` / `Promoted-post authoring` tokens, all of which survive the
move — had it asserted the campaign-state phrase, it would have locked the defect in.
