# 2026-08-20 — LFXV2-2643 the scratch DSN dropped everything it did not name

**Fix** — `freshDatabase` claimed to "swap the database name in the DSN" but
REBUILT the URL from four parsed fields (`user`, `host`, `port`, `name`) with a
hardcoded `?sslmode=disable`, so every part it did not name was discarded.

What was OBSERVED. CI's DSN is, verbatim from
`.github/workflows/lfx-v2-campaign-service-build.yaml:90`:

    CI DSN : postgres://USER:PASS@127.0.0.1:5432/campaign_service_test?sslmode=disable
    OLD    : postgres://USER@127.0.0.1:5432/scratch_1?sslmode=disable
    NEW    : postgres://USER:PASS@127.0.0.1:5432/scratch_1?sslmode=disable

Exactly ONE real loss against today's CI value: **the password**. That alone is
fatal — CI authenticates over TCP with a `user:password` pair, so the stripped
URL cannot connect at all. The hardcoded `sslmode=disable` happened to MATCH
what CI already sets, and there is no `connect_timeout` in the CI DSN, so
neither was actually lost. `CREATE DATABASE` still succeeded, because that runs
on the original `TEST_DATABASE_URL` — only the later connects to the new
database used the stripped URL.

What was NOT observed, and is the reason the fix is still the right one. A
rebuild-from-parts drops whatever it does not enumerate, so had the CI DSN
carried `sslmode=require` or a `connect_timeout`, the same line would have
silently DOWNGRADED the TLS mode and dropped the timeout. That is a property of
the construction, not a thing that happened here — and it is hypothetical
precisely because it depends on a value someone may change tomorrow without
touching this file.

**It could not fail locally.** A developer DSN authenticates by peer/trust and
carries no password, so the reconstruction happens to be lossless for the exact
input every local run supplies. CI authenticates with a user:password pair
over TCP, where the same code cannot authenticate at all. The test's own gate
(`TEST_DATABASE_URL` must be set) is what made the gap invisible: the variable
was set in both places, with materially different contents.

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

**Second rule, earned by this entry's own first draft** — state the loss you
OBSERVED, not the losses the defect's shape allows. The original draft of this
fragment asserted "three losses in one line: the password, the `sslmode`
(silently DOWNGRADED from `require` to a hardcoded `disable`), and
`connect_timeout`", over an illustrative DSN labelled "CI DSN". Only the
password loss was real; the workflow file says `sslmode=disable` and carries no
`connect_timeout`. Reasoning from what a whitelist-rebuild *can* drop produced
a concrete-looking three-item finding that no CI run ever exhibited — and it
read as evidence because it was formatted as a diff. If a line is labelled with
where it came from, it must be pasted from there. (*Corrected in place
2026-08-20.*)
