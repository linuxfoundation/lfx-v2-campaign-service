# 2026-08-20 — which FAULT to report is a provenance question too

**Fix** — extends the resolution rule in
[2026-08-20-existing-campaign-follows-its-creation-account](2026-08-20-existing-campaign-follows-its-creation-account.md)
from "which connection do we resolve" to "which failure do we report". That fragment's rule is
intact and unchanged; this records the arm it did not reach.

## The same defect, a fourth time, one layer down

This PR has now produced one shape of defect four times: a fix guards one arm and leaves its
sibling. Pre-cutover → post-cutover. Success-path → error-path. Paid-ads → HubSpot. And now:
which CONNECTION to resolve → which ERROR to return.

Each round fixed the named lines. The class kept reappearing because the fix was aimed at the
finding rather than at the family of exit paths the finding belonged to.

## The invariant was applied to resolution and dropped at the return

Resolution correctly keyed on the recorded creation account. But two arms return an ERROR
rather than a resolution, and both decided which error by asking *which resolution happened to
succeed* — an inference, not the recorded fact:

- the project resolves a DIFFERENT account and the system row is broken → returned the
  project's resolution.
- both scopes fail → returned the project's error.

Both readings are right for a project-created campaign and wrong for a system-created one. A
campaign created while forcing was on records the SYSTEM account; if that row later breaks, the
project's resolution comes back and the adapter's provenance guard renders:

```
409 the campaign belongs to a different ad account than this project's current connection —
    reconnect the original account to change its status
```

The project does not own the LF row, cannot see it, and cannot repair it. The operator who can
is never paged, and the campaign keeps spending. The `ErrSystemConnectionMissing` /
`ErrSystemConnectionNotUsable` classification this PR added was defeated at the last step.

## The real tension: both cases must hold at once

The existing comments defended returning the project's error, and that defence is CORRECT — for
a project-created campaign. A project that merely re-pointed or disconnected its own connection
must not page the platform operator. An earlier mutation on this PR specifically caught
substituting the system error unconditionally.

So this is not a bug to invert. Both remedies are right for their own case, and the discriminator
is the recorded creation account — the same fact resolution already keys on. Inverting would have
traded one wrong page for another, and the inverse mutation below proves the fix does not.

## Provenance survives the row being unusable; a resolution does not

`resolveForcedSystem` cannot answer "did the system account create this campaign" at the moment
the answer matters. It LOADS, VALIDATES and DECRYPTS, and returns an error INSTEAD of a value —
so precisely when the system row is broken, its account id is unavailable and the caller is left
inferring provenance from which resolution worked. That inference IS the defect.

`systemCreated` reads the system row's `AccountID` **column** directly. A missing credential
blob or a rotated key does not change which account created the campaign, and the column is
readable through both. The general lesson: when a decision needs one FIELD, do not obtain it
through a pipeline that can fail for reasons unrelated to that field.

It answers `known=false` for an unsettleable question — no recorded provenance, a system row
absent or nameless, a repo failure — and callers keep the project-owned default. The asymmetry is
deliberate: claiming system creation pages an operator, so an unproven claim must never be guessed
in that direction. An unanswered "was this disconnected?" is not a no, and an unanswered "did the
system create this?" is not a yes.

## Enumerating the exit paths first is what ended the loop

Rather than patch the two cited lines, every exit of `resolveExisting` was enumerated and each
classified against the invariant. Seven exits, two arms wrong, and the second was not in the
review — it would have been the fifth round.

Doing this by grep is how a sibling gets missed; the enumeration has to be of PATHS, not of
matches.

## Mutation results, including the inverse

Six compiling mutations. The inverse mutation matters most: returning the system error
unconditionally is killed by three tests, proving the fix DISCRIMINATES rather than flips. A fix
that merely inverted the old bias would have passed every test written for the reported finding.

Two mutations survived the first pass, and both were real routing defects rather than test
cosmetics — a system row present but recording NO account, and a repo error not disqualifying
the answer. Both made an unprovable provenance answer "system-created", paging the wrong
operator. `TestUnprovableProvenanceKeepsTheProjectFault` kills both.

The repo-error survivor needed a FIXTURE change, not just a test: `scopedConnReader.Get` returned
`nil, err`, so the `conn == nil` check caught the mutation and the `err != nil` check was never
load-bearing. The fake now returns a row alongside the error (`errRows`), because a repository
may hand back a partially populated value with a failure. **A mutation can survive because the
fixture cannot express the state that distinguishes it** — the test was not weak, the fake was.

## Assert the remedy, not the error value

The unit tests assert sentinels, but the defect was never about an error value — it was about
which of two people is told to fix something. A fix returning the right sentinel to the wrong
caller would satisfy them. `TestSystemCreatedCampaignPauseReportsTheOperatorFault` drives the
REAL Meta dispatcher end to end and asserts the fault an operator actually receives, because the
adapter layer is what turned the previous round's correct error into a project-owned 409.

A service-layer test on `classifyBriefMetricsErr` alone was written and then DELETED: it passed
before and after the fix. It pinned the classifier's contract, which was never broken. A test
that cannot fail on the defect is not evidence, however well it reads.
