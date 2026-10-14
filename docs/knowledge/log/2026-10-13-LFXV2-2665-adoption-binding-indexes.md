# 2026-10-13 — LFXV2-2665 one live binding per upstream campaign for Meta, Microsoft, Reddit and X

**Fix** — Meta, Microsoft, Reddit and X adoption could bind one upstream campaign to two briefs
(any project): adoption's ON CONFLICT arbitrates only the destination slot, and the binding
indexes covered only Google Ads (000020) and LinkedIn (000042). Migrations 000043-000046 add one
SEPARATE unique index per provider over live rows (CONCURRENTLY, one statement, no `IF NOT EXISTS`,
CONCURRENTLY down), decided per provider:

| Provider | Key | Why |
| --- | --- | --- |
| Meta (000043) | `(platform, platform_campaign_id)` — global | A campaign is a Graph node, "an individual object with a unique ID" (https://developers.facebook.com/docs/graph-api/overview); read as `GET /{id}` with no account. |
| Microsoft (000044) | + account: `result.accountId`, else `aid=` of `result.microsoftAdsUrl` | Ids are account-scoped (2026-08-10 LFXV2-3056 log; Campaign Management is scoped by `CustomerAccountId`, https://learn.microsoft.com/en-us/advertising/campaign-management-service/getcampaignsbyids). |
| Reddit (000045) | + account: `result.accountId` | Not documented as global; every v3 campaign path is `/ad_accounts/{account_id}/campaigns/{id}` (https://ads-api.reddit.com/docs/v3/). |
| X (000046) | + account: `result.AccountID` | Not documented as global; every campaign endpoint is `accounts/:account_id/campaigns/:id` (https://docs.x.com/x-ads-api/campaign-management). |

An account key is exact whether the ids are global (one id always carries one account) or per
account (no false reject), so it is the safe choice where the docs are silent. The account is
extracted from the `result` provenance JSON exactly as each dispatcher's `*CreationAccountID`
reads it. **Legacy rows** recording no account are NULL in the key and never block a legitimate
binding; `AdoptCampaign` refuses to adopt an id a live account-less row holds, so they cannot slip
a second binding through adoption (a dispatch under a known account is not blocked by them).

All six binding indexes are registered in `requiredIndexes`; the boot check now reads keys with
`pg_get_indexdef(index, n, true)` so expression keys are verified. `isPlatformCampaignBindingViolation`
covers all six, so adoption answers 409 AlreadyBound and a duplicate dispatch is the named
conflict with its claim retained. A Meta live-test seed that reused the fixed id `555` now mints
a unique one.

**Deploy risk:** if live rows already bind one upstream campaign twice on any of these providers
(same account for Microsoft/Reddit/X), that CONCURRENTLY build fails and the deploy stops. Before
deploying, check with e.g. `SELECT platform, platform_campaign_id, count(*) FROM campaigns WHERE
status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform IN ('meta-ads',
'microsoft-ads','reddit-ads','twitter-ads') GROUP BY 1,2 HAVING count(*) > 1` (refine by account
for the account-keyed three); soft-delete one row of each duplicate, drop any INVALID index, re-run.

Follow-up (thread on #318): the campaign-ref methods' published `matches` cardinality is now per
provider — `localCampaignRefMethod` takes the sentence per call. Meta and LinkedIn say at most
one in a valid database (global indexes 000043/000042); Microsoft, Reddit and X say more than one
is reachable (one live row per (id, ad account) under 000044-000046, plus account-less legacy
rows). The same claim was corrected in the service comments, the comment above
`resolvePlatformCampaignQuery`, the api-catalog Microsoft and twin rows, and the service KB.

#328 review: the required-index boot check compared expression keys as exact strings against a
PG16 deparse, so a PostgreSQL upgrade re-rendering them could fail boot. Production's Postgres is
external and not pinned here (only CI is, `postgres:16-alpine`), so the check now compares the
RUNNING server's deparse of both sides: `serverDeparsedRequiredIndexes` builds each expected
definition on an empty TEMP copy of its table inside a rolled-back transaction and reads that
probe's deparse. A live test proves a differently-spelled identical definition passes and a
different one fails. `identifierRe` moved above the `requiredIndex` doc comment so godoc attaches
correctly.

The deparse probe is best-effort: a failure of any probe step (begin, CREATE TEMP TABLE, savepoint,
rollback-to, reading the probe — a role without TEMPORARY, a read-only default, a hot standby)
logs one WARN with the step and error class and falls back to the stored-text comparison for the
remaining entries, so it adds no new way for boot to fail; a genuinely different definition is
still refused either way.
