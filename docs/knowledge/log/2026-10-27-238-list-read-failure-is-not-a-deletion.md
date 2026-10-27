# 2026-10-27 — a list read that failed is no longer reported as a deleted list

**Fix** — `listBriefs` (`internal/dispatch/audience_explorer.go`) resolved every list id on a
prior send through `GetList`, and treated **any** error the same way: fall back to
`LegacyListName`, and if that gave nothing, mark the row `Missing`. `Missing` is published as
"the referenced list could not be resolved" and read by an operator as "this list was deleted".

So a throttled portal, an expired token, a scope the private app no longer has, or a HubSpot 5xx
rendered a live suppression list as gone — and the action that follows from "the suppression list
was deleted" is rebuilding it. `hubspot.IsNotFound` already existed for exactly this distinction,
and its own doc comment says it: a 404 is an answer to show the operator, any other failure leaves
the list's existence unknown.

## What changed

- `audience.ListBrief` gains `Unresolved bool`. `Missing` now means HubSpot said there is no such
  list; `Unresolved` means the read failed for another reason. Exactly one is ever set.
- `listBriefs` decides on the v3 error: `hubspot.IsNotFound(err)` (or a nil error with no list) is
  `Missing`; anything else is `Unresolved`. The legacy-id loop decides the same way on the mapped
  id's read, because an id that mapped had a v3 list to read.
- `LegacyListName` and `ListIDForLegacy` are unchanged and remain best-effort — they swallow their
  own errors, so neither can carry the distinction, and neither is asked to.
- An unresolved row keeps its `HubSpotURL`: the remedy is to open the list by hand. A missing row
  still has none.
- The Goa design (`design/audience_builder.go`) publishes `unresolved` as a REQUIRED boolean on
  `audience-list-brief`, with `missing`, `name` and `hubspot_url` descriptions reworded to match.
  The shared resolved-list examples set both flags false.

## Scope

Read-path classification only. No request is added or removed, no list is written, and a row that
resolves is byte-identical to before.

## Tests

`TestLastSent_AFailedListReadIsNotReportedAsDeleted` (`internal/dispatch/audience_explorer_test.go`)
puts two unresolvable ids on one send — one answering 403 from both APIs, one answering 404 — and
asserts they no longer come back identical. It fails three ways on the old code.
`audience_builder_examples_spec_test.go` gains the `missing`/`unresolved` exclusion as a third
published-example invariant.

Refs #238
