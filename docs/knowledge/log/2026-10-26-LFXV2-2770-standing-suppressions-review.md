# 2026-10-26 — Pre-PR review of the standing suppression lists (LFXV2-2770)

**Fix** — review findings on 2026-10-26-LFXV2-2770-standing-lists-grow-only.

- `audience.RegistrantsListMatches` also requires `CoveredEditions` to be exactly the edition. The
  exclusive check alone accepted a list holding only a slice of the edition (an AND adding a
  contact property beside the registration, or an extra filter inside the event branch), so
  registrants outside that slice were not suppressed.
- `ensureStandardList` confirms every write by reading the list back and merges again when another
  writer's stale overwrite dropped an id, up to three writes, then fails. Not a lock; the exact
  guarantee is in [internal/dispatch](../code/internal-dispatch.md).
- A list found under a standing name must also be a contact list (`objectTypeId` 0-1), or it is the
  same 409.
- compose-master's result gains optional `suppression_list_ids` (standing list first, then each other
  excluded list, then the current-registrants list), matching what attach-existing returns and what
  a recording compose stores. OpenAPI regenerated.
- `SuppressionLists`: a failed search for the registrants row or the standing link is logged and
  left unresolved instead of failing the whole picker; a permission rejection still fails it.
- [internal/service](../code/internal-service.md) now says compose produces only the `master_name` and
  `master` partial shapes (the suppression ones are legacy contract fields), and records the 409
  mapping and the compose ids.
- Tests: both slice shapes, an overwrite after the write and one on every write, non-contact standing
  lists, the lenient picker, the compose ids; the fake portal's assertions read through locked
  accessors.
