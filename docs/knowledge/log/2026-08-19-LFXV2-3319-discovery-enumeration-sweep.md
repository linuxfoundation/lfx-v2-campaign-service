# 2026-08-19 — LFXV2-3319 "Reddit and X have no discovery" survived in five places after being corrected in four

**Fix** — a review pass over the X Ads discovery branch found the codebase contradicting
itself about whether X has a discovery endpoint, plus two struct members on
`twitter.AdAccount` that no production path read. Both classes are recorded here because
both are shapes that recur rather than one-off typos.

## The enumeration was corrected where the diff already went, and nowhere else

Adding X discovery falsified every comment phrased as "Reddit and X lack discovery". The
branch corrected four of them — the ones inside hunks it was already touching for other
reasons — and left five untouched, in files the diff *did* touch but not at those lines.
The two that mattered most:

* `internal/bootstrap/sysacct.go` had its bullet corrected from "Reddit and X lack the FIRST
  half" to "Reddit lacks the FIRST half", while the LEAD PARAGRAPH of the same docblock still
  read "Reddit and X have no discovery endpoint, so an account-less row is unrecoverable".
  One docblock, two answers, governing `accountDiscoveryProviders` — the map deciding whether
  the CLI installs an account-less LF system row.
* `design/connection.go` carried three: `:517`'s "Reddit and X still have NO list to choose
  from" described an endpoint added 500 lines below it in the same file, and gave a *reason*
  to keep `Required("account_id")` that no longer existed. `design/` is the Goa source of
  truth, so the next credentials-first ticket for X would have read a stale justification and
  stopped.

The sharp part: `sysacct.go`'s own docblock warns 65 lines further down that an enumeration
"goes stale silently — this comment described a Google/Meta-only world for two tickets after
that stopped being true." It happened again, in that file, in the PR fixing it. A comment
warning about its own failure mode does not prevent the failure mode.

**What actually finds these is a grep for the CLAIM, not a review of the diff.**
`grep -rn -i "reddit and x\b\|reddit and twitter"` over `internal/ design/ docs/` returns
every site in one pass. Reviewing the diff cannot, by construction: the stale lines are the
ones the diff did *not* touch.

**Not every hit is stale, and changing a true one is its own defect.** Five of the sweep's
hits pair Reddit and X for *other* shared properties that remain true — the account-mismatch
invariant, the absent result-URL fallback (`RedditURL`/`TwitterURL` are bare constants), the
`ErrAccountNotSelected` resolver ordering, the cascade-error wrapping, and the sentinel
tagging in `internal/domain/errors.go`. Each was read in context and deliberately left. The
grep localizes candidates; only the surrounding paragraph decides.

One roster was replaced rather than corrected. `internal/service/connection.go` explained
that membership should be "stated as the SHAPE rather than by naming which providers
implement `AccountLister`" — and then named them, in a parenthetical that X had just
falsified. The names are gone; the compile-time assertion block in
`internal/dispatch/account_discovery_test.go` is now cited as the self-updating list.

## A rationale over a field nothing reads is not documentation

`AdAccount.Timezone` was decoded, trimmed, and justified with a paragraph arguing that "two
accounts that differ only by timezone are genuinely different choices" — while
`twitterAccountLabel` never read it and `AccessibleAccount` carries only `{id, label}`. The
only reader was a test assertion. The argument was correct and the code did not act on it,
which is worse than either alone: a reader trusts the paragraph and concludes the picker
distinguishes those accounts.

The sibling settled the shape. `linkedInAccountLabel` renders `Currency` into the label for
precisely this argument, so the timezone now joins the NAME — `"LF Events [America/Los_Angeles]"` —
rather than the notes, which stay reserved for reasons an account may not be usable. Deleting
the field was the other honest option; wiring it was chosen because the justification is
sound and LinkedIn had already committed the codebase to that answer.

`AdAccount.Approved()` was the same shape without the redeeming argument: no non-test caller,
and a doc comment explaining where it is *not* used while naming nowhere it is. It was
removed rather than wired. A positive `Status == "ACCEPTED"` check would have contradicted the
allow-list discipline the rest of the type documents at length — `ApprovalLabel` returns `""`
for accepted, absent AND unrecognized statuses alike precisely because X publishes no complete
enum, so "not approved" cannot be read as "defective". The two tests now assert `Status`
directly, which is what they were really pinning.

## Test hygiene: an unobservable race is still a missing edge

Four tests in `internal/platform/twitter/accounts_test.go` read `calls`/`queries`/`paths`
after the client call without taking the mutex the handler takes. `go test -race -count=20`
was **clean before and after the fix** — a single sequential client separates the accesses in
real time, so the detector never observes a conflicting pair. The fix establishes the
happens-before edge that a future `t.Parallel()` or concurrent case would need; it did not
repair a failing build, and no behaviour changed. Recorded so the next reader does not infer
from the diff that `-race` had caught something.

The sweep also caught a fourth site the review had not named
(`TestListAdAccounts_RequestsTheCollectionNotTheConfiguredAccount`), which is the usual result
of fixing the class rather than the listed lines.
