# 2026-11-03 — Google conversion actions list is a campaign_manager read

**Update** — `GET /projects/{project_id}/google-ads/conversion-actions` (#458) was ruled by its
own Heimdall rule, `root-campaign-admin:get`, which admitted only `writer_guard` or
`global_marketing_ops` on the ROOT project. The ROOT project's UID came from the chart value
`openfga.rootProjectUid`, which no environment ever set, so the rule rendered `deny_all` and
every caller got 403.

The paid-marketing owner decided (#468, closed as won't do) that `campaign_manager` is held only
by the internal LF marketing team, which manages every foundation, so no input or read needs ROOT
and none is scoped per foundation. The path now sits in `project-api` with every other
create-form lookup. The `root-campaign-admin:get` rule, the `openfga.rootProjectUid` value and
its UUID check are removed. The response still lists every foundation's conversion actions on
the shared customer, which is intended.

`TestConversionActionsIsAProjectAPIRead` (`charts/lfx-v2-campaign-service/parity_test.go`) pins
the path in `project-api` exactly once, and that no rendered rule mentions
`root-campaign-admin`, `writer_guard` or `global_marketing_ops`. Restoring the old ruleset fails
it on all three counts, and dropping the path from `project-api` fails it too. The four
`TestRootAdminRule*` tests and `TestConversionActionsIsRuledOnlyByTheRootAdminRule` went with the
rule. The Goa description of `list-google-ads-conversion-actions`, the api-catalog, architecture,
ruleset, httproute and targeting-lookups docs say the same.

The user-list read is unchanged: it is still refused with 409 on the shared account.
