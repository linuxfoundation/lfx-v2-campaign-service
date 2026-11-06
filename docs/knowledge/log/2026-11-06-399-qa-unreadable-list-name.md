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
  absence finding (the CRITICAL, the excludes-NOTHING MEDIUM, the opt-out HIGH and the Canada
  MEDIUM) and raises one HIGH NEEDS VERIFY naming the ids and what could not be verified. A
  suppression that was read is still credited.
- Response shape, additive: optional `checks.suppression.unverified_list_ids`. Absent when every
  exclusion was read, so `applied_gdpr` / `applied_opt_out` keep their meaning for existing
  clients; present, a false flag means "not verified", not "absent".
- `CheckSignalMapping(filters, names, unreadable)`: when no engagement signal is found and an
  inclusion references an unread list, a MEDIUM NEEDS VERIFY names it instead of the generic
  "could not classify". The issue's check-4 FAIL path does not occur on main (an `IN_LIST`
  signature always carries "in_list", which is not a firmographic hint), so this only makes the
  existing NEEDS VERIFY say which read failed.

Not changed: a send-time suppression id whose read fails is still skipped uncredited, so check 2
(exclusion completeness) can still FAIL on a list whose only exclusion is such an id.

Tests: `TestRunQA_AnUnreadableListNameIsNotVerifiedRatherThanAbsent` (a fake portal answering
403 from v3 and legacy for one id: direct exclusion, wrapper child, wrapper whose filters fail,
the 404 control, and the signal-mapping inclusion),
`TestCheckSuppression_AnUnreadableExclusionIsNotVerifiedRatherThanAbsent` and
`TestCheckSignalMapping_AnUnreadableNameIsNotVerifiedRatherThanFailed`. Each fails with its part
of the fix reverted (six mutations: `listName` never unreadable, child id dropped, wrapper-filter
failure dropped, `CheckSuppression` ignoring the ids, `CheckSignalMapping` ignoring them,
`NOT_IN_LIST` counted as an inclusion).
