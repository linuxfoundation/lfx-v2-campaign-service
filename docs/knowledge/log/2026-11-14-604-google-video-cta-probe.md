# 2026-11-14 — #604 Google video call-to-action values are the probed set

**Fix** — The Demand Gen video ad's call to action (`DemandGenVideoCreative.CallToAction`, a
CallToActionType enum sent on a CALL_TO_ACTION asset) was limited to SIGN_UP, LEARN_MORE,
BOOK_NOW, VISIT_SITE and WATCH_NOW. That list predates #575's probe and was never re-probed.

- Probe: validateOnly `googleAds:mutate` on 2026-11-14 (v23, account 8666746580), one request
  per value, each the same shape `createDemandGenVideoAd` sends: a `callToActionAsset` created
  under a temporary asset name, then an `adGroupAdOperation` whose ad references it. The ad
  copied an existing Demand Gen video ad's content and named the PAUSED ad group 201415844146
  ("ArgoCon - Display") of the PAUSED test campaign 24441656372. Every request carried
  `validateOnly: true`, so nothing was created. A control with no call to action was accepted.
- Accepted: SIGN_UP, LEARN_MORE, BOOK_NOW, VISIT_SITE, WATCH_NOW, APPLY_NOW, CONTACT_US,
  DOWNLOAD, GET_QUOTE, SUBSCRIBE, SHOP_NOW, and the lower-case names sign_up, Sign_Up and
  learn_more.
- Refused with 400 "Invalid value" for `CallToActionTypeEnum.CallToActionType`: "Sign up",
  "SIGN UP", "Learn more". The field is an enum, not free text like #575's `callToActionText`.
- `demandGenVideoCallToActions` (`demandgen_video_creative.go`) now holds all eleven accepted
  values. Nothing was dropped. The match stays exact on the canonical spelling, so a
  lower-case name is still refused here even though Google takes it.
- `docs/api-catalog.md` lists the eleven values.
- Tests: `TestDemandGenVideoCallToActions_AreTheProbedSet` (fails on the old list), plus a
  "label-spelling CTA" refusal case in the video creative refusal table.
- Not covered: the Video channel's `videoResponsiveAd` `callToActions` text assets, which are
  free text on a channel the API cannot create.
