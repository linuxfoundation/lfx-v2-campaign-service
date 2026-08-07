# 2026-08-07 — A 429 that outlives the retry budget is ambiguous, not a clean failure

**Update** — Closed the review finding on PR #79 (`internal/platform/meta/client.go`
and `client_test.go`).

`createOutcomeAmbiguous` classified every 4xx as a definite rejection, and a table test
pinned 429 to `false` with the note "handled by retry, not here." That note describes the
common case, not the case the function is asked about: reaching it AT ALL means the bounded
retry in `doRequest` was already exhausted (or aborted on an over-cap `Retry-After`). What
survives the retry budget is a throttle we never got past — and **a throttle is not a
rejection.** It establishes neither of the two facts callers read out of this helper.

Two callers, two different facts, both unestablished:

- A **mutating** call may have been shed AFTER Meta committed the node. Classified clean,
  it invites the blind retry that duplicates a PAID campaign — the exact failure the 3xx
  branch below it already guards against.
- The **name lookup** exists to establish that the campaign name is ABSENT. A throttled
  lookup establishes nothing, so returning a bare error tells the operator the opposite of
  what is known.

That second caller is why the 429 branch is deliberately **not** method-gated, unlike the
3xx one. The 3xx gate asks "could this have created something?", which a GET cannot. 429
also has to answer "did we establish absence?" — and a GET cannot answer that either, so
gating on the method would leave the lookup path exactly as wrong as it was.

Reachability, recorded so it is not overstated: the name lookup is gated on
`CampaignInput.ReconcileByName`, which is false everywhere today, so nothing in production
takes the lookup half of this path yet. The mutating half is live. The end-to-end test has
to set the flag explicitly, which is itself the documentation of that gate.

`TestCreateCampaign_ThrottledNameLookupIsUnconfirmed` drives a lookup that 429s until the
budget is spent and asserts three things the unit table cannot: that the retry budget was
genuinely exhausted (more than one GET), that **no mutating call is attempted** afterwards,
and that the caller gets an UNCONFIRMED partial carrying the "verify in Ads Manager" step
rather than `(nil, err)`. Revert-verified — with the 429 branch removed it fails on the
missing partial.
