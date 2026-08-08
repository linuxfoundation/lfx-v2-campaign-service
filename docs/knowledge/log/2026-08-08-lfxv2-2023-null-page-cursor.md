# 2026-08-08 — LFXV2-2023: a null cursor truncates pagination into a false absence

**Update** — `nextPageToken` now decodes through a `pageToken` type that refuses an explicit
JSON `null`, matching what `searchRows` already does for `results`. Suppressed finding on PR
#90, verified before acting.

## The mechanism

Decoded as a plain Go string, all three of these are indistinguishable:

| body | decoded token |
|---|---|
| `{}` (omitted) | `""` |
| `{"nextPageToken":null}` | `""` |
| `{"nextPageToken":""}` | `""` |

and `""` is what `gaqlSearchForCustomer` reads as "last page". So a response carrying an
explicit null cursor stops pagination at page 1 — silently, with no error and no signal that
anything was cut short.

## Why it is expensive here specifically

This is the fail-closed lookup class. `FindCampaignByName` reports "no match in any page" as a
clean absence, and its caller treats a clean absence as a licence to CREATE. Truncated
pagination therefore turns a campaign sitting on page 2 into a **duplicate paid campaign**.
The result set was never empty; it was read as complete.

That is a different route to the same destination as the two guards already in place. The
top-level and `results` nulls produce an EMPTY result set; this one produces a TRUNCATED one.
Both answer "absent" about a record that exists.

## Why refusing is free

proto3 JSON emits an unset string field as `""` or omits the key entirely — it never emits
`null`. So no conformant server can send the shape being refused, and the guard costs nothing
legitimate. An omitted key is untouched (`UnmarshalJSON` is not called for it), which is
important because omission is exactly how a real final page is spelled.

## Test

The regression table gains `{"results":[],"nextPageToken":null}`, deliberately with a
LEGITIMATE empty `results` so the case can only fail through the cursor. Revert-checked:
softening the guard to `*t = ""` fails it with `a null result set must not report a clean
absence, got ""` — the clean-absence value itself, which is the whole point.
