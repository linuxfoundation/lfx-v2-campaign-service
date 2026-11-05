# 2026-11-04 — #468 closed as won't do; the Google Ads audience and shared-list gates described as deployment opt-ins

**Update** — #468 (foundation-scoped ownership for account-scoped Google Ads inputs) was closed
as won't do by the paid-marketing owner. The `campaign_manager` relation is held only by the
internal LF marketing team, which manages every foundation, so per-foundation scoping would
protect no one. That relies on `campaign_manager` being held only by LF marketing; revisit the
gates if foundation-level campaign managers are introduced. The text around the four Google Ads
gates still said they waited "until #468 lands" or until an input "can be checked against the
calling foundation". That is no longer true, so the text now says what holds.

The four flags stay, and their behaviour is unchanged: `GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED`,
`GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED`, `GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED` and
`GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED`. Each is a per-deployment opt-in, off by default,
that ops turns on by setting it to exactly `"true"` in lfx-v2-argocd (lfx-v2-argocd#2001 enables
the Demand Gen one). The referenced resource is checked against the connected Google Ads account
(same customer id), and that account is the boundary.

The refusal callers see, built by `googleAdsInputDisabled`, drops the #468 rationale and says
what to do instead: `google-ads: <fields> is disabled on this deployment (<FLAG> is not "true");
omit <fields>; or ask an operator to set <FLAG> to "true"`. The two remedies are split by `;`
because a Display refusal joins several fields with `, `. The helper's unused `checked` argument
went with it. Rewritten: the constants' doc comments, the chart's values comments, the README,
the deployment and dispatch/googleads KB concepts, `docs/api-catalog.md`, and the client and
dispatch comments. The six gate tests now assert the new sentence, and
`TestGoogleAds_DisplayAudiencesGate` gains a two-field case that pins the whole joined sentence.
Earlier log entries that describe the gates as waiting for #468 stay as written.
