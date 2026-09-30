# 2026-09-29 — LFXV2-2665: the snapshot URL-run pattern now matches zone-scoped IPv6 hosts

**Fix** — the bracketed-authority branch added to `snapshotURLRunRe` earlier today
was written as a hex/colon IP grammar, `\[[0-9a-f:.]+\]`. A zone-scoped literal —
`https://[fe80::1%25eth0]/reg?ticket=…`, which `net/url` accepts — does not match
it, so the run fell through to the general alternative, truncated at `]` exactly as
before, and left `]/reg?ticket=…` in the unencrypted `config_snapshot` as prose.
The branch reopened the leak for a host shape it was added to close.

The class is now "anything up to the closing `]` that is not whitespace", with
validity left to `net/url`. A tight grammar is the wrong instinct here because the
two failure directions are not symmetric: over-matching a bracketed run that is not
really a host sends a fragment of prose through `sanitizeSnapshotURL`, which strips
it and returns it; under-matching leaves a token in a column that is stored in the
clear and outlives the campaign. The pattern's job is to find the boundaries of a
run, not to decide whether the run is a valid URL — `sanitizeSnapshotURL` already
does that, and fails closed when it cannot.

Found by the local pre-PR review's repo_code reviewer on `8073b8cc`. A regression
case for the zone-scoped form is pinned in `TestSanitizeSnapshotText`, verified to
fail against the narrow class.

Concept file updated: `docs/knowledge/code/internal-dispatch.md`.

Refs: LFXV2-2665
