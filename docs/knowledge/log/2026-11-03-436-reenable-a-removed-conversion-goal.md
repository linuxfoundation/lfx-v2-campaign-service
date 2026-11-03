# 2026-11-03 — #436 follow-up: a removed conversion goal holding the set is re-enabled

**Fix** — After #447, a goal named for an action set that had been REMOVED was left as it is and a
new goal was created under the set's second name. Google refuses a goal whose action list
another goal holds (`DUPLICATE_CONVERSION_ACTION_LIST`). Whether a REMOVED goal counts for that
check is not documented, and the LF account has no removed goal to test against, so that create
may never succeed and every later campaign for the set would stop as a paused partial.

`goalHoldingActionSet` (`internal/platform/googleads/conversion_goal.go`) now finds the goal
holding exactly the set under any name and status, preferring an enabled one, and
`useGoalHoldingActionSet` reuses an enabled holder as it is or re-enables a removed one with a
status-only update. This covers the named LFX goal, a name race on it, and a duplicate-list
refusal alike. Both lookups ask for `status IN ('ENABLED', 'REMOVED')` explicitly. If Google
definitely refuses the re-enable of the goal found for the named goal, or for a name race on the
set's own name (`useGoalNamedForSet`, which resolves the race as if the first lookup had seen
the winner), nothing changed and the set gets a goal under its second name instead. That
fallback is an attempt, not a guarantee: if Google both counts a removed goal toward the
duplicate check and refuses to re-enable it, the second-name create is refused too and the goal
is NOT applied, leaving a PAUSED partial. A duplicate-list refusal takes no fallback, since a
create under any name would be refused for the same list. Re-enabling is safe: Google refuses to
remove a goal linked to a campaign (`CANNOT_REMOVE_LINKED_CUSTOM_CONVERSION_GOAL`), so a removed
goal counts conversions for no campaign. A goal edited by hand is still never written and still
leads to the second name. The re-enable goes through `doRequestCounted` and must name the goal,
or it is UNCONFIRMED and the campaign is not pointed at it.

Tests: `conversion_goal_test.go` covers re-enabling the named goal and one under another name
(status-only payload), an enabled holder preferred to a removed one (directly and after a name
race), a duplicate-list refusal held by a removed goal, a name race lost to a removed goal on
the set's own name and on its second name, an unconfirmed re-enable (a 2xx naming another goal;
a refusal after a retried 429), a definitely refused re-enable falling back to the second name
(directly and after a name race), and a removed goal Google counts and will not re-enable ending
NOT applied with no goal created. Each guard and both lookup filters were mutation-checked: the
test fake applies a query's status filter to its rows as Google would, and fails on any goal
write other than a status-only re-enable.

Open: the re-enable has not run against the live account; there is no removed goal there to aim
a `validateOnly` probe at.
