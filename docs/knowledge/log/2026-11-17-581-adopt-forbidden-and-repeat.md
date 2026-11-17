# 2026-11-17 — #581: adopt-campaign 403 on the wire; an exact repeat behind a newer version

**Fix** — Two PR #630 review findings on adopt-campaign's `account_id` path.

- A caller without `global_marketing_ops` got a 500, not the documented 403: the service returned
  `ForbiddenError`, but `adopt-campaign` never declared `Forbidden` in `design/brief.go`, so Goa's
  generated encoder had no case for it and fell through to the generic fault. The method now
  declares `Forbidden` and maps it to `StatusForbidden`, as `create-campaigns` does.
  `TestAdoptCampaignHandler_DeniedAccountIDIs403` drives the denial through the generated handler.
- The exact-repeat check read `GetCampaignByPlatform`, which answers only the slot's LATEST live
  version. After adopting 312 and creating 313 with `new_version`, repeating the 312 adoption
  answered 409 though 312 was still bound to the brief. The check now searches every live campaign
  on the brief (`ListCampaignsForBrief`) for the same platform, campaign id and account.
  `TestAdoptCampaign_RepeatFindsTheAdoptedRowBehindANewerVersion` pins it.
