# 2026-09-03 — LFXV2-3198: a stage validated on read but not on write

**Fix** — `find-brief`'s `stage` query param carried
`Enum("", "CFP Launch", ... "Post-Event")`. `BriefData.stage` — which `BriefInput` inherits through
`Reference()`, and which is therefore the CREATE/UPDATE payload — carried only `Example(...)`.

The two asymmetric ends produced a row that could be written and never read. `POST` a brief with
`stage: "Registration push"` (lowercase p) and it is stored, returns 201, and occupies a slot in the
new unique key. Every subsequent lookup then fails in both directions: asking for the typo returns
400 from the enum -- a generated validation rejection, not a 422 -- and asking for the correct
spelling returns 404 because no such row exists. The brief
is reachable only by id, and because `replaceBriefQuery` deliberately omits `stage` from its SET
list it cannot be repaired by an update either — the only exit is archive-by-id.

Nothing else covered it AT THE TIME: there was no CHECK on `stage` (unlike `delivery_type`), and
the service layer passed the string straight through.

Three guards now close it, added over the course of this ticket:

- The `Enum` moved onto `BriefData.stage`, so the write path rejects a bad stage at the edge exactly
  as `delivery_type` already did. Safe for the response type too: every persisted stage is either
  `''` or one of the six, so no stored brief becomes undecodable.
- `TestPublishedStageEnumMatchesEmailStageNames` pins the published enum to `emailstage.Names()`,
  on BOTH the write payload and the find-brief query parameter. The design DSL cannot import a
  service package without inverting the contract direction, so the list is hand-copied there; the
  test is what makes the copy safe.
- `campaign_briefs_delivery_stage_pair_valid`, added to 000030 later in this same ticket, is the
  schema backstop. It refuses not just an unknown stage but the impossible PAIRS -- a paid brief
  with an email stage, an email brief with none -- which the two per-column enums admit
  individually. It answers for writers that never pass through this service at all: a migration, a
  backfill, a psql session. The service validates the pair before the write so an API caller gets
  a 400 rather than the 500 a raw constraint violation would produce.

**The general shape:** when a value is validated at one end of a round trip and not the other, the
gap is not "unvalidated input" — it is a state the system can enter and cannot leave. Ask of any
enum: can something be WRITTEN that no READ can name? Note also that this repo had already learned
this exact lesson on `event_slug`: the `BriefData` type in `design/brief.go` carries a comment
explaining why `MinLength` must live on the request type only, because a constraint a stored row
can fail makes that row undecodable. The stage enum is the mirror case -- it belongs on BOTH ends,
because every persisted row satisfies it.
