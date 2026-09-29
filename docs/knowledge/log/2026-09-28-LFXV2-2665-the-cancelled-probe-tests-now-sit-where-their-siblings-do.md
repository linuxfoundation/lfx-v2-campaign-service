# 2026-09-28 — LFXV2-2665: the cancelled-probe tests now sit where their siblings do

**Note** — self-audit before a further review pass. No production code changed, no assertion
changed.

`googleads`, `microsoft` and `reddit` each pin the pre-cancelled probe in a file named
`probe_precancelled_test.go`, holding a test named `TestProbeCancelledBeforeAnyRequestIsNotSent`.
When `meta` and `twitter` gained the same guarantee they landed in each package's general
`probe_test.go` under a different name, so the five copies of one guarantee could no longer be
found as one family — and the family is the point: `ProbeNotSent` is a per-package predicate, and
what makes the behaviour reviewable is that every package answers the same question the same way.

Both packages now have `probe_precancelled_test.go`, and the moved test carries the sibling name.
`TestProbeNotSentIsEntryTimeOnly` moved with it and kept its own name: it has no sibling in the
other three packages, because only `meta` and `twitter` mark the entry of a REQUEST path, where a
mid-flight expiry can reach the same function that the entry check guards. It is the negative half
of the same file's subject, so it belongs beside it rather than back in `probe_test.go`.

Naming is the whole content of this entry and that is deliberate. A guarantee that five packages
hold is only as good as a reader's ability to enumerate the five, and the two that drifted were
the two added last — the ordinary way a convention erodes.
