# 2026-08-06 — chore/LFXV2-dco-hook: fix new Cursor Bugbot findings on the fingerprint fix

**Fix** — `diff_fingerprint`'s filtering pipe (`grep -Ev '^(index |@@ )'`) exits
1 whenever nothing matches, which happens whenever the diff itself is empty —
e.g. `git diff --cached` at a pure `reword` rebase stop, where the tree is
untouched and nothing is staged. Under `set -euo pipefail`, an unguarded grep
exit 1 inside a `var=$(...)` assignment aborts the whole hook before the
committer-sign-off fallback ever runs, rejecting even a correctly-signed
reword with no hook error text. Wrapped the filtering stage in `|| true` so an
empty diff yields a well-defined empty-blob fingerprint instead of aborting.

**Fix** — The same filtering pipe kept unchanged context lines in the hash,
only stripping `index`/`@@` header lines. A replay landing on a different base
commit can shift the context surrounding an otherwise-identical change (the
scenario `git patch-id`'s tolerance was relied on for), which would flip the
fingerprint and lose the replay exemption even though the actual patch content
didn't change. Changed the filter to keep only `+`/`-` content lines (excluding
the `+++`/`---` file-header lines), dropping context lines from the hash while
still hashing the actual changed content — preserving cross-base tolerance
without reintroducing patch-id's whitespace-insensitivity.

**Verification** — added `commit-msg.bats`'s "exempts a reword-only replay
with an empty staged diff" test; confirmed it fails against the pre-fix
`grep -Ev '^(index |@@ )'` pipe (`[ "$status" -eq 0 ]` fails, since the
unguarded grep exit 1 aborts the hook) and passes with the `|| true` fix
restored.

Addresses Cursor Bugbot's findings on chore/LFXV2-dco-hook after the prior
`diff_fingerprint` fix (commit 62d0244f).
