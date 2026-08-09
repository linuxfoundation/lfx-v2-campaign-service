# 2026-08-07 — LFXV2-3038: the missing-actor warning counts attempts, and now says so

**Update** — `attributedActor`'s warning message now reads "write attempted with no
authenticated actor; attribution will be recorded as NULL if it commits", and its doc comment
states that the signal counts attempts rather than commits, with the reasoning.

**Fix** — Review raised that the warning is emitted before the repository call, so a write that
then fails on a version conflict, a missing parent, or a database error still logs it — while the
message asserted flatly that attribution "will be recorded as NULL", which is untrue when no row
commits at all. The message was the defect; the placement is correct and stays.

Whether a request carried an actor is settled at the gateway, upstream of anything the repository
does. A failed write is therefore evidence about the auth path in exactly the same way a successful
one is, and moving the log after a successful commit — the tightening this finding invites — would
go silent during a deploy that broke auth AND writes together, which is precisely when the signal
is wanted. The correct denominator for alerting is total write attempts, not commits, and that is
now written down where an operator will find it.

`TestBriefActor_MissingActorWarnsEvenWhenTheWriteFails` pins the choice, because the existing test
would not: its write succeeds, so it stays green under either placement. Revert-verified by gating
the warning to committed writes — the new test fails naming the alert-blinding consequence.
