---
type: "Architecture Doc"
title: "Reddit Custom Audiences"
description: "Reddit retargeting and suppression: redditConfig.customAudienceIds and excludedCustomAudienceIds sent as the ad group's targeting.custom_audience_ids and excluded_custom_audience_ids on every ad-group attempt, and a picker read of the ad account's custom audiences; both built from Reddit's public v3 OpenAPI document, off unless REDDIT_CUSTOM_AUDIENCES_ENABLED, and reserved to global_marketing_ops because the audiences live on the LF ad account every foundation shares."
resource: "internal/dispatch/reddit_custom_audiences.go"
---

# Reddit Custom Audiences

Gap 1 of the Reddit Ads gap review (#549). Before this, a Reddit campaign labelled
`Intent retargeting` (multi-campaign cs-3) carried the label and nothing else: the ad group
never named an audience, so it served to everyone the communities, keywords and geo reached.

## Create inputs

`redditConfig.customAudienceIds` (target) and `excludedCustomAudienceIds` (exclude) are lists
of Reddit custom audience ids. The client (`internal/platform/reddit/client.go`) sends them as
the ad group's `targeting.custom_audience_ids` and `targeting.excluded_custom_audience_ids`,
the members `components_schema_targeting` defines.

- **They are in `baseTargeting`, the retry payload.** When Reddit refuses the communities or
  interests, the single fallback retry drops those two and keeps the audiences. Dropping a
  retargeting audience would widen the campaign to everyone, and dropping a suppression list
  would serve to the people it exists to exclude.
- **Rules, before anything is created** (`reddit.ValidateCustomAudienceIDs`): each id 1-64
  letters, digits or underscores, sent exactly as given (not trimmed); at most 100 per list;
  no repeats; no id in both lists. Reddit's schema only says "string", with the example
  `67547345732543`, so the rule is the account-id charset rather than digits only.
- **Where they are refused.** `RedditDispatcher.PreflightCreate` refuses a wrong JSON type
  (every case variant of the key, as encoding/json reads it), a rule break or the flag off with
  a synchronous 400 `invalid campaign config: redditConfig.<field>: ...`. `Dispatch` repeats
  the same checks before any Reddit call, as a `domain.PreflightRefusal` the job reports
  verbatim, and the client checks the ids once more before its account-verify GET.
- **Role.** The audiences are resources of the LF Reddit ad account, which every foundation
  shares, and `campaign_manager` includes each foundation's executive directors. So a
  reddit-ads create that sets either list needs `global_marketing_ops` on the project
  (`service.RedditMarketingOpsInputs`, checked in the same single check as the Google inputs,
  403 / 503). `TestEveryRedditConfigFieldIsClassifiedForTheMarketingOpsGate` makes every new
  `redditConfig` field choose gated or ungated.
- **Keyword levers keep them.** The keyword removal and negative-keyword add PATCH the whole
  targeting object and send back every member as read, so the audience members survive them.

## Picker read

`GET /projects/{project_id}/reddit-ads/custom-audiences` (`list-reddit-ads-custom-audiences`)
reads Reddit `GET /ad_accounts/{ad_account_id}/custom_audiences` on the account a create for
the project uses. It follows `pagination.next_url` through the client's `nextPagePath`, which
keeps the walk on the same origin and resource, for at most 10 pages, and reports `truncated`
when more remained. Any answer it cannot believe fails the read with 503 and nothing partial.
It lists every status (`VALID`, `PENDING`, `NOT_ENOUGH_MATCHES_ERROR` and so on), so a picker
can show why an audience is not usable yet. The route is in the gateway's `marketing-ops-api`
rule (`global_marketing_ops`); `parity_test.go` pins it there.

## Deployment gate

`REDDIT_CUSTOM_AUDIENCES_ENABLED`, default off. Off, a create naming an audience is refused
with 400 and the read answers 400 "not supported". Nothing here has been exercised against a
live Reddit account. Turn it on in lfx-v2-argocd only after a live PAUSED create confirms
Reddit accepts both targeting members and a read confirms the list envelope.
