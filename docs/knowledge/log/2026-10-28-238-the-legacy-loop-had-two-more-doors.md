# 2026-10-28 — the legacy list loop had two more doors onto "deleted"

**Fix** — review round 2 on #403. The #238 fix landed the `Missing`/`Unresolved` distinction and
wired it through the v3 loop and the legacy loop's MAPPED read. The legacy loop makes three
HubSpot calls, not one, and the other two still decided on absence alone.

## What was still wrong

- **A failed id mapping read as "no mapping."** `ListIDForLegacy` returned `""` for a legacy id
  HubSpot has no v3 copy of AND for a mapping call that was refused or throttled. `readFailed` was
  only ever set inside the `mapped != ""` branch, so a revoked token or a throttled portal put
  every legacy row through `Missing: !readFailed` — `missing: true`. That is #238 verbatim,
  reached through a different call.
- **A failed legacy name lookup read the same way.** `LegacyListName` swallowed its error too, so
  a 503 on the last call that could have found the list was indistinguishable from the list not
  being there.
- **An unresolved mapped row linked to the legacy id.** `add` built `HubSpotURL` from
  `brief.ListID`, which on that path was still the legacy id. `ListURL`'s `objectLists` route
  resolves v3 (ILS) ids only, so the one remedy link on the row opened HubSpot on "list not
  found" — the deletion the row exists to say it cannot confirm. Raised by Copilot and by the
  review agent independently.

## What changed

- `LegacyListName` and `ListIDForLegacy` now return `(string, error)`. Still best-effort: `""`
  remains the answer for a 404 and for a failure alike, and no caller is failed because one list
  could not be named. The error is simply available, so a caller that must separate "HubSpot
  reported no such list" from "the lookup never reached an answer" can ask `IsNotFound`.
  `listName` discards it, with a comment saying why.
- `listBriefs`' legacy loop seeds `readFailed` from the mapping error and lets a non-404 name
  lookup failure set the row too. Every one of the loop's three calls now decides the same way the
  v3 loop does.
- An unresolved row that HAS a mapped v3 id carries it as `ListID`, so its link resolves. The
  legacy id stays in `ResolvedFromLegacyID`.
- `add` builds `HubSpotURL` only when the row's `ListID` is not still its `ResolvedFromLegacyID`.
  A row with no v3 id — never mapped, or resolved by legacy name alone — gets no link rather than
  one onto "list not found". That also retires a link that was wrong before #238: a legacy-named
  row has always been handed the v3 route for an id it cannot resolve.
- `design/audience_builder.go` says so: `hubspot_url` is absent on a missing row, and on a row
  whose `list_id` is still the legacy id, and its absence is never itself a statement about the
  list.
- `audience.ListBrief`'s comments corrected: "at most one of Missing and Unresolved", not
  "exactly one" — a resolved row has neither — and `HubSpotURL`'s comment no longer says the link
  goes empty when the list "no longer resolves".

## Scope

Read-path classification and link construction. No request is added or removed, no list is
written, and a row that resolves is byte-identical to before — except that a legacy-named row no
longer carries a link that never worked.

## Tests

`TestLastSent_LegacyListReadFailure` (`internal/dispatch/audience_explorer_test.go`) puts five
legacy ids on one send, one per outcome of the loop: mapped-then-refused (unresolved, linked by
the MAPPED id), mapped-then-404 (missing, no link), mapping refused (unresolved, no link), mapping
refused with a clean 404 on the name (unresolved — the mapping error is the row's only signal),
and no mapping with a refused name lookup (unresolved — the name error is). Each of the four
behaviour changes was mutated out in turn and the test goes red on each, naming a different
assertion every time.

Refs #238, #403
