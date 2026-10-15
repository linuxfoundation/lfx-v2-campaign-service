# 2026-10-15 — #245 review: LinkedIn unselected-profile facets are redacted too

**Fix** — `linkedinSnapshotConfig` had kept `targetingProfiles[].skills`/`groups` verbatim on
the grounds that `validFacets` refuses anything but a facet URN. That holds only for the profile
`targetingProfile` selects: `validatePrerequisites` skips every other entry, so an unselected
profile's facets reached `config_snapshot` as typed. Both lists now go through
`sanitizeSnapshotTexts`, which leaves a real facet URN unchanged. The doc comment and the
`internal-dispatch` concept now say which facets are validated. The LinkedIn snapshot test adds
an unselected profile carrying reset links in `skills` and `groups`. Dropping either sanitize
call fails the test.
