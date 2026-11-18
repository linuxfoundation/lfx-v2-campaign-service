# 2026-11-17 — #584 hold the event's campaign group through a capped create

**Fix** — review of the continuous-schedule change (cs#607) found two ways the shared-group
guard could let a daily-capped campaign serve past its end.

- **Interleaved creates.** The sibling search sees only campaigns that exist. A capped create
  checked the group, released the group lock, and POSTed its campaign later; a second create
  of the event could widen the group in between, its search not yet seeing the capped campaign.
  `CreateCampaign` now holds `eventGroupLocks`, keyed on the group NAME (a group the create
  makes has no id yet), from `findOrCreateCampaignGroup` through `createSponsoredCampaign`.
  In-process only: another replica is still LFXV2-2665's.
- **Unreadable search elements.** `null` and `{}` unmarshal into an empty element, which read
  as an uncapped campaign. An element with no campaign id is now an error, so the group is not
  widened.
- Tests: `TestCreateCampaign_InterleavedCreatesCannotWidenUnderACappedCampaign` (two creates,
  the second must wait and then be refused with no widening write), and `null`, `{}` and no-id
  cases in the no-write table. Removing the lock, or the id check, fails them.
- KB: the #584 pacing paragraph in `internal-service-rules.md` had split a sentence in the
  Currency section; moved after it.
