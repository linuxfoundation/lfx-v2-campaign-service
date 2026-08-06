# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT
#
# Exercises .githooks/commit-msg's branching logic in isolation: unsigned
# rejected, signed accepted, merge exempted via MERGE_HEAD, unmodified replay
# exempted, edited-message replay requires fresh sign-off, and a tree-only
# amend (message/trailer unchanged, content changed) also requires fresh
# sign-off since it fails the patch-id comparison.

setup() {
  HOOK="$(cd "$(dirname "$BATS_TEST_FILENAME")" && pwd)/commit-msg"
  REPO="$(mktemp -d)"
  cd "$REPO" || exit 1
  git init -q
  git config user.name "Test Committer"
  git config user.email "test-committer@example.com"
  echo one >file.txt
  git add file.txt
}

teardown() {
  rm -rf "$REPO"
}

msg_file() {
  local f
  f="$(mktemp)"
  printf '%s\n' "$1" >"$f"
  echo "$f"
}

@test "rejects a commit with no Signed-off-by trailer" {
  f=$(msg_file "chore: no sign-off")
  run "$HOOK" "$f"
  [ "$status" -ne 0 ]
  [[ "$output" == *"missing the required DCO sign-off trailer"* ]]
}

@test "accepts a commit with a matching Signed-off-by trailer" {
  f=$(msg_file $'chore: signed\n\nSigned-off-by: Test Committer <test-committer@example.com>')
  run "$HOOK" "$f"
  [ "$status" -eq 0 ]
}

@test "exempts a merge commit via MERGE_HEAD regardless of trailer" {
  git commit -q -m "initial" --no-verify
  echo two >file.txt
  git add file.txt
  git commit -q -m "initial2" --no-verify
  git rev-parse HEAD >.git/MERGE_HEAD
  f=$(msg_file "Merge branch 'main'")
  run "$HOOK" "$f"
  [ "$status" -eq 0 ]
  rm -f .git/MERGE_HEAD
}

@test "exempts an unmodified rebase replay carrying the original author's sign-off" {
  git commit -q -m "initial" --no-verify
  echo two >file.txt
  git add file.txt
  GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" \
    git commit -q -s -m "feat: original change"
  replay_sha=$(git rev-parse HEAD)
  echo "$replay_sha" >.git/REBASE_HEAD

  # Genuinely enter the exemption's patch-id (fingerprint) comparison: soft-reset
  # HEAD back to the parent so the "two" change is re-staged in the index,
  # matching what `git diff --cached` sees at an interactive-rebase stop right
  # before the replayed commit is made. Without this, the index already matches
  # HEAD (nothing staged), `git diff --cached` is empty, the fingerprint check
  # fails, and the test would only pass via the fallback committer-check path —
  # never actually exercising the exemption this test claims to cover.
  git reset --soft HEAD^

  GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" \
    f=$(msg_file "$(git log -1 --format=%B "$replay_sha")")
  run env GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" "$HOOK" "$f"
  [ "$status" -eq 0 ]
  rm -f .git/REBASE_HEAD
}

@test "rejects a replay whose message was edited without a fresh sign-off" {
  git commit -q -m "initial" --no-verify
  echo two >file.txt
  git add file.txt
  GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" \
    git commit -q -s -m "feat: original change"
  replay_sha=$(git rev-parse HEAD)
  echo "$replay_sha" >.git/REBASE_HEAD

  f=$(msg_file $'feat: reworded during rebase\n\nSigned-off-by: Original Author <original@example.com>')
  run env GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" "$HOOK" "$f"
  [ "$status" -ne 0 ]
  rm -f .git/REBASE_HEAD
}

@test "rejects a tree-only amend replay (message/trailer unchanged, content changed)" {
  git commit -q -m "initial" --no-verify
  echo two >file.txt
  git add file.txt
  # Original author == original committer, so the replay exemption's author-trailer
  # check matches the message's actual Signed-off-by trailer (the realistic case:
  # the original commit was properly self-signed).
  GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" \
    GIT_COMMITTER_NAME="Original Author" GIT_COMMITTER_EMAIL="original@example.com" \
    git commit -q -s -m "feat: original change"
  replay_sha=$(git rev-parse HEAD)
  echo "$replay_sha" >.git/REBASE_HEAD

  # Simulate an interactive-rebase `edit` stop where the tree is amended but the
  # message (and its Signed-off-by trailer) is carried through unchanged, and the
  # author identity is preserved (as `git commit --amend --no-edit` does by
  # default) — this is exactly the case the patch-id check exists to catch, since
  # a message-only comparison would wrongly treat it as a faithful replay. The
  # entity performing the amend (e.g. a rebase bot) is a different COMMITTER than
  # the original author, so if the (broken) message-only exemption fired, this
  # would wrongly pass with no sign-off from the entity that actually produced
  # this content; only the patch-id mismatch correctly forces the fallback
  # committer check, which then fails for lack of a fresh trailer from THIS
  # committer.
  echo three >file.txt
  git add file.txt

  f=$(msg_file "$(git log -1 --format=%B "$replay_sha")")
  run env GIT_AUTHOR_NAME="Original Author" GIT_AUTHOR_EMAIL="original@example.com" \
    GIT_COMMITTER_NAME="New Committer" GIT_COMMITTER_EMAIL="new-committer@example.com" \
    "$HOOK" "$f"
  [ "$status" -ne 0 ]
  [[ "$output" == *"missing the required DCO sign-off trailer"* ]]
  rm -f .git/REBASE_HEAD
}
