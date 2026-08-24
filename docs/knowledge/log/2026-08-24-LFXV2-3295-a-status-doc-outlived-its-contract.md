# 2026-08-24 — a status-code sentence outlived the contract it described

**Fix** — `docs/knowledge/code/design.md` still said the creative-asset upload *"responds `201`"*,
full stop. That was true when written. It stopped being true when the endpoint became idempotent
on `(brief_id, checksum)` and grew a second success status:

```go
Response(StatusCreated, func() { Tag("created", "true") })
Response(StatusOK)
```

201 now means *this request stored the asset*; 200 means *an identical upload already existed and
the stored row was returned unchanged*. An unconditional 201 told a retrying client it had created
a resource when nothing was created — which is the whole reason the 200 arm was added, so a
package concept that still promises only 201 documents away the fix.

## The sweep, not the line

The review named one line. The claim is a *class* — "what status does this endpoint return" — and
it can be asserted anywhere in the bundle, so the repair is a sweep of every site that states it:

| site | state | action |
| --- | --- | --- |
| `docs/knowledge/code/design.md:126` | unconditional `201` | rewritten |
| `docs/knowledge/code/internal-service.md:221` | already names both arms | none |
| `docs/knowledge/log/…-a-constraint-must-name-the-thing-it-measures.md:35` | already names both arms | none |

Only one site was stale. That is the useful outcome of a sweep, not a wasted one: it is the
difference between *knowing* the class is clean and *hoping* the cited line was the only instance.

## Editing a line adopts the claims you keep

The rewritten sentence also carries forward *"NO ETag: creative assets are insert-only and carry
no version"*. Preserving a clause makes it the editor's claim, so both halves were re-derived
rather than trusted:

- **no ETag on either arm** — `design/brief.go`, the comment above the two `Response(...)` lines
  states it and neither response declares one.
- **insert-only, no version** — `000028_create_creative_assets.up.sql:28`, *"Insert-only: an
  asset's bytes are immutable once stored, so there is no version or"*.

Both hold. Had either failed, the edit would have laundered a stale claim into a fresh commit,
which is worse than leaving it — a reviewer reads a just-touched line as verified.
