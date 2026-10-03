# 2026-10-02 — LFXV2-2770 check 4: the wire contract had no test

**Fix** — Review response on PR #240. Adds the service-layer test for check 4's
client-observable contract, the `[minor]` half of dealako's review.

**Why the absence mattered more than the test does.** The `[blocking]` finding on the same
review was that `CheckCurrentRegistrants` returned a NEEDS VERIFY verdict for every caller
supplying no event name, while the godoc on `QaChecks.CurrentRegistrants`, the design
attribute ("omitted … when it could not run") and the PR description all promised the field
would be OMITTED. Since every client today sends no `event_name` — the UI is not wired — that
would have flipped every previously-PASSing audit to NEEDS VERIFY and trained operators to
ignore the verdict.

That contradiction survived because nothing asserted what a client receives. The check's own
unit tests proved the verdict it returns; the dispatch test passed `""` without asserting on
`Checks.CurrentRegistrants` or `Overall`. The `""` arm in `CombineVerdicts` and the nil arm in
`currentRegistrantsResult` were both reachable only from unit tests, never from `RunQA` — dead
in production and alive in the suite, which is the shape that makes a false contract look
tested.

**What is pinned now.** `TestRunAudienceQaOmitsCheckFourWhenNoEventWasNamed` asserts the field
is absent AND that `Overall` is unchanged — that equality is the "no existing caller's verdict
changes" promise, stated as an assertion rather than as prose.
`TestRunAudienceQaCarriesCheckFourWhenTheEventIsNamed` covers the other half, since an
omitted-only test passes for a check that never runs at all.
`TestRunAudienceQaForwardsTheEventNameItWasGiven` closes the gap between them: a service that
dropped the name would satisfy the first test for the wrong reason, because a dropped name and
an absent one are indistinguishable in the response.

Mutation-verified at both layers. Removing `currentRegistrantsResult`'s nil guard fails the
omitted test; restoring the original `Check{Verdict: VerdictNeedsVerify}` on an empty event
name fails the dispatch test. Neither passes vacuously.
