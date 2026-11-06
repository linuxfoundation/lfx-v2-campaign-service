# 2026-11-05 — Every shared-account Google Ads input requires global_marketing_ops

**Update** — #501 made `googleAdsConfig.demandGenAudience` and each
`demandGenAdGroups[].audience` require `global_marketing_ops` on the path project, because the
Google Ads account is shared by every foundation and `campaign_manager` includes foundation
executive directors. The three other default-off flags rested on the same wrong premise and had
no role check:

- `GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED` (#462, #475): `audienceSegments` on Display,
  `customAudiences`, `customAffinities`, `excludedAudienceSegments`, and the same four lists on
  each `displayAdGroups` entry.
- `GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED` (#461): `performanceMaxAudienceSignal`.
- `GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED` (#477): `sharedNegativeKeywordLists`.

`BriefService.CreateCampaigns` now applies #501's check to all of them.
`service.GoogleAdsMarketingOpsInputs` (renamed from `GoogleAdsDemandGenAudienceRequested`)
returns the gated fields a config sets, in config order, with group entries named by 1-based
position (`displayAdGroups entry 2 customAudiences`). Any non-empty value or list counts,
whitespace included and on any channel, the same rule the dispatch gates use. A request makes
one check however many fields it sets. A denial is `403` with a service-authored message
naming every gated field it set; a check that cannot be completed is `503`.

**Search `audienceSegments` are now gated too.** They are observation-only and predate every
flag, so the Display flag does not cover them, but they name the shared account's user lists.
The check applies to campaign-level `audienceSegments` on every channel and to each
`adGroups[].audienceSegments`. **Behaviour change:** an executive director who is not
`global_marketing_ops` used to be able to attach Search user-list observations and now gets
`403`. No flag controls this, and it takes effect when the release ships.

The service cannot import `internal/dispatch`, so it decodes the field names itself.
`TestSharedAccountInputDetectionAgreesWithTheCreateGate` (`internal/dispatch`) builds the
expected list from the dispatcher's own gate helpers (`googleAdsDisplayAudienceFieldsSet`,
`googleAdsDemandGenAdGroupAudiencesSet` and the single-field checks) plus Search's segments,
and requires the two lists to be equal. The published 403 example is the message for
`demandGenAudience` alone, and `TestForbiddenErrorExampleMatchesService` pins it.

Each of these mutations fails the tests:

- removing the gate call;
- dropping the campaign-level lists, the `displayAdGroups` loop, the `adGroups` loop, the
  Performance Max signal, the shared lists or `excludedAudienceSegments`;
- trimming whitespace before the signal check;
- checking twice;
- admitting on a check error;
- leaving the fields out of the 403;
- checking a create that does not select `google-ads`.

The flags are unchanged and still `"false"` in the chart. Once this ships they can be turned on
in lfx-v2-argocd.
