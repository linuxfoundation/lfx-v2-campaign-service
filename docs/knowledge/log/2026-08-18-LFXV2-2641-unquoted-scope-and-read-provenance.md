# 2026-08-18 — LFXV2-2641 keyword scope: unquoted ids, and provenance on the insight reads

**Fix** — two defects in the project-scoping change from earlier the same day
(`2026-08-18-LFXV2-2641-project-scoped-keyword-reads.md`). Both were raised in review and both
are in the security boundary that change introduced, so the scoping was not yet doing the job it
was written to do.

## 1. The scope predicate quoted an int64, so the filter could not work

`campaignScopePredicate` rendered `campaign.id IN ('111', '222')`. `campaign.id` is an **int64**
in GAQL, so quoting makes it a string comparison against a numeric column — the query is
rejected or matches nothing. Either way the predicate does not filter, which means the
account-wide exposure the earlier fix set out to close was still reachable in every code path
that mattered.

This repo already knew the rule and stated it twice, which is what makes this a miss rather than
an unknown:

- `campaign_lookup.go`: "campaign.id is an int64 in GAQL, so it is compared UNQUOTED — quoting it
  would make this a string comparison against a numeric field."
- `metrics.go` renders `campaign.id = %s` unquoted.
- `get_campaign_test.go` asserts **both** that `campaign.id = 555` is present and that
  `campaign.id = '555'` is absent.

The predicate now renders `campaign.id IN (111, 222)`.

**The test agreed with the bug.** `TestCampaignScopePredicate_RendersAnINList` asserted the
quoted string, so the whole suite was green against a predicate that could not filter — a test
written from the implementation rather than from the contract. It is now
`TestCampaignScopePredicate_RendersAnUnquotedINList` and asserts the unquoted form *and* that no
quote character appears at all, mirroring the two-sided assertion `get_campaign_test.go` already
used for the same field on the single-id path.

## 2. The insight reads skipped the creation-customer check the other paths enforce

`ReadKeywordPerformance` and `ReadAudienceInsights` passed bare `platform_campaign_id`s straight
to GAQL under the project's **current** connection. `ReadMetrics` refuses that
(`googleAdsCreationCustomerID(campaign) != client.CustomerID()`), and `ApplyKeywordActions`
enforces the same invariant, because an id is a bare numeric unique only **within** its customer
and `UpdateGoogleAds` can re-point a connection between create and read. A stale id then either
matches nothing — an empty read indistinguishable from a campaign with no activity — or, on a
numeric collision, selects another account's campaign. On a customer shared across every
foundation that means reporting a different project's keyword text and spend as this project's.

The id alone could not carry that check, so the scope now carries its provenance:

- `listProjectPlatformCampaignIDsQuery` selects `platform_campaign_id, result` — `result` holds
  the creating customer.
- `ListProjectPlatformCampaignIDs` returns `[]model.ProjectCampaignScope` instead of `[]string`.
- `googleAdsScopeForCustomer` drops entries whose recorded customer disagrees with the one the
  client resolves to, before any GAQL is built.

**Unknown provenance is still READ.** An empty recorded customer means "unknown", and
`googleAdsCreationCustomerID` documents that the caller must treat it as permission to proceed —
the same choice `ReadMetrics` and `ToggleStatus` make. Dropping those rows would silently empty
the results of every project whose campaigns predate provenance tracking. This is deliberately
asymmetric with `ApplyKeywordActions`, which fails **closed** on unknown provenance: a misleading
read is recoverable and an irreversible `REMOVE` is not.

A scope that is non-empty on the way in but empty **after** the filter is an error, never an
empty id list. `campaignScopePredicate` would refuse an empty list anyway, but relying on that
would leave the account-wide read one dropped guard away.

## Verification

Every guard was confirmed by a compiling revert rather than by reading it:

| Reverted | Test that failed |
| --- | --- |
| unquoted rendering → quoted | `TestCampaignScopePredicate_RendersAnUnquotedINList` |
| provenance filter removed | `TestGoogleAdsInsightReads_ForeignAccountCampaignsAreNotQueried`, `..._AllForeignScopeIsRefusedWithoutQuerying` |
| orchestrator empty-scope early return removed | `TestKeywordInsights_EmptyScopeIssuesNoUpstreamCall` |
| predicate returns `""` on empty list | `TestCampaignScopePredicate_EmptyListIsRefused` |

`TestGoogleAdsInsightReads_UnknownProvenanceIsStillRead` pins the legacy-row case in the other
direction, so a future tightening of the filter cannot silently empty those projects' results.

## Also in this pass

- `design/brief.go` and `connection_keywords.go` still described these reads as **account-wide**
  after the scoping landed. The generated OpenAPI is the consumer contract, so that wording
  contradicted the authorization model actually implemented. Corrected and regenerated with
  `make apigen`.
- The apply-keyword-actions Method Description did not list the provenance-unknown 409, which the
  handler returns separately from account-mismatch and which has a different remedy
  (re-dispatch, not reconnect).
