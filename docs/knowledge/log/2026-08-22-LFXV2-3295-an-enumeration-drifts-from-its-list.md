# 2026-08-22 — an enumeration drifts from the list it introduces

**Docs** — two corrections on the creative-asset storage PR, both found by reading a sentence
against what follows it.

`docs/knowledge/code/internal-infrastructure-postgres.md` introduced `CreateAsset`'s single
statement with "Four things are doing work here" above a list of FIVE bullets: the `WHERE EXISTS`
parent gate, the no-op `DO UPDATE`, the composite parent FK, the bound `byte_size`, and the
bytes-free returned column set. The fifth was added when the returned column set became load
bearing, and the count above it was not touched.

This is the failure mode a counted enumeration invites: the number and the list are two
statements of the same fact, and only one of them is checked by anything. A reader who trusts the
count stops after four and misses that `bytes` is deliberately omitted from the returned columns —
which is the bullet most likely to be undone by accident, since adding a column to a shared
`RETURNING` list looks harmless.

`000028_create_creative_assets.up.sql` read "CHECK CHECK (byte_size = octet_length(bytes))" in the
prose above the constraint, a duplicated word from an edit that rewrapped the line. The SQL itself
was always correct; only the sentence describing it was not.

Neither is a behaviour change and neither could have been caught by a test — the code both
sentences describe passes either way. Recorded because the class is worth naming: prose that
COUNTS or QUOTES the thing beside it acquires a second copy of that fact, and the copy drifts
silently.
