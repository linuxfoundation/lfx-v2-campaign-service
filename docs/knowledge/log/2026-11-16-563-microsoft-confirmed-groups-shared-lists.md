# 2026-11-16 — #563 Microsoft shared lists need PartialErrors

**Fix** — A #613 review finding. `AttachCampaignSharedNegativeKeywordLists` now requires `PartialErrors` in a 200 response. `null` or `[]` still means no error, but a body that omits the field (`{}`) marks every list UNCONFIRMED, so the campaign is `created_degraded` instead of every list being recorded APPLIED.
