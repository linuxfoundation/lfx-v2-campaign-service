# 2026-08-20 — LFXV2-2643 the scratch DSN dropped everything it did not name

**Fix** — `freshDatabase` claimed to "swap the database name in the DSN" but
REBUILT the URL from four parsed fields, so every part it did not name was
discarded:

    CI DSN : postgres://postgres:postgres@localhost:5432/campaign_test?sslmode=require&connect_timeout=5
    OLD    : postgres://postgres@localhost:5432/scratch_1?sslmode=disable
    NEW    : postgres://postgres:postgres@localhost:5432/scratch_1?sslmode=require&connect_timeout=5

Three losses in one line: the password, the `sslmode` (silently DOWNGRADED from
`require` to a hardcoded `disable`), and `connect_timeout`. `CREATE DATABASE`
still succeeded, because that runs on the original `TEST_DATABASE_URL` — only
the later connects to the new database used the stripped URL.

**It could not fail locally.** A developer DSN authenticates by peer/trust and
carries no password, so the reconstruction happens to be lossless for the exact
input every local run supplies. CI connects over TCP as
`postgres://postgres:postgres@...`, where the same code cannot authenticate at
all. The test's own gate (`TEST_DATABASE_URL` must be set) is what made the
gap invisible: the variable was set in both places, with materially different
contents.

The fix parses, edits `u.Path`, and re-renders — the operation the comment
already described. A rebuild-from-parts enumerates what to KEEP, so anything
added upstream later is dropped by default; an edit-one-field enumerates what
to CHANGE and is closed under future additions.

**Rule** — when a helper says it changes one component of a structured value,
edit that component in place. Reconstructing from parts is a whitelist, and
whoever writes it is guessing at the full set of fields the value can carry.
And a config-shaped defect that "passes locally" is unfalsifiable by local
runs: compare a real CI value against the local one before believing a green
suite.
