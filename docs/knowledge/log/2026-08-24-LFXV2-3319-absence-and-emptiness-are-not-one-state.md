# 2026-08-24

**Fix** — X Ads find-or-create: a 2xx body carrying `"data":null` decoded into a nil element
slice and was reported as a confident "not found", which the caller answers with a create POST.
`findByName` now rejects a nil decoded slice before it classifies the cursor, matching the guard
`ListAdAccounts` already had.

Three malformed shapes were reproduced against the shipped walk before anything was changed,
because the review comments disagreed about which one was still live:

| body | findByName (before) | ListAdAccounts (before) |
| --- | --- | --- |
| full page + `"next_cursor":""` | error | error |
| full page + cursor key absent | error | error |
| `{"data":null,"next_cursor":null}` | **`("", nil)` — confident not-found** | error |

The first two had already been closed by routing both walks through `cursorVerdict`. The third
had not, and it is a different axis: the earlier fix unified how the two walks read the CURSOR,
while this one was about the DATA field, so a shared cursor classifier could not have caught it.
`"data":null` with the cursor absent produced the same false not-found, which is the tell — the
defect never depended on the cursor at all.

Why it survives every check upstream of the decode: `encoding/json` stores the four bytes `null`
in a `json.RawMessage`, so `resp.Data` is non-nil with length 4, and unmarshalling those bytes
into a slice SUCCEEDS, leaving the slice nil and returning no error. Nothing objects. Only a
post-decode nil test separates the three cases, because a present `[]` is the one that yields a
non-nil empty slice. Weakening the guard to `len(items) == 0` compiles and reads as equivalent,
but it broke find-or-create across the package — an empty account is a legitimate "no campaigns"
answer and must stay a clean not-found. That mutation is the proof the nil/empty distinction is
load-bearing rather than stylistic.

The generalisation, and it is the same one this branch has now hit twice: **absence and
emptiness are different claims, and a decoder that erases the difference makes the safe reading
unavailable to every caller downstream.** A missing FIELD is not a missing ROW. The first fix
drew that line for the cursor; this one draws it for the data field. Both walks now refuse to
turn "the body told us nothing" into "the thing does not exist".

Mutation-verified with compiling reverts, each diff inspected to confirm it landed:

| mutation | result |
| --- | --- |
| `cursorVerdict`: present-but-empty → `cursorExhausted` | **FAIL in both walks** |
| `cursorVerdict`: absent key → `cursorExhausted` | **FAIL in both walks** |
| `findByName`: remove the nil-data guard | FAIL (campaign + line-item arms) |
| `findByName`: weaken guard to `len(items) == 0` | FAIL (10+ create-path tests) |
| `ListAdAccounts`: remove the nil-data guard | FAIL |

No survivors. The two cursor mutations failing in BOTH walks at once is what makes the walks
structurally unable to diverge again, rather than merely agreeing today.
