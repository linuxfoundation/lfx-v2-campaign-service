---
type: "Architecture Doc"
title: "Campaign Service — API & Platform Catalog"
description: "Reference catalog of all campaign endpoints, platform account attributes, and data structures for the Go service."
resource: "docs/api-catalog.md"
---

# Campaign Service — API & Platform Catalog

Reference catalog of all campaign endpoints, platform account attributes, and data structures for the Go service.

See [docs/api-catalog.md](../../api-catalog.md) for full details.

Campaign adoption (`POST .../campaigns/adopt`) is supported on Google Ads, Microsoft Advertising,
Meta, Reddit and X; the catalog's "Adopting a campaign" section tabulates each platform's single
read, accepted id format, absent and adoptable statuses, and how provenance is proved (LFXV2-2665).

An adopted campaign is returned, read back and indexed with an optional `adopted: true` (absent,
never `false`, on every other campaign), and toggle-campaign-status refuses to RESUME it with
`409` `reason=campaign_adopted` — distinct from the reason-less not-provisioned `409` — while
pause still works (lfx-self-serve #3741).
