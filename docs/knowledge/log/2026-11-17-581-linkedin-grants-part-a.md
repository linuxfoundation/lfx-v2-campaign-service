# 2026-11-17 — #581 part A: grants, levers and the scoped monitor on main

**Update** — Part A of #581, split from the original stacked branch and replayed on main after
#557, #621, #614 and #622: grant storage and installer (migration `000058`, on `000057`), levers on
granted accounts, and the project-scoped LinkedIn monitor and insights.

- The `allowedVersionGaps` entry for `000057` is not carried: #614 landed `000057`.
- `bootstrap-system-account` refuses `-clear-linkedin-grants` with `-linkedin-grant`, and runs the
  row-free grant shape checks (`ValidateLinkedInGrants` with an empty row pair) in
  `planGrantInstall`, before a credential run writes anything (pre-PR review).
- api-catalog: the LinkedIn grants and the scoped monitor are documented as new lines under
  Platform-Specific Gotchas › LinkedIn Ads, leaving the long `{provider}` account-monitor row alone.
