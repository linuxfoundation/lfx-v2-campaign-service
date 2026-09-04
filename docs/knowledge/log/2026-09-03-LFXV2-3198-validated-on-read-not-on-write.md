# 2026-09-03 — LFXV2-3198: a stage validated on read but not on write

**Fix** — `find-brief`'s `stage` query param carried
`Enum("", "CFP Launch", ... "Post-Event")`. `BriefData.stage` — which `BriefInput` inherits through
`Reference()`, and which is therefore the CREATE/UPDATE payload — carried only `Example(...)`.

The two asymmetric ends produced a row that could be written and never read. `POST` a brief with
`stage: "Registration push"` (lowercase p) and it is stored, returns 201, and occupies a slot in the
new unique key. Every subsequent lookup then fails in both directions: asking for the typo returns
422 from the enum, asking for the correct spelling returns 404 because no such row exists. The brief
is reachable only by id, and because `replaceBriefQuery` deliberately omits `stage` from its SET
list it cannot be repaired by an update either — the only exit is archive-by-id.

Nothing else covered it. There is no CHECK on `stage` (unlike `delivery_type`), and the service
layer passed the string straight through.

Two guards now close it:

- The `Enum` moved onto `BriefData.stage`, so the write path rejects a bad stage at the edge exactly
  as `delivery_type` already did. Safe for the response type too: every persisted stage is either
  `''` or one of the six, so no stored brief becomes undecodable.
- `TestPublishedStageEnumMatchesEmailStageNames` pins the published enum to `emailstage.Names()`.
  The design DSL cannot import a service package without inverting the contract direction, so the
  list is hand-copied there; the test is what makes the copy safe.

**The general shape:** when a value is validated at one end of a round trip and not the other, the
gap is not "unvalidated input" — it is a state the system can enter and cannot leave. Ask of any
enum: can something be WRITTEN that no READ can name? Note also that this repo had already learned
this exact lesson on `event_slug`, and the note recording it is a few lines above in the same file.
