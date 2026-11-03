# 2026-11-03 — #475 pre-PR review fixes

**Fix** — The #475 pre-PR review of the Display ad-group list.

- **Ad pass.** Every Display ad group is created before the first ad, so a stop on group k's
  ad leaves groups k+1..n created with no ad. `adGroups` already listed them with their ids
  and no `adIds`; with more than one group the error now names them as well. The ads go in a
  second pass because the ad is the only step that makes the campaign servable, so the
  campaign-level exclusions must land before any ad.
- **Over-long group name.** The refusal reused `validateEntityName`'s text, which says to
  shorten EventName/Project and calls the suffix fixed. That advice is wrong when the suffix
  is the caller's own label. `namedAdGroupNameTooLong` now builds the message: the group's
  position, the name, its rune count, Google's limit, and "shorten EventName or this group's
  name". The Search ad-group list had the same message and now shares the helper.
- **Duplicate group names.** The key was `strings.ToLower`, which keeps `ſ`/`s` and `ς`/`σ`
  apart where `strings.EqualFold` joins them. `adGroupNameKey` (`textfold.Key`) is now the
  key for both the Display and the Search list.
- **Canonical ids.** `validateDisplayAudienceList` accepted any digit run as the id, so
  `userLists/011` targeted and `userLists/11` excluded passed both the de-dupe and
  `refuseExcludedOverlap`. A non-canonical id is now refused with its own message. Search's
  `validateAudienceSegments` had the same gap on both of its ids and refuses the same way. Geo
  and language ids were already canonical-checked.
- **Off-channel refusal.** `DisplayAdGroups` sent to another channel now names that channel's
  own shape: `AdGroups` on Search; `DemandGenCreative`, `VideoCreative` or
  `PerformanceMaxCreative` elsewhere.
- **targetingSetting.** A group with exclusions but no audiences sends no targetingSetting.
  This was not probed; the setting governs positive criteria only.

Tests: a dispatch toggle test on a two-group Display row, in both directions; the ad-pass
partial-result case; fold, canonical-id and per-channel refusal cases for Display; and
fold, over-long-name and canonical-id cases for Search.
