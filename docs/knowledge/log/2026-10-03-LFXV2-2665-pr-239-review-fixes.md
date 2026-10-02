# 2026-10-03 — PR #239 review fixes: multi-group cascade, keyword-action bound, snapshot and link checks

**Fix** — The five findings from PR #239's review (dealako, Copilot, cursor),
all of them created by the LFXV2-2665 completeness work rather than pre-existing.

**1. An omitted device `bidModifier` silently excluded the device
(`internal/dispatch/googleads.go`).** `googleAdsDeviceBidModifierConfig.BidModifier`
was a plain `float64`, so a `deviceBidModifiers` entry with no `bidModifier`
decoded to `0` — and `0` is not "no adjustment", it is the -100% opt-out that
stops the campaign serving on that device. The caller got a successful create
and a device switched off. `docs/api-catalog.md` already documented the field as
REQUIRED; the code was the half that did not enforce it. Now a `*float64`,
refused when nil with a message that explains what `0` means, and refused in the
mapper hoisted ABOVE the `CampaignInput` literal so the refusal lands before the
budget mutate — a later one strands a paid campaign. An explicit `0` is still
accepted: a caller asking for the opt-out on purpose must get it.

**2. Multi-group campaigns sat outside the status cascade
(`internal/platform/googleads/adgroup_ad.go`, `internal/dispatch/googleads.go`).**
`ToggleStatus` cascaded over the scalar `adGroupId`/`adId` pair, which is a COPY
of the first group's and was the whole story while a campaign could only have one
group. Since multiple ad groups and multiple RSAs landed, ACTIVATE enabled group
1's first ad and left every other group and ad PAUSED while the campaign reported
ENABLED — a campaign that says it is running and mostly is not.

New `AdGroupStatusTarget` (one group with that group's OWN ad ids — nested,
because an adGroupAd resource name is the composite `{adGroupId}~{adId}`, so a
flat ad list paired with a single group id is exactly how group 1's ads get
toggled over and over) and `Client.UpdateAdGroupsAndAdsStatus`, which sends every
group in ONE `adGroups:mutate` and every ad in ONE `adGroupAds:mutate`.

Batching over looping, deliberately: per-group calls would produce a staircase of
partial states that no single error can describe, where two calls keep the failure
surface the same shape as the original single-pair cascade. Batching is also what
MAKES a short response meaningful — one operation per resource means a 2xx with
fewer results covers only some of them — so `checkStatusMutateResults` reports
that as a `partialCascadeError` with `stage` `"ad group"` or `"ad"`, satisfying
`IsOutcomeUnconfirmed`: the groups it covered really did flip, which is "verify
before retrying", not "nothing changed". MORE results than operations is accepted
without complaint; failing a correct toggle over Google reporting extra work is
the over-refusal this guard must not commit. `UpdateAdGroupAndAdStatus` survives
as a thin wrapper with no logic of its own, so the two cannot drift.

Duplicate groups and duplicate `{group}~{ad}` composites are COLLAPSED, not
refused. Google rejects two operations against one resource, and a caller merging
the scalar pair with the `AdGroups` list legitimately repeats the first entry —
the scalars are a copy of it. Refusing would fail a toggle that is perfectly well
specified.

On the dispatcher side, `googleAdsToggleTargets` recovers every group with its own
ads and falls back to the scalar pair for rows written before `AdGroups` existed
(single-group by construction). The ACTIVATE keyword gate became campaign-wide —
any group having a keyword criterion qualifies, because the gate asks whether the
campaign can DELIVER and it delivers if one group has keywords; asking only about
the first refuses a campaign that would have served. A group recorded but not
fully created is logged by name and SKIPPED rather than refusing the whole toggle,
which would strand a campaign that is otherwise ready, while the operator still
learns the state is not uniform.

**3. Keyword actions were refused for every ad group but the first
(`internal/dispatch/googleads.go`).** `ApplyKeywordActions` bounded each action's
`adGroupId` to the scalar `AdGroupID`. Fail-closed, so never unsafe, but it made
keyword actions unusable on exactly the multi-group campaigns the feature was
added for. New `googleAdsCampaignAdGroupIDs` builds the campaign's full ad-group
set from the blob; the guard is unchanged in strength — a group this campaign does
not record is still refused locally, before Google is contacted, which is what
stops a caller holding a criterion id from anywhere in the shared account from
acting on it through a campaign they do own.

That set is deliberately NOT `googleAdsToggleTargets`' list. That one answers
"which groups can take a toggle" and drops a group whose ads never got created;
this one answers "which groups are this campaign's", and a group with keyword
criteria but no ad is still this campaign's — its keywords are real, serving
nothing, and pausing or removing them is a legitimate ask.

**4. The `campaignAsset` link check verified only the campaign
(`internal/platform/googleads/assets.go`).** `campaignAssetID` now returns all
three components of the `{campaignId}~{assetId}~{fieldType}` composite, and the
link-result loop checks each against the operation at the same index. Results come
back in operation order — `createExtensionAssets` already depends on that to pair
ids with `plan.fieldTypes` — so a 2xx reporting `222~700~SITELINK` for the
operation that linked asset 700 as a CALLOUT is the response and the request
disagreeing about what exists. Checking only the campaign id accepted exactly
that. The field type is compared with `strings.EqualFold`: it is Google's own enum
echoed back, so a casing change is a change in spelling, not the wrong link, and
a genuinely different type still fails.

**5. Sitelink URLs were persisted whole into `config_snapshot`
(`internal/dispatch/googleads.go`).** `config_snapshot` is stored unencrypted and
a registration URL pasted from a browser carries whatever query that session put
in it. New `googleAdsSnapshotConfig` reduces each `finalUrl` through the existing
`sanitizeSnapshotURL` (scheme+host, fail-closed on userinfo), wired into both the
create and the adoption call sites — the parity the sweep against `twitter.go`'s
`campaignFromTwitter` should have caught before the PR opened. It deep-copies the
`Sitelinks` slice first: the config is passed by value but the slice shares a
backing array with the caller's, so sanitizing in place would redact the URL the
create path is about to SEND and break the sitelink, a worse defect than the one
being fixed.

`googleAdsChildIDs` was deleted with its test — `googleAdsToggleTargets` and
`googleAdsCampaignAdGroupIDs` between them leave it with no production caller.
