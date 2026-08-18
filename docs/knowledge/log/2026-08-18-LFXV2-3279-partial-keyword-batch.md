# 2026-08-18 — LFXV2-3279 a partial keyword batch discarded the keywords it created

**Fix** — `AddKeywords` is a BATCH, and the response is index-aligned with the request. A
rejection of one keyword therefore sits alongside real ids for the ones that succeeded:
`[701, null, 703]` with a single `PartialError` means TWO keywords exist upstream, not none.

`createKeywords` classified any `PartialError` as a clean rejection and returned `nil`, so those
ids were dropped and the caller reported "keyword targeting rejected" with an empty list. Two
consequences, both real:

- the created keywords never joined the status cascade, so ACTIVATE could not enable them;
- a reconciliation had nothing to match against, so a retry of the batch would create a second
  copy of every keyword that had succeeded.

The ids now travel out WITH the error, and the message carries the count (`2 of 3 keywords were
created`) so an operator can tell a partial from a total failure. Every-entry-rejected stays a
clean failure with an empty list and no count — there is genuinely nothing to reconcile there,
and claiming a partial would be its own overstatement.

**Two stale comments corrected in the same pass**, both made false by earlier work on this
branch rather than pre-existing:

- The steps line explained that it reported "ids PARSED, not the number of keywords sent" because
  an oversized request could legitimately come back short. That gap was the 16-id decode bound,
  which this branch closed — full cardinality is now required, so the two counts cannot diverge
  and the hedge described a state that no longer exists.
- `msDate` was marked "Reserved for MS-4". MS-4 turned out to BE the keyword/bid work, which does
  not touch it. Naming a specific slice in a reservation only dates the comment; it now says what
  it is waiting for (the flight-date work) without pinning a label.

**Found by copilot on #138** as suppressed comments.
