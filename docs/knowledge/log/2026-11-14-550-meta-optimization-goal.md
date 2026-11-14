# 2026-11-14 — #550 Meta landing-page-views optimization goal

**Creation** — Gap 11 of the Meta gap review (#550). A traffic ad set always optimized for
`LINK_CLICKS`. `metaConfig.optimizationGoal` (LINK_CLICKS or LANDING_PAGE_VIEWS, traffic objective
only) lets an event campaign optimize for people who load the registration page instead. Omitted,
the payload is byte-for-byte the old one. Leads keeps LINK_CLICKS on purpose: it runs as traffic only
until instant forms exist. A manual bid is a link-click bid cap, so LANDING_PAGE_VIEWS with manual
bidding is refused synchronously (`metaBidPlan`) and again in the client, which now checks the
effective goal before any write.

Behind `META_OPTIMIZATION_GOAL_ENABLED` (default off) until a live probe; every refusal — flag off,
another objective, outside the list, a wrong JSON type, the bid conflict — is a synchronous 400 and
is re-checked in the job before any request.

Tests: `delivery_options_test.go` (the goal on the ad set, the defaults, four refusals with no
request sent, the bid conflict with no write) and `meta_optimization_goal_test.go` (flag off and on,
wrong type, the bid conflict, Dispatch's refusal and wiring). Eight hand mutations (the goal
override, the effective-goal bid check, the traffic-only rule, the flag, the dispatcher bid
conflict, the wiring, the string type check, the dispatcher allow-list call) each fail a test.
