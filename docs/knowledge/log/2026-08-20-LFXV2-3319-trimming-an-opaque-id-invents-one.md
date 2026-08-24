# 2026-08-20 — LFXV2-3319 trimming an opaque account id invents an account X never sent

**Fix** — `ListAdAccounts` ran each row's id through `strings.TrimSpace` before validating it
with `accountIDRe`. Three bot findings were reviewed against the code; this was the only one
that turned out to be a defect in behaviour rather than in prose. The other two are recorded
below because "the comment is wrong and the code is right" and "the doc was already fixed"
are both outcomes worth being able to recognise quickly.

## The repair defeated the walk's own stated policy

`accounts.go`'s docblock enumerates its failure modes and commits to fail-closed: "A walk that
cannot be completed is an ERROR, never a short list... Every failure mode below — ... **a row
whose id is not alphanumeric**, the page cap — returns nil rather than what was collected so
far."

`accountIDRe` is `^[A-Za-z0-9]+$`, anchored, admitting no whitespace. So a padded id `" acct1 "`
is by that enumeration a row that must fail the walk — and it did not, because the trim ran
first and handed the regexp an id the response never carried. The account id is an OPAQUE
upstream token: trimming does not tidy the row, it mints the DIFFERENT id `acct1` and offers it
in the picker as one X sent. Select it and the connection binds to an id we never saw.

The tell is that the same file already argues this exact point about the page cursor twelve
lines up — "Escaped, but NOT trimmed. A page cursor is an opaque server token echoed back
verbatim, so trimming can request a DIFFERENT page than the one offered." Identical reasoning,
identical kind of value, opposite treatment, in one function. `Name` and `Timezone` keep their
trims and should: they are display labels, and nothing binds to them.

The existing `TestListAdAccounts_UnusableIDFailsTheWholeWalk` table covered `""`,
`"acct/../other"`, `"acct?x=1"` and `"acct id"` — an INTERNAL space, which the trim does not
touch — so the whole table passed while padded ids were being silently repaired. A table of
malformed inputs proves nothing about the malformation whose repair path it happens to miss;
the padded cases are now in it, and reinstating the trim fails them.

## Where the same bug ISN'T: `"data":null` is caught, by the check that runs after decoding

The struct comment on `apiResponse.Data` claimed the field "is nil when the field was ABSENT
or null". That is backwards for the null half — `encoding/json` stores the four literal bytes
`null` in a `json.RawMessage`, leaving it non-nil with length 4 — so no length or nil check can
separate absent from null from `[]`.

The comment was wrong and **the code was right anyway**, which is the outcome that is easy to
get wrong in review. `ListAdAccounts` does not branch on `len(resp.Data)` to decide the answer;
it decodes first and then tests the DECODED slice (`if elements == nil`). Absent and null both
leave that slice nil, a present `[]` yields a non-nil empty slice, and that is precisely the
distinction the guard needs. `accounts.go` already carried a second, CORRECT comment saying so,
contradicting the struct comment three files away. Only the false one was changed.

Worth stating how that was settled, because reasoning about `encoding/json` from memory is what
produced the false comment: a four-line probe printing `nil`/`len` for `{"data":null}`, `{}`,
`{"data":[]}` and a populated body answers it in seconds. Mutating the guard to the naive
`len(resp.Data) == 0` then made the existing null fixture return `accounts=[]` — a healthy zero
where the body said nothing — so the test was already load-bearing.

The other `Data` readers (`extractID`, `extractPromotedTweetID`, `verifyAccount`) were checked
too. Each decodes and then tests the decoded VALUE, so a `null` yields `""`/no rows — the same
conservative answer absence gets. No second site to fix.

## A stale doc claim that a later commit in the same branch had already swept

The third finding — the HubSpot row in `docs/api-catalog.md` still saying "Reddit and X
implement neither" — was TRUE of the merge base and already false of HEAD: the branch's own
`fix(twitter): sweep stale discovery claims` commit had corrected it and the sibling `:184`
roster. Nothing to change. A bot finding is against the revision it read, so confirming a doc
finding means diffing base against HEAD, not just reading HEAD and agreeing the claim is stale.

The one surviving instance of the phrase repo-wide is in
`docs/knowledge/log/2026-08-16-LFXV2-3064-linkedin-half-claim.md`, where it is a dated record of
what was true that day. Fragments are history and are not retro-edited; a sweep for a falsified
claim has to stop at `docs/knowledge/log/`.
