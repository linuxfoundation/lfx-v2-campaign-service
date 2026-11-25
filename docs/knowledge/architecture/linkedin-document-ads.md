---
type: "Architecture Doc"
title: "LinkedIn Document Ads"
description: "How a LinkedIn dispatch creates document ads behind LINKEDIN_ADS_DOCUMENT_ENABLED: linkedInConfig.format document with documentAssetId (a stored PDF of the brief), documentTitle and documentIntroText, an ENGAGEMENT campaign optimising for reach by default or a gated LEAD_GENERATION one with a Lead Gen Form, created without a format and named Document, the PDF uploaded once through the Documents API, then one dark post carrying content.media and one creative."
resource: "internal/platform/linkedin/documents.go"
---

# LinkedIn Document Ads

Sponsor prospectus and report ads on LinkedIn (#665, split from #552 gap 9), the second half of
#665 after [LinkedIn Carousel Sponsored Content](linkedin-carousel-ads.md). Code:
`internal/service/creative_asset_document.go` (PDF upload check), `internal/dispatch/linkedin_document.go`
(gate, request shape, objective, resolve + validate) and `internal/platform/linkedin/documents.go`
(limits, the Documents API calls, the post and the creative, run from `CreateCampaign`).

## Storing a PDF

The creative-asset store admits `application/pdf` since migration 000059. The upload is checked
structurally, not rendered: a `%PDF-` header with a 1.x or 2.x version, and the file must end with
its final `%%EOF` (within the last 1024 bytes, followed only by CR, LF, space, tab or NUL), so an
incremental update cut off after an earlier revision's marker is refused. A PDF has no width or height. Every image consumer (Meta, Microsoft, X, Google, Reddit,
LinkedIn) refuses an asset that is not PNG or JPEG through `model.IsImageMimeType`, every video
consumer one that is not MP4, and the signed asset route never serves a PDF. The upload body cap's
worst case is now the `application/pdf` envelope (41,943,085 bytes), still under 42 MiB.

## Request shape (opt-in)

```jsonc
"linkedInConfig": {
  "format": "document",
  "documentAssetId": "<uuid>",                         // a stored PDF of the brief
  "documentTitle": "KubeCon 2099 sponsor prospectus",  // at most 70 characters
  "documentIntroText": "Sponsor KubeCon 2099",         // at most 600 characters
  "objective": "lead_generation", "leadGenForm": "123" // optional: a gated document
}
```

`LINKEDIN_ADS_DOCUMENT_ENABLED` must be exactly `"true"` (chart default `"false"`); off, the create is
a synchronous 400 naming the flag. `PreflightCreate` and `Dispatch` both run `linkedinDocumentFormat`
before any LinkedIn request.

## Objective (decision 2026-11-25)

Document ads are listed under Brand Awareness, Engagement and Lead Generation in LinkedIn's
Campaign Objectives, not Website Visits or Conversions. A document create is **ENGAGEMENT** by
default (`MAX_REACH`, `CPM`, automatic only, name segment `Engagement`); `objective: "lead_generation"`
with a Lead Gen Form makes it `LEAD_GENERATION` / `MAX_LEAD` with the form on the creative (the
lead gen flag and `global_marketing_ops` still apply). `"traffic"`, `"conversions"` and a manual bid
are refused. The client refuses ENGAGEMENT for any other format.

## What is created

1. A `SPONSORED_UPDATES` campaign **without** `format` ("If no Ad format is set, it's set by the first
   creative"); name segment `Document`. The reuse lookup expects LinkedIn to report
   `SPONSORED_UPDATE_NATIVE_DOCUMENT` and refuses a same-name campaign of another format.
2. The PDF: `POST /rest/documents?action=initializeUpload` (organization owner), the PUT to the
   LinkedIn upload URL, `GET /rest/documents/{urn}` until AVAILABLE within a shared 60 s wait.
3. ONE dark post with `content.media {title, id: urn:li:document:…}` and no landing page, so a
   document create skips the registration URL check every other format applies.
4. ONE DRAFT creative named `<event> | document-1`, with `leadgenCallToAction` on a gated document.

The minted URN is recorded (`documentUrns`); a failure after the campaign exists returns the partial
(claim retained), and an ambiguous post or creative failure is UNCONFIRMED. The snapshot keeps the
canonical `documentAssetId`, which the asset-deletion guard reads (fourth snapshot key).

## Not yet verified

No live LinkedIn call has been made. Owed before enabling: one paused engagement create and one
gated create with a stored PDF, confirming the campaign without `format` under ENGAGEMENT /
MAX_REACH, the upload reaching AVAILABLE, the post's `content.media`, the creative (and its form),
and whether a gated document needs a preview page count; then archive both.
