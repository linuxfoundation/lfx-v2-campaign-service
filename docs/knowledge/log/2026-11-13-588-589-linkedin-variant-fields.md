# 2026-11-13 — #588 #589 LinkedIn variant name, call to action and description

**Update** — each `linkedInConfig.variants[]` entry (`linkedin.CreativeVariant`) takes three
optional strings. Omitted, each sends what was sent before.

- `name` (#589) names the creative (whitespace collapsed, trimmed, cut to 255; dashes kept) in
  place of `<event> | variant-N`. Two variants whose names match once normalized are refused.
- `callToAction` (#588) is sent on the dark post as `contentCallToActionLabel` with
  `contentLandingPage` = the variant's UTM URL. One of the 13 Posts API labels at 202602
  (`linkedin.CallToActionLabels`), exact spelling. Behind `LINKEDIN_ADS_CALL_TO_ACTION_ENABLED`,
  off by default.
- `description` (#588) is the article description, normalized like `introText` and cut to 300.
- A wrong JSON type, an unknown label, a repeated name, or a label while the gate is off is a
  synchronous 400 from `PreflightCreate`; `CreateCampaign` re-checks before the campaign group.
  The snapshot redacts links in `name` and `description`.

Built from the LinkedIn Marketing API docs (li-lms-2026-02). Not yet probed live.
