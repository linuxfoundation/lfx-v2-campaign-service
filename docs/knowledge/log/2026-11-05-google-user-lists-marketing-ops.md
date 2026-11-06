# 2026-11-05 — Google user-list read is a global_marketing_ops read on the shared account

**Update** — `GET /projects/{project_id}/google-ads/user-lists` (`list-google-ads-user-lists`)
now answers on the shared LF Google Ads customer, so the self-serve Search observed-lists picker
can list the HubSpot CRM and website visitor lists.

Before this change it was a `campaign_manager` read (`project-api`). It resolved the project's
own connection only, and `Orchestrator.ReadUserLists` refused any shared-tenancy platform with
409 (`ErrUserListsSharedTenancy`). The read is account-wide and nothing scopes user lists to a
project, so a foundation executive director would otherwise have seen every foundation's lists.
On Google Ads that meant the picker could never list anything.

- The route moves to the `marketing-ops-api` Heimdall rule (`global_marketing_ops`). That is the
  LF marketing team, the same group that can read the conversion actions and audiences on the
  shared customer. `TestUserListsIsAMarketingOpsRead` pins it, and
  `TestMarketingOpsRuleHoldsExactlyItsReads` now lists three paths.
- The orchestrator's shared-tenancy refusal is removed, along with `ErrUserListsSharedTenancy`,
  the 409 response and the `user-lists-conflict-error` type.
- `GoogleAdsDispatcher.ListUserLists` now resolves as a create does (`createAccountClient`): the
  project's own connection, the LF fallback, and the forced-system redirect. The default
  account is therefore the one the create uses, and `audienceSegments` accepts its lists.
- The read still refuses a connection that is actually unusable: no account selected or a
  non-digit stored id gives 400 on the project's own row and 500 on the LF row. A malformed
  `account_id` is still a 400 before any credential is resolved.
- A read-only GAQL search of the LF customer returned 450 user lists, under the 500 cap:
  - by type: 84 CRM_BASED, 110 RULE_BASED, 89 REMARKETING, 151 SIMILAR, 15 LOGICAL and
    1 LOOKALIKE;
  - 351 OPEN, all 450 ENABLED, and 378 eligible for Search.
