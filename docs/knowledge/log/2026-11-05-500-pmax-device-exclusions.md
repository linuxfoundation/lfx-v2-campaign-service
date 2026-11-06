# 2026-11-05 — Performance Max takes device exclusions (#500)

**Update** — `deviceBidModifiers` used to be refused outright on `performance-max`, on the
belief that Performance Max takes no device criteria at all. A read-only query of account
8666746580 showed otherwise: Google creates four device rows on every Performance Max
campaign itself, `negative: false`, with its fixed platform criterion ids (DESKTOP `~30000`,
MOBILE `~30001`, TABLET `~30002`, CONNECTED_TV `~30004`, the "Platforms" table of the
codes-and-formats reference), and an excluded device is stored as `bidModifier: 0` on its
row. RISC-V Summit Europe 2026 excludes three devices that way.

On Performance Max the preflight now accepts `bidModifier` 0 for those four devices and
refuses, before the budget mutate:

- any other modifier. A real adjustment is one Google refuses (`OPERATION_NOT_PERMITTED_FOR_CAMPAIGN_TYPE`
  for 1.2). Google accepted 1, but it adjusts nothing, so it is refused with its own message;
- excluding all four devices. Google's `validateOnly` accepted it, but the campaign could not
  serve anywhere.

`createCampaignTargetingCriteria` sends each exclusion as an UPDATE of
`customers/{cid}/campaignCriteria/{campaignId}~{id}` with `updateMask: bid_modifier` and an
explicit `bidModifier: 0` (`campaignCriterionBidModifierUpdate`,
`criteriaPlan.devicesUpdateExisting`). The update goes in the existing Step 4
`campaignCriteria:mutate`, after the campaign exists, in the same atomic batch as the
language and schedule creates. The result check also insists each update's result names the
row the request named. A Step 4 failure returns the PAUSED campaign alongside the error, as
every step past the campaign create does. Other channels still create their device criteria.

**Probes** (2026-11-05, `validateOnly`, paused LFX campaign 24441077512, nothing changed):
updating each row to 0 was accepted; all four in one request was accepted; an ad-schedule
create plus a `~30004` update in one request was accepted; MOBILE at 1.2 was refused with
`OPERATION_NOT_PERMITTED_FOR_CAMPAIGN_TYPE`; MOBILE at 1.0 was accepted (not offered). An
earlier probe built its update mask with a field-mask helper, which omits a field at its
default value, so its 0 updates had masked only `resource_name`. The probes above set
`bid_modifier` in the mask explicitly. Not verified live: that the rows exist the instant
`campaigns:mutate` returns. They do not wait for an asset group: 24410310689, which this
service created through the API on 2026-10-29 and which has no asset group, carries all four.
`change_event` does not log Google's own rows, so their exact creation time is not visible.

A Step 4 failure on this channel names the missing kind "device exclusion" rather than
"device", because Google's rows exist either way.

Stale "Performance Max takes no device criteria" comments in `campaign_criteria.go`,
`bidding.go`, `pmax.go` and the dispatch config were corrected, and the api-catalog and the
googleads concept updated.
