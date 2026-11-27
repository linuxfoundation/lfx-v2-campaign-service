# 2026-11-26 — #666 LinkedIn targeting update lever after create

**Update** — `PATCH /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/targeting`
(`update-campaign-targeting`) replaces a live LinkedIn campaign's `targetingCriteria`, gap 10 of the
LinkedIn gap review (#552).

- The body is the whole audience: `targeting` (`linkedInConfig.targeting`'s shape and rules) and
  `geo_targets`. LinkedIn's `PARTIAL_UPDATE` `$set` replaces the criteria wholesale, so there is no
  delta form.
- Built by the create's builder (#557): geo clause, include clauses, exclude with the row's recorded
  `employerExclusions` merged. When no clause names `interfaceLocales` the update adds
  `urn:li:locale:en_US`, the locale every create sends.
- Segment facets need `global_marketing_ops`. Adopted rows, rows without a config snapshot, and
  campaigns LinkedIn reports terminal or under another account are refused 409 with nothing sent.
- On success the row's `config_snapshot` records the sent `targeting` and `geoTargets`.
- Behind `LINKEDIN_ADS_TARGETING_UPDATE_ENABLED`, off by default. Built from the LinkedIn Marketing
  API docs (li-lms-2026-10). Not yet probed live.
