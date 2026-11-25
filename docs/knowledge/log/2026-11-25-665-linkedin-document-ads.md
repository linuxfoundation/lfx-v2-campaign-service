# 2026-11-25 — #665 LinkedIn document ads and PDF creative assets

**Update** — The creative-asset store admits `application/pdf` (migration 000059): the upload is
checked structurally (`%PDF-` header, `%%EOF` in the last 1024 bytes), stored without dimensions,
never served at a signed URL, and refused by every image and video consumer before any platform
request. `linkedInConfig.format` takes `"document"` with `documentAssetId`, `documentTitle` (at
most 70 characters) and `documentIntroText` (at most 600), behind `LINKEDIN_ADS_DOCUMENT_ENABLED`
(off by default). Decision 2026-11-25: the campaign is ENGAGEMENT (MAX_REACH, CPM, automatic) by
default, LEAD_GENERATION with a Lead Gen Form when `objective: "lead_generation"` is sent, and
Website Visits / Conversions are refused. It is created without a campaign format and named
`Document`; the PDF goes through the Documents API, then one dark post (`content.media`) and one
creative. With the flag off, or any refused shape, the create is a 400 before a job exists, and
Dispatch repeats every check. Not yet created on a live account; the paused probe is in the PR.
This closes #665.
