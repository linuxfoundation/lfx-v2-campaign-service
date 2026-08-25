# 2026-08-25 — X/Twitter dispatch-adapter doc sentence repair

**Fix** — The prior nullcast-tweet-authoring commit
([2026-08-25-LFXV2-2665-x-nullcast-tweet-authoring](2026-08-25-LFXV2-2665-x-nullcast-tweet-authoring.md))
spliced its new paragraph about `tweetText`/`asUserId` config mapping into the
middle of the `internal/dispatch` section's existing sentence about
`validateTwitterConnection`, duplicating that sentence's opening clause.
Repaired by giving the new paragraph its own home and letting the
`validateTwitterConnection` sentence read as originally written, in
`docs/knowledge/code/internal-platform-twitter.md`. No documented behavior
changed — this corrects the prose only.
