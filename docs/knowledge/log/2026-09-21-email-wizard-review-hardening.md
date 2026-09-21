# 2026-09-21 — email wizard: XSS, tenancy and retention hardening

**Review round** — six defect classes found reviewing the wizard added on
[2026-09-16](2026-09-16-email-creation-wizard.md). Four of them were introduced by the fix for
the previous one, which is the part worth recording.

## The sanitizer, and where it belongs

`rich_text.html` is model-supplied and reached three sinks: the preview document served to the
operator, the HubSpot draft body sent to recipients, and — missed for two rounds — the raw
`sections` / `variant_a_sections` JSON returned to the caller.

`sanitizeWizardHTML` is an allow-list tokenizer over `golang.org/x/net/html`. Tokenizing rather
than pattern-matching is load-bearing: it defeats the split-tag trick (`<scr<script>ipt>`) that
any regex over tag names misses. `script`, `style`, `iframe`, `object` and `embed` drop their
CONTENT as well as the tag, because their text is markup rather than copy.

The lesson is the placement, not the sanitizer. It ran at the two RENDER paths first, which is
why the third sink survived: every new consumer of the sections is another chance to forget. It
now runs where the sections are PARSED, so a consumer cannot receive unsanitised HTML without
bypassing the parser entirely.

## The same shape, three more times

- **A lifecycle check on 4 of 7 handlers.** Review flagged one; auditing found three missing
  (`set-send-list`, `update-sections`, `get-session`). Moved into `loadWizardSession`, which all
  call sites already go through, with the repo as a REQUIRED parameter so the compiler enforces
  it on the next handler.
- **`ORDER MATTERS` inside that helper.** Checking the brief before reading the session let a
  delete land between the two reads and hand back a post-scrub session whose version a later
  save still matched — repopulating what the delete had just cleared. Session first, so the
  scrub's version bump makes the stale save fail.
- **A progress token the caller supplied.** The column is deliberately not UNIQUE, so an
  attacker choosing a victim's token passed the handler's project check against their OWN row
  while the hub delivered the victim's frames. Server-minted now, and the field is removed from
  the API rather than ignored.

## Retention

`chat_history`, `plan_result` and the two generated-body columns are user-authored text with no
TTL and no purge job, and `ArchiveBrief` is a SOFT delete. `ScrubSessionsForBrief` clears them
on delete. Four defects surfaced inside that one fix:

1. It cleared only `chat_history`; the operator's own guidance and both generated bodies
   survived. **A partial scrub is worse than none, because it looks done.**
2. It did not bump `version`, so an in-flight turn's pre-scrub snapshot still matched and wrote
   the transcript back. `generate-content` holds exactly such a snapshot across a model call.
3. A session could still be created against an already-archived brief — the composite FK only
   requires the row to EXIST, and the soft delete leaves it.
4. `WHERE EXISTS` did not close that under READ COMMITTED; it needed `SELECT ... FOR UPDATE`,
   the shape `createCreativeAssetQuery` already used.

And making the scrub retryable made it run after ANY archive failure, destroying the work of
briefs that were still live — gated on success-or-`ErrNotFound`, since `ErrNotFound` is the only
failure that PROVES the brief is gone.

## Verification

Every fix is mutation-checked: revert it, and a test must fail with a diagnostic naming the
consequence. Two are worth singling out — removing only the `FOR UPDATE` (keeping `WHERE
EXISTS`) reproduces the leak on all three runs within a few iterations, and removing the SSE
deadline extension reproduces the reported `unexpected EOF` verbatim.

The repository layer gained its first live-Postgres coverage
(`internal/infrastructure/postgres/dbtest/wizard_scrub_live_test.go`). The pre-existing
source-text tests assert over SQL strings, which cannot reach whether Postgres accepts the
statement, whether the bind arguments land on the columns they name, or whether the WHERE that
is the tenancy boundary on a destructive UPDATE actually holds.
