# 2026-11-05 — Google conversion actions list is a global_marketing_ops read

**Update** — `GET /projects/{project_id}/google-ads/conversion-actions` (#458) was ruled by its
own Heimdall rule, `root-campaign-admin:get`, which admitted only `writer_guard` or
`global_marketing_ops` on the ROOT project. The ROOT project's UID came from the chart value
`openfga.rootProjectUid`, which no environment ever set, so the rule rendered `deny_all` and
every caller got 403.

The response lists every foundation's conversion actions on the shared Google Ads customer, so it
must reach only the LF marketing team. `campaign_manager` is not that population:
lfx-v2-helm's `model.fga` defines it as `executive_director or global_marketing_ops`, and
`executive_director` is a foundation-scoped person granted per project. `global_marketing_ops`
is the LF marketing team, written on every project, so checking it on the path's own project
needs no ROOT project id.

The path now has its own rule, `marketing-ops-api`: GET only, the same slug/UUID branches as
`project-api`, on relation `global_marketing_ops`. The `root-campaign-admin:get` rule, the
`openfga.rootProjectUid` value and its UUID check are removed.

`TestConversionActionsIsAMarketingOpsRead` (`charts/lfx-v2-campaign-service/parity_test.go`)
pins the path to `marketing-ops-api` alone (no `project-api` pattern matches it), GET only, and
that no rendered rule mentions `root-campaign-admin`, `writer_guard` or `rootProjectUid`.
`assertProjectAPIAuthz` became `assertRuleAuthz(ruleID, relation)`, and the parity tests now
read and pin both FGA-gated rules. Each of these fails the tests: the relation set back to
`campaign_manager` (either branch or both), the path added back to `project-api`, and a second
method on the rule. The Goa description of `list-google-ads-conversion-actions`, the
api-catalog, architecture, ruleset, httproute and targeting-lookups docs say the same.

The user-list read is unchanged: it is still refused with 409 on the shared account.
