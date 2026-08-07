# 2026-08-07 — LFXV2-3038: pin the missing-actor warning, correct the schema docs

**Update** — Review round on the brief actor-attribution branch turned up three gaps, all
of the same shape: something is asserted in prose that nothing enforces.

**Fix** — `TestBriefActor_MissingActorWarns` asserts the warning `attributedActor` emits when
a write carries no authenticated principal, including its `operation` attribute. An
unattributed write fails nothing — the row commits with NULL attribution and the response is
an ordinary 2xx — so this log line is the entire operational signal, and its rate is what
alerting keys on. `TestBriefActor_MissingActorStillWrites` only checks that the write
succeeds, so the line could be deleted or demoted and every actor test would stay green.
Verified binding twice: demoting `WarnContext` to `InfoContext` fails with "no WARN record",
and dropping the `operation` attribute fails naming the attribute.

**Fix** — `docs/architecture.md` D5 said *who* performed a write is recorded inline on the
row, without qualification. This branch adds the columns to `campaign_briefs` only;
`campaigns` still records *when* but not *who*. D5 now says so and points at the follow-up,
so the architecture does not advertise attribution the `campaigns` table cannot store.

**Fix** — The Persistence ER diagram's `campaign_briefs` block jumped from `approved_at`
straight to the timestamps, omitting both columns migration `000015` adds. Added
`created_by` / `updated_by` there, matching the `google_ads_connections` block above it.

**Fix** — The placeholder assertion was positional-only. `require.Regexp(placeholderRe, …)`
accepts any `$N`, so `createBriefQuery`'s actor moving from `$11` to `$10` — a
one-character edit that persists the APPROVER as both author and last editor — left every
subtest green. The table now names the exact placeholder per column (`$11`, `$9`, `$1`,
`$3`), which is the only thing tying a column to the argument the repo method passes. The
arguments are all strings, so a neighbouring placeholder type-checks, commits, and reads
back as a plausible actor; nothing but the index catches it. Revert-verified with exactly
that `$11`→`$10` edit.

**Fix** — Architecture D5 said actor columns exist "on `campaign_briefs` and `connections`
only", which contradicted `docs/channel-connections-schema.md` and
`audience_repo.go:30,73,97`: `campaign_audiences` has carried `created_by` since it landed.
The row now states the coverage precisely — briefs and connections carry both columns,
audiences `created_by` only, campaigns neither.
