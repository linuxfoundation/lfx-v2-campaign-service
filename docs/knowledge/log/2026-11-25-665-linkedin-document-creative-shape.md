# 2026-11-25 — #665 document ad creative content shape

**Fix** — A LinkedIn document ad's creative now sends `content.documentAd.reference` instead of
`content.reference`, per the li-lms-2026-11 Document Ads API ("For Document Ads, the content field
requires a documentAd object"). `createDocumentCreative` and `createCreative` share `postCreative`,
so the DRAFT status, the Lead Gen Form on a gated document and the missing-id handling are
unchanged. `gatedLeadgenPreviewPageCount` is not sent (LinkedIn's default preview). Found in PR #683
review; the feature is still behind `LINKEDIN_ADS_DOCUMENT_ENABLED` with the live probe owed.
