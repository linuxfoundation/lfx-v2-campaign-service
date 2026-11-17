# 2026-11-17 — #564 000057 down migration restores a plain CHECK

**Fix** — from the CI failure on PR #614 (`TestLiveMigrationsGoDownAndUpAgain`):

- `000057_creative_assets_video_mp4.down.sql` re-added the image-only `creative_assets_mime_type_check`
  as `NOT VALID`, but version 56 has it as a plain CHECK, and the live migration test requires each
  down file to be the exact inverse of its up. The down file now restores the plain, validated CHECK.
- Behaviour change: a rollback of 000057 now refuses while any `video/mp4` row is stored, instead of
  keeping those rows unchecked. That is the safer outcome, since a rollback never deletes an asset or
  leaves rows the restored schema does not admit; remove the video rows first. The file comment says so.
