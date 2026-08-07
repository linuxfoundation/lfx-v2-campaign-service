# 2026-08-07 — GA-4: dead audience branch removed, stale ACTIVATE contract corrected

**Update** — Closed the remaining suppressed Copilot findings on PR #69
(`internal/platform/googleads/targeting.go`, `internal/platform/googleads/campaign.go`,
`internal/dispatch/googleads_test.go`, `docs/knowledge/log/2026-08-05-ga4-review-fixes.md`).

Removed `customAudienceInfo`, the `CustomAudience` field on `adGroupCriterionCreate`, and the
`case "customAudience"` arm in `createAdGroupTargeting`. `validateAudienceSegments` rejects
every `customAudiences` resource name before a mutate is ever built — SEARCH campaigns, the
only kind this client creates, do not support Custom Audiences — so the arm was unreachable and
the payload advertised a targeting shape the client can never populate. The `switch` it lived in
had a second problem beyond dead code: for an unrecognized field it fell through leaving NO
oneof set, producing a criterion Google rejects with a 4xx that arrives only after the budget,
campaign, ad group and ad already exist. The loop now assigns `UserList` unconditionally, which
is the only shape that can reach it.

`audienceCriterionField` still recognizes `customAudiences`, deliberately: it is what lets
`validateAudienceSegments` reject the name with its real reason rather than the generic
unrecognized-resource-name error, which would send a caller hunting for a typo in a perfectly
well-formed name. Its GoDoc now says so, and also drops the association with
`docs/api-catalog.md`'s `campaign_audiences` resource — that resource's `platform` enum is
`hubspot` only (`design/audience.go`), so it holds HubSpot master-list pointers which can never
appear as a Google Ads criterion. The comment was directing maintainers to a table that cannot
contain these ids.

`UpdateCampaignStatus`'s GoDoc still described the GA-3c world: "ACTIVATE is deferred until GA-4
provisions targeting criteria" and "today the dispatcher rejects ACTIVATE unconditionally". GA-4
is this slice. The dispatcher now cascades ACTIVATE children-first, campaign-last, and refuses
only a campaign genuinely missing its ad group/ad ids or its keyword criteria. Both paragraphs
corrected.

The same staleness had settled into the tests. `provisionedGoogleAdsCampaign` returns a blob with
`adGroupId`/`adId` but no `keywordCriteriaIds` — by GA-4's guard it is precisely NOT provisioned,
so the name asserted the opposite of what the fixture is for. Renamed to
`googleAdsCampaignWithChildrenNoTargeting`. Two tests named `..._Refused` claimed to verify that
"ACTIVATE is refused in GA-3c until GA-4 provisions targeting"; with GA-4 present they were
testing something real but describing something obsolete. Retargeted to what they actually pin:
`ActivateWithoutKeywordCriteriaIsNotProvisioned` (the guard's second condition — children alone
are not enough) and `ActivateGuardRunsBeforeAnyMutate` (the guard's ordering — the dispatcher in
that test has no client options, so a guard checked after the first mutate would surface a
connection error instead of `ErrCampaignNotProvisioned`).

The general rule, since this is the third comment on this branch to describe a superseded
lifecycle: **a slice that lands the capability an earlier slice deferred must sweep the
deferrals, not just the code.** The deferral text lives in GoDoc, in test names, and in fixture
helper names, and every one of them keeps reading as current long after it stops being true.

Also converted the seven `**Fix** —` markers in
`docs/knowledge/log/2026-08-05-ga4-review-fixes.md` to `**Update** —`, the marker `CLAUDE.md`
requires after a log fragment's H1.
