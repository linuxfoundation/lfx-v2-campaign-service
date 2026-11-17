# 2026-11-17 — #550 Meta targeting ids with a leading zero are refused at preflight

**Fix** — A Meta targeting id with a leading zero (`behaviors: ["06002714895372"]`) is now a
synchronous 400 naming the field. Before, `normalizeNumericIDs` accepted any `numericIDRE`
string, so `NormalizeAudienceTargeting` and `PreflightCreate` passed the config, and
`checkDetailedTargeting` then failed to encode the id as a `json.Number` (`invalid number
literal`). The create became a failed job instead of a 400. Found by lfx-one review on PR #612.

- `normalizeNumericIDs` (`internal/platform/meta/targeting.go`) now checks each id against
  `canonicalCampaignIDRe` (`^[1-9][0-9]*$`, from `campaign_ref.go`) instead of `numericIDRE`.
  No new regex was added. The refusal reads `<field> entry "<id>" is not a <kind> (a numeric
  string without a leading zero)`.
- Lists covered, since every one goes through `normalizeNumericIDs`: `behaviors` and
  `workPositions` (sent as `json.Number`), and `interests`, `regions`, `cities`,
  `customAudiences` and `excludedCustomAudiences`. Those five are compared as strings against
  Meta's canonical answers, so a leading-zero spelling could never match and would also have
  ended as a failed job. `"0"` on its own is refused too.
- `locales` (ints) and `userOs` (an enum) do not go through this function and are unchanged.
- Tests: `TestMeta_PreflightCreate_RefusesLeadingZeroTargetingIDs` and
  `TestMeta_DispatchRefusesLeadingZeroBehaviorBeforeAnyRequest`
  (`internal/dispatch/meta_behavior_targeting_test.go`), plus new leading-zero cases for every
  list in `TestNormalizeAudienceTargeting_BehaviorsAndWorkPositions`. All of them fail with the
  old regex.

Not verified live: no Meta call was made.
