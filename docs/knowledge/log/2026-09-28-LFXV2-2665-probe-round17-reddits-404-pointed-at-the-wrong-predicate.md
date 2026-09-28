# 2026-09-28 — LFXV2-2665: Reddit's 404 cross-reference pointed at the wrong predicate

**Docs** — round 17 of local review. One stale cross-reference, no behaviour change.

`RedditDispatcher.ProbeConnection`'s doc said the verdict for a `404` on the account read "is
decided inside `reddit.ProbeCredentialRejected`". It is not, and has not been since
`ProbeAccountUnreachable` was split out: the dispatcher asks that predicate first, ahead of
`probeClass`, and answers `accountNotReachable`.

The sentence was not nonsense, which is why it survived. `ProbeCredentialRejected`'s doc really
does discuss the `404` — at length, explaining why it is deliberately NOT there. So a reader who
followed the pointer landed somewhere that talked about the right subject and gave the right
reasoning, and only a reader who went looking for the *decision* would notice it was in the other
file. The cross-reference had been written when there was one predicate to point at, and it kept
pointing there after the split moved the decision out.

That is the failure mode worth recording: a stale pointer whose destination still reads
plausibly is harder to catch than one that reads wrong, because nothing about arriving there
feels like an error. The split exists precisely because the two answers send an operator to
different fields — re-authorise the credential, or repoint the account id — so a pointer that
blurs them undoes the distinction it was documenting.

The doc now names `ProbeAccountUnreachable` as the decider, says it is asked before `probeClass`,
and keeps `ProbeCredentialRejected` in the sentence as what it actually is: the other half of the
split, which documents the exclusion and decides nothing here.
