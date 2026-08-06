# 2026-08-06 — chore/LFXV2-dco-hook: fix relocation collision and external-diff bypass

**Fix** — `diff_fingerprint` dropped all unchanged context lines, keeping only
file identity and `+`/`-` content lines. That meant two edits of identical
content at *different* locations within the same file (e.g. a line reading
`one` appearing twice, changed to `two` at one occurrence in the original
commit and at the other occurrence in a modified replay) reduced to the same
filtered stream and fingerprinted identically, wrongly granting the replay
exemption for a genuinely different change. Switched `git diff` to `-U1` (1
line of context) and changed the awk filter to keep context lines (`^ `)
alongside `+`/`-` lines, restoring enough surrounding-content signal to tell
same-content-different-location edits apart in the common case. `@@ ... @@`
hunk headers are still normalized to a bare `@@` marker (position and counts
dropped) to preserve the existing cross-base-rebase tolerance. When even the
1-line context is identical at both locations, the fingerprint still
correctly fails closed (denies the exemption) rather than guessing.

**Fix** — Added `--no-ext-diff` to the `git diff` invocation. Without it, a
`diff.external`/`GIT_EXTERNAL_DIFF` driver configured on the machine can
replace the diff *body* while git still emits the ordinary `diff --git`
header ahead of it; the awk filter then keeps the (unchanged) file-identity
line but silently loses the swapped-in body, so a same-file content amend
during a rebase `edit` stop can hash identically to the original regardless
of what actually changed. `--no-ext-diff` pins the hook to git's built-in
diff engine regardless of local config.

**Fix** — Wrapped `diff_fingerprint`'s full pipeline in `|| true`. Under
`set -o pipefail` (part of this hook's `set -euo pipefail`), a fatal `git
diff` failure — a real error, not just "differences found": plain `git diff`
without `--exit-code` only returns nonzero on an actual error — would
otherwise abort the whole hook via the `var=$(...)` assignment at the call
site, before the committer-sign-off fallback ever runs. On failure this now
yields an empty fingerprint, which the existing `-n "$original_fingerprint"`
check already treats as "not a proven replay" — failing closed to requiring a
fresh sign-off, never open.

**Verification** — added `commit-msg.bats`'s "rejects a modified replay that
edits identical content at a different location in the same file" and
"ignores a diff.external/GIT_EXTERNAL_DIFF driver and still distinguishes
different content" tests. Confirmed both fail against the pre-fix filter
(context-free, no `--no-ext-diff`) and pass with the fix restored; all 10
tests pass.

Addresses Cursor Bugbot's and Copilot's findings on chore/LFXV2-dco-hook
after the prior `diff_fingerprint` fixes (commits 50338c18, 62d0244f).
