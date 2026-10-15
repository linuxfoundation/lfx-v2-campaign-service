# 2026-10-15 — #245: LinkedIn, Meta and Reddit snapshots redact caller free text

**Fix** — `config_snapshot` is stored UNENCRYPTED and indexed, and the LinkedIn, Meta and Reddit
create paths stored operator-authored ad copy in it unredacted. LinkedIn persisted the raw
`linkedinConfig`, so every variant's `IntroText`/`Headline` went in as typed. Meta reduced only
`ImageURL`, leaving `PrimaryText`/`Headline`/`Description`. Reddit reduced `PostURL`/`ImageURL`
but not the variants' `Headline`/`Body`. A password-reset link pasted into ad copy keeps its
secret in the path, so it reached the column whole. X and Microsoft already ran their free text
through `sanitizeSnapshotText`.

- New builders `linkedinSnapshotConfig`, `metaSnapshotConfig` and `redditSnapshotConfig`,
  following `microsoftSnapshotConfig`'s rule. Every caller string the client does not
  shape-check goes through `sanitizeSnapshotText`. That covers the ad copy plus labels,
  selectors, keywords, and fields validated only when used. URLs still go through
  `sanitizeSnapshotURL`. Fields the client refuses unless they are a URN, enum or numeric id stay
  verbatim. Each builder's doc comment lists both sets.
- New `sanitizeSnapshotTexts` in `creds.go`: the list form, keeping nil and empty distinct.
- Every rewritten slice is reallocated first, so the config sent to the platform is unchanged.
- `snapshot_free_text_test.go` builds each snapshot through `campaignFromLinkedIn`/`Meta`/`Reddit`.
  Every field gets its own marker carrying a scheme-ful reset link (path, query, fragment) and a
  scheme-less one. The tests assert that no marker survives into the snapshot and that the
  caller's config still holds the full value. Removing any builder call, any single field's
  sanitize, the LinkedIn slice reallocation or the `sanitizeSnapshotTexts` reallocation fails
  the tests.
- Rows written before this fix are not backfilled.
