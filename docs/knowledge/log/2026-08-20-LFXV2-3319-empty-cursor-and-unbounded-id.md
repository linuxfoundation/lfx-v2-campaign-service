# 2026-08-20 — LFXV2-3319 the sibling arms two earlier fixes left behind

**Fix** — two findings on the X ad-account discovery walk, both of the same shape: an earlier
fix in this PR established a distinction and then applied it to only one of the two places the
distinction lives.

## An empty cursor still read as a full enumeration

The walk's contract is that it returns EVERY account or an error, and this PR had already
fixed one false-absence hole in it: `data` absent vs an explicit `null` vs a present `[]`,
which a nil check alone cannot tell apart. The cursor had the identical problem one field
over, and the earlier cursor fix only closed half of it:

```go
if !resp.NextCursorPresent { return nil, fmt.Errorf("...no next_cursor field...") }
if resp.NextCursor == "" { return accounts, nil }   // "" treated as exhaustion
```

`NextCursorPresent` was added precisely because a plain string field collapses `null` and
absent onto `""` — the presence bit recovers absent. But it does not recover the OTHER
collapsed pair: a body carrying `"next_cursor":""` is present, so it sails past the first
guard, and then the `== ""` check reads it as the end of the walk. X's contract documents
termination as an explicit null ("If less than `count` entities are returned in the current
page of the result set, the `next_cursor` value will be `null`") and gives `""` no meaning at
all. So the walk stopped on a body that never said it was finished and returned the accounts
gathered so far AS A COMPLETE LIST.

That failure mode is invisible where it matters. This list populates an account picker: a
truncated picker looks exactly like a full one, and the user simply does not find the account
they were looking for — with no error anywhere to explain why.

Fixed by carrying the one bit that survives only in the raw bytes:

```go
raw, e.NextCursorPresent = keys["next_cursor"]
e.NextCursorNull = e.NextCursorPresent && string(raw) == "null"
```

Compared against the literal rather than decoded, because decoding is exactly what erases the
distinction — the same reason the `data` guard inspects the DECODED slice instead of the raw
bytes, in mirror image. `ListAdAccounts` then terminates only on `NextCursorNull` and treats
an empty cursor as an error. `findByName` still consults neither bit, deliberately: it is
bounded by the match it is searching for and already errors when it runs out of pages.

**The existing test asserted the broken behaviour.** `TestListAdAccounts_ExplicitNullCursorTerminates`
ran a two-body table:

```go
`{"data":[{"id":"abc123"}],"next_cursor":null}`,
`{"data":[{"id":"abc123"}],"next_cursor":""}`,
```

and its doc comment described both as "a present null/empty cursor is exhaustion". The test
was written to stop the absent-cursor guard being over-tightened into rejecting every falsy
cursor, which is a real hazard worth pinning — but it pinned it by asserting the empty string
is a termination signal, which X's contract never says. It now covers the null body only, and
`TestListAdAccounts_EmptyCursorIsNotExhaustion` pins the opposite for the empty one.

## The id charset was enforced without the length the design advertises

`design/connection.go` constrains `twitter-ads-connection-config.account_id` with BOTH
`Pattern(^[A-Za-z0-9]+$)` and `MaxLength(64)`, and Goa enforces both at bind time. Discovery
checked only the charset, via the shared `accountIDRe`. A 65+ character alphanumeric id
therefore passed the walk and was offered to the user as ready to store — and then rejected
as a 422 every single time it was selected. A dead entry in the picker, identical in
appearance to a live one, failing only at the last step.

The bound is applied at the discovery site as `len(id) > maxAccountIDLen` rather than by
tightening `accountIDRe`, because that regexp also guards the campaign-create, metrics and
toggle paths. Those validate an id ALREADY stored on a connection, where a length the design
admitted at bind time is not theirs to re-litigate; this walk is the one deciding what to
OFFER, so the offer is what must match the design.

**The general rule:** when discovery advertises a value a later step will bind, discovery must
enforce every constraint that step enforces — not just the one that happens to be shared with
an existing helper. Reusing `accountIDRe` was right and was reasoned about explicitly in the
code ("an account this walk offers must be one the client will later accept"); the gap was
that the client-side guard was never the whole contract, because the DESIGN adds a bound the
client does not check.

## Mutation-verified

Every revert COMPILES, so none is answered by a build break:

```
`if false && resp.NextCursor == ""` (neuter the empty guard)  -> EmptyCursorIsNotExhaustion FAILS
drop `|| len(id) > maxAccountIDLen`                           -> OverlongIDFailsTheWholeWalk FAILS
`len(id) >= maxAccountIDLen` (off-by-one at exactly 64)       -> OverlongIDFailsTheWholeWalk FAILS
NextCursorNull set from presence, not from the raw bytes      -> EmptyCursorIsNotExhaustion +
                                                                 NextCursorPresent...FindByName FAIL
```

The off-by-one mutation is the one that earns its place: the fix's whole claim is that it
matches the design's `MaxLength(64)`, and a bound that rejected a 64-character id would break
that claim in the opposite direction while still passing a test that only supplied 65. The
last mutation is the counterpart at the envelope layer — it shows the new bit is genuinely
read from the bytes rather than being an alias for presence, which is what the fix's
correctness rests on.
