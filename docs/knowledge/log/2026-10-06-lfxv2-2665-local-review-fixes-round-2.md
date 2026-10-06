# 2026-10-06 — LFXV2-2665 Local-review fixes, round 2

Six findings from the local pre-PR review of `f1b888ee..ffacbe65`, all Important, none
Critical. The review also confirmed no over-refusal was introduced by the two earlier
rounds — the one failure mode this preflight is not allowed to have.

**Unbounded caller echoes survived the first capping pass.** `validateMoney`
(`assets_extended.go`) echoed the RAW `currencyCode` rather than the normalized
`currency`, uncapped; `validateAssetDateWindow` echoed `startDate`/`endDate` uncapped at
four arms. Measured: 20,082- and 20,058-byte errors from a 20,000-character input. These
errors persist UNENCRYPTED as `Steps` entries, which is why the rule is about the whole
error and not only about URLs. Both now go through `capForError`, and `validateMoney`
echoes the value it actually tested the regex against.

**The lesson the first pass missed: a shape-anchored regex is not a length bound.**
`assets_leadform.go`'s duplicate-field arm was recorded in the previous log entry as
"already correct" because the value had passed `enumShapeRE` before reaching it. But
`enumShapeRE` anchors the SHAPE — `A` followed by any number of underscores matches at any
length. The concept file now says this in place, so the next reader does not have to
re-derive it from a counterexample. The general form: *having been validated* is not the
same as *being bounded*, and only a length check establishes the second.

**Three ad-group names were trimmed where the campaign path sanitizes.**
`display_creative.go`, `video_creative.go` and `pmax_creative.go` composed their
group name with `strings.TrimSpace(in.EventName)`. A control character inside `EventName`
survives a trim, passes the whole preflight, and is rejected by Google at
`adGroups:mutate` — which runs AFTER the budget and campaign are created. That is a
stranded PAID campaign, precisely the orphan the preflight exists to prevent. All three
DERIVED names now use `sanitizeNamePart`, the helper the campaign name has always used.

**Where the name is the operator's, the arm is refusal, not sanitization.** Performance Max
is the one path that takes a caller-supplied `AssetGroupName`, and running that through
`sanitizeNamePart` would return a different name than the operator typed — it also maps `|`
to a space and collapses whitespace runs, neither of which Google objects to in an asset
group name. So the supplied branch refuses exactly what the field cannot hold — NUL, LF and
CR, the same three runes `returnedCampaignName` names — and passes TAB, `|` and format
characters through untouched. Refusing more would be over-refusal: a create Google would
have accepted, stopped locally. The general shape: sanitize a value this code DERIVED,
refuse a value the caller CHOSE.

**Demand Gen stays on `TrimSpace`, and that is a gap rather than an exemption.** Its ad
group name is attached to live campaigns, and `sanitizeNamePart`'s whitespace-run collapse
would rename them out from under the name-based reconciliation the name exists for. So the
control-character stranding described above remains OPEN on Demand Gen. Closing it needs a
migration of the live names, not an edit to the line. The composition moved into
`demandGenAdGroupName` so the Display collision test can call BOTH productions rather than
re-derive this one as a literal — a literal keeps passing when the code it mirrors moves,
which is the half of a collision a test cannot see from the other side.

**Display's ad group collided with Demand Gen's, byte for byte.** Both composed
`"<event> - Display"`, Demand Gen because it once WAS the Display channel. The collision
did not exist until this branch made both slots fillable under one brief, and it defeats
the name-based reconciliation each relies on. Demand Gen's suffix is the one already
attached to live campaigns, so the NEW channel moved: `"<event> - Display Network"`. The
test asserts the INEQUALITY against Demand Gen's composed name rather than only the suffix
— a later "simplify the suffix" edit has to fail in the test rather than in production.

**Two stale texts, both made stale by this branch's own edits.** The `DeviceBidModifiers`
comment in `internal/dispatch/googleads.go` still said Search was the only channel reaching
device criteria; Video and Display both reach them now, and the comment was edited in THIS
branch, so it was stale as written. It now states what the concept file already stated
correctly: `CONNECTED_TV` is absent from `deviceTypes`, so no channel can ask for it, and
that is this client's own named gap rather than an upstream rule. And
`validateConversionActions`' refusal still told the caller to "create a Search campaign"
after Video and Display had been admitted — a refusal message that under-states where the
input IS accepted pushes the operator toward a channel they did not want.

**Pattern across four of the six.** Each was a true statement that a later edit falsified
without touching the sentence: the capping invariant, the "already correct" note, the
device comment, the refusal message. A comment that names a CONDITION ("Search is the only
channel that…") goes stale silently when the condition changes, whereas one that names a
MECHANISM ("`deviceTypes` does not list it") stays true or fails loudly. Prefer the second
shape when the surrounding set is still growing.
