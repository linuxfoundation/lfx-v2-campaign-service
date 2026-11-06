# 2026-11-06 — #399 QA: an unreadable list name is unverified, not an absent suppression

**Fix** — `(*AudienceExplorer).listName` returned "" for every failure, so a throttle, a 401/403
or a 5xx on a referenced list's name read the same as a list that does not exist. QA check 3
classifies on exclusion NAMES, so a GDPR suppression whose name read failed dropped out of the
set and an EU audience got the CRITICAL "No GDPR suppression list found", with
`applied_gdpr: false`, on a list whose GDPR suppression was applied.

- `listName` now returns `(name, unreadable)`. `unreadable` is set when neither the v3 nor the
  legacy read produced a name and either failed with anything but a 404, classified on
  `hubspot.IsNotFound` as `listWithFilters` and `listBriefs` (#238) already do. A 404 from both is
  still a genuine "" and still an absent list. `LegacyListName` / `ListIDForLegacy` are unchanged.
- `exclusionNames` also returns the exclusions it could not read: an unreadable name, a wrapper
  whose own filters failed for anything but a 404 (its covered lists are unknown), and an
  unreadable covered child.
- `CheckSuppression(names, unverifiedIDs, eu, ca)`: while any id is unverified it withholds every
  absence finding (the "No GDPR suppression list found" CRITICAL, the excludes-NOTHING MEDIUM,
  the opt-out HIGH and the Canada MEDIUM) and raises one finding naming the ids and what could
  not be verified. It is CRITICAL (FAIL) when an EU audience's GDPR suppression is the unverified
  one: fail closed, so a throttled read never softens an EU send that may have no GDPR
  suppression (raised in review). Otherwise it is HIGH (NEEDS VERIFY). A suppression that was
  read is still credited.
- Response shape, additive: optional `checks.suppression.unverified_list_ids`. Absent when every
  exclusion was read, so `applied_gdpr` / `applied_opt_out` keep their meaning for existing
  clients; present, a false flag means "not verified", not "absent".
- `CheckSignalMapping(filters, names, unreadable)` (check 1): when no engagement signal is found
  and an inclusion references an unread list, a MEDIUM NEEDS VERIFY names it instead of the generic
  "could not classify". The issue's signal-mapping FAIL path does not occur on main (an `IN_LIST`
  signature always carries "in_list", which is not a firmographic hint), so this only makes the
  existing NEEDS VERIFY say which read failed.
- Send-time suppression ids: one whose read fails with anything but a 404 is still never
  credited, but it joins check 3's unverified set, and
  `CheckExclusionCompleteness(filters, sendTime, unverifiedSendTime)` (check 2) reports NEEDS
  VERIFY naming it instead of the zero-exclusions FAIL.
- Check 4 (`current_registrants`): a PASS is replaced by a HIGH NEEDS VERIFY while an included
  list the send still reaches has an unread name, since it may be this edition's registrants.
  A verdict reached on names that were read is left alone.

Tests: `TestRunQA_AnUnreadableListNameIsNotVerifiedRatherThanAbsent` (a fake portal answering
403 from v3 and legacy for one id: direct exclusion, wrapper child, wrapper whose filters fail,
send-time GDPR list, unreadable send-time wrapper, the 404 control, check 1 naming an unread
inclusion, check 4 not passing on one, and check 4 ignoring one the send suppresses),
`TestCheckSuppression_AnUnreadableExclusionIsNotVerifiedRatherThanAbsent`,
`TestCheckSignalMapping_AnUnreadableNameIsNotVerifiedRatherThanFailed` and
`TestCheckExclusionCompleteness_AnUnreadableSendTimeListIsNotVerified`. Each part of the fix was
reverted on its own and a test failed every time (ten mutations).
