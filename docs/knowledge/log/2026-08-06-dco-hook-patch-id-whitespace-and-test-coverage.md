# 2026-08-06 — chore/LFXV2-dco-hook: fix review-round findings

**Fix** — `.githooks/commit-msg`'s replay-exemption check used
`git patch-id --stable` to compare an original commit's diff against the
currently-staged diff. `git patch-id` deliberately ignores whitespace and
line-number differences, so a whitespace-only tree amend (message and
Signed-off-by trailer unchanged, content only re-indented/re-wrapped) would
hash to the same patch-id as the original and be treated as a faithful,
unmodified replay — silently skipping the current committer's required
sign-off for content that did in fact change. Replaced the patch-id
comparison with a `diff_fingerprint` helper that hashes `git diff` output
with only genuinely base-dependent lines stripped (`index ...` blob-oid
lines and `@@ ... @@` hunk headers) — this still tolerates the replay
landing on a different base commit (the original design goal), but no
longer normalizes away whitespace changes to the actual +/- content.

**Fix** — `commit-msg.bats`'s "exempts an unmodified rebase replay" test
never actually exercised the exemption's fingerprint-comparison branch:
after `git commit -s`, the index already matched `HEAD` (nothing staged),
so `git diff --cached` was empty and the fingerprint check always failed by
construction — the test passed only via whatever the fallback committer
check happened to do, not via the exemption path it claims to cover. Added
a `git reset --soft HEAD^` before invoking the hook, re-staging the
replayed change in the index so `git diff --cached` matches what an
interactive-rebase `edit` stop actually presents to the hook. Verified by
temporarily removing the `reset --soft` line and confirming the test then
fails (`[ "$status" -eq 0 ]` fails, since the exemption path is no longer
reachable) — the test is now binding.

**Fix** — The "rejects a replay whose message was edited" fixture built its
message with a double-quoted string containing literal `\n\n`, and
`msg_file`'s `printf '%s\n'` does not interpret backslash escapes, so the
file ended up with literal backslash-n characters instead of a real blank
line before the trailer — not the reworded-message-with-trailer shape the
test claims to construct. Changed to an ANSI-C-quoted (`$'...'`) string,
matching the pattern already used elsewhere in this same file (e.g. line 40).

Addresses Copilot's and Cursor Bugbot's findings on chore/LFXV2-dco-hook.
