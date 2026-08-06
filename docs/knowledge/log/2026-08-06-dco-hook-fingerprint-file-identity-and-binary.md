# 2026-08-06 — chore/LFXV2-dco-hook: fix file-identity/binary loss in the fingerprint

**Finding** — `diff_fingerprint`'s filtering pipe kept only `+`/`-` content
lines, dropping the `diff --git a/x b/y` file-identity line, mode/rename
metadata (`old mode`/`new mode`/`new file mode`/`deleted file mode`/
`similarity index`/`rename from`/`rename to`/`copy from`/`copy to`), and
binary-patch bodies entirely. A replay that moved the same textual change to
a different file, or a mode-only or binary-only change, would fingerprint
identically to the original (or to an empty diff) and wrongly qualify for the
replay-exemption's sign-off skip.

**Fix** — Switched to `git diff --binary` (so `GIT binary patch` bodies are
present in the diff to filter) and rewrote the filter as an `awk` script that
preserves file identity, mode/rename metadata, and binary bodies verbatim,
while still normalizing away only what legitimately varies across a replay
landing on a different base commit: blob-oid `index` lines, `@@` hunk-header
line numbers, and unchanged context lines. Switched `grep` to `awk` for the
filtering itself: `grep` exits 1 when nothing matches (an empty diff), which
under `set -euo pipefail` aborts the hook via the `var=$(...)` assignment
before the committer-sign-off fallback ever runs; `awk` exits 0 even when it
prints nothing.

**Verification** — added `commit-msg.bats`'s "rejects a modified replay that
moves the same content change to a different file" test. Constructing it
surfaced a second, pre-existing gap: `git commit -s` stamps the *committer*
identity into the trailer, not the author, so a test author-overridden via
`GIT_AUTHOR_NAME` only (as the existing "exempts an unmodified rebase replay"
test does) produces a trailer for the *committer* identity that `setup()`
configures — the same identity that later runs the hook — so the fallback
committer-sign-off check trivially accepts the replay regardless of the
fingerprint comparison, without ever exercising the exemption path. Fixed by
also setting `GIT_COMMITTER_NAME`/`GIT_COMMITTER_EMAIL` on the original
commit (mirroring the existing "tree-only amend" test's pattern), so its
trailer belongs to an identity distinct from the hook's runtime committer.
Confirmed the new test fails (`[ "$status" -ne 0 ]` fails) against the
pre-fix `grep -Ev '^(\+\+\+|---) '`-only filter, and passes with the `awk`
fix restored; all 8 tests pass.

Addresses Cursor Bugbot's and Copilot's findings on chore/LFXV2-dco-hook
after the prior `diff_fingerprint` fix (commit 50338c18).
