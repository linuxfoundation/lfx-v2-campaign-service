---
type: "Kubernetes Resource"
title: "Deployment"
description: "Helm Deployment for the campaign service, including PG*, CREDENTIAL_ENCRYPTION_KEY, SNOWFLAKE_* and the AI_PROXY_URL / AI_API_KEY credentials from lfx-v2-campaign-service-secrets, plus plain non-secret values such as AI_MODEL."
resource: "charts/lfx-v2-campaign-service/templates/deployment.yaml"
---

# Deployment

Kubernetes Deployment manifest for the campaign service, defined in the Helm
chart. Application env vars come from `values.yaml` `app.environment`,
including `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` / `PGDATABASE` /
`PGENGINE` and `CREDENTIAL_ENCRYPTION_KEY` via `secretKeyRef` to
`lfx-v2-campaign-service-secrets` (keys `host`, `port`, `username`,
`password`, `dbname`, `engine`, `credential-encryption-key`). The encryption
key is required whenever a database URL is configured because startup
initializes the AES-GCM encryptor before opening the pool used by `/readyz`.

`SNOWFLAKE_ACCOUNT` / `SNOWFLAKE_USER` / `SNOWFLAKE_PRIVATE_KEY` also come via
`secretKeyRef` to `lfx-v2-campaign-service-secrets` (keys `snowflake-account`,
`snowflake-user`, `snowflake-private-key`), each marked `optional: true` — unlike
the Postgres/encryption vars, an unset Snowflake secret does not block startup:
the warehouse client is a read-only enrichment for the email channel's
past-editions audience lookup (`internal/platform/snowflake`), and its absence
just narrows a built audience to country-only rather than failing the pod.
`SNOWFLAKE_WAREHOUSE` / `SNOWFLAKE_ROLE` are plain (non-secret) values, empty by
default so a cluster with no override uses the account's default.

`AI_PROXY_URL` / `AI_API_KEY` follow the Snowflake pattern exactly: `secretKeyRef`
to `lfx-v2-campaign-service-secrets` (keys `ai-proxy-url`, `ai-api-key`), both
`optional: true`. Generated email copy is an enrichment on the same terms — a
cluster with no LiteLLM provisioning must still start, and the `GenerateEmailCopy`
endpoint returns 503 ServiceUnavailable when unconfigured. The secret is the **proxy's**
key, not a Bedrock or Anthropic credential. `AI_MODEL` is a plain (non-secret) value,
empty by default: a model id is not a credential, and empty selects `llm.DefaultModel`.
All three are printed by `Config.String()` — the model verbatim, the key through
`redactSecret`, and the URL through `redactAIProxyURL`, which keeps only the scheme,
renders the host as `xxxxx`, and masks the whole value if it will not parse or its
scheme is neither `http` nor `https`. The host is masked because it can itself BE the
pasted secret — `AI_PROXY_URL=https://sup3r-s3cret/` is a well-formed absolute URL.
That reduction is deliberate: `Config.String()` renders at startup before
`llm.NewClient` can reject a bad value, so it is the only place a pasted credential
would otherwise land in the log. What survives is enough for the thing being
diagnosed — whether a proxy and key are configured at all.

`REDDIT_KEYWORD_TARGETING_WRITES_ENABLED` is a plain feature gate, defaulting to `"false"`
(LFXV2-2665): it opts a cluster in to BOTH Reddit ad-group targeting writes — removing keywords
(`remove-keyword-targeting`) and adding negative keywords to `excluded_keywords`
(`add-negative-keywords`). Each writes the whole targeting object back, which has not been
exercised against a live ad account. Only exactly `"true"` enables them; the keyword-targeting
read is not gated.

`GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED` is a plain feature gate, defaulting to `"false"`
(#459): it opts a cluster in to `googleAdsConfig.demandGenAudience`, one existing Google Ads
Audience on a Demand Gen ad group. It is a per-deployment opt-in, turned on by setting it to
exactly `"true"` in that environment's lfx-v2-argocd overlay. The service checks the Audience
against the connected Google Ads account (same customer id). That account is shared by every
foundation and `campaign_manager` includes foundation executive directors, so create-campaigns
also requires `global_marketing_ops` on the project for any Demand Gen audience, asked of
project-service (`lfx.projects-api.slug_to_uid`) and fga-sync (`lfx.access_check.request`) over
the same NATS connection the indexer uses; with `NATS_URL` empty such a create answers 503. It also gates each
`googleAdsConfig.demandGenAdGroups[].audience` (#460), the same input on a named ad group. Only
exactly `"true"` enables them; off, a create naming one is refused before anything upstream
exists, and a Demand Gen ad group without an audience is unaffected.

`GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED` is the same kind of gate, defaulting to `"false"` (#462):
it opts a cluster in to the Display audience lists that name an account-owned segment —
`googleAdsConfig.audienceSegments` on `display`, `customAudiences` and `customAffinities`, and
since #475 the user-list exclusions `excludedAudienceSegments` and each `displayAdGroups`
entry's `audienceSegments`, `customAudiences`, `customAffinities` and
`excludedAudienceSegments`. Like the Demand Gen gate it is a per-deployment opt-in, each
segment is checked against the connected Google Ads account, and create-campaigns requires
`global_marketing_ops` on the project for any of them, by the same NATS check. Only exactly `"true"`
enables it; off, a create naming one is refused before anything upstream exists.
`userInterests` (at either level), `excludedPlacements`, Search's `audienceSegments` and
`displayAdGroups` entries without those lists are not behind the flag; Search's
`audienceSegments` still need `global_marketing_ops`. To enable it on a cluster, set
`app.environment.GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED.value` to `"true"` in that environment's
ArgoCD overlay.

`META_CALL_TO_ACTION_ENABLED` and `META_CONVERSION_EVENT_ENABLED` are the same kind of gate for
Meta, each defaulting to `"false"` (#550): they opt a cluster in to `metaConfig.callToAction` (the
ad's button) and `metaConfig.conversionEvent` (the conversions objective's `custom_event_type`).
Only exactly `"true"` enables each; off, a create carrying the field is refused with a 400 before
any job exists. Neither names an account resource, so neither is behind `global_marketing_ops`.

`META_AUDIENCE_TARGETING_ENABLED` is the same kind of gate for Meta, defaulting to `"false"`
(#550): it opts a cluster in to `metaConfig.targeting` (age, gender, custom audiences to include
and exclude, and the Advantage+ audience flag on the ad set). Each custom audience is read and must
belong to the connected Meta ad account before anything is created, and create-campaigns requires
`global_marketing_ops` on the project for the audience lists. Only exactly `"true"` enables it;
off, a create carrying a targeting block is refused with a 400 before any job exists.

`GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED` is the same kind of gate, defaulting to `"false"`
(#461): it opts a cluster in to `googleAdsConfig.performanceMaxAudienceSignal`, one existing
Google Ads Audience attached to a Performance Max asset group as a signal. Like the Demand Gen
gate it is a per-deployment opt-in, the Audience is checked against the connected Google Ads
account, and create-campaigns requires `global_marketing_ops` on the project for it. Only exactly `"true"` enables it; off, a create naming one is refused before
anything upstream exists. Search themes and the Performance Max extensions are not gated.

`GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED` is the same kind of gate, defaulting to `"false"`
(#477): it opts a cluster in to `googleAdsConfig.sharedNegativeKeywordLists`, existing shared
negative keyword lists attached to a Search campaign. Like the Demand Gen gate it is a
per-deployment opt-in, each shared list is checked against the connected Google Ads account,
and create-campaigns requires `global_marketing_ops` on the project for any. Only exactly `"true"` enables it; off, a create naming one, on any channel, is refused before
anything upstream exists. `searchAssets` is not gated.

`GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED` defaults to `"false"` too (#497), but gates something
different: not an account resource, but inputs no live (persisted) create has exercised yet —
`googleAdsConfig.observedDemographics` (#497), `googleAdsConfig.displayDemographics` (#499), and
`googleAdsConfig.demandGenDemographics` (#498), whose create body a validateOnly probe accepted on
2026-11-05 but whose persisted create and read-back are not yet verified. Only exactly `"true"` enables it; off, a create naming it,
on any channel, is refused before anything upstream exists. Turn it on once the validateOnly probe
in the field's docs passes.

`LINKEDIN_ADS_TARGETING_ENABLED` defaults to `"false"` (#552) and is the same kind of gate for
LinkedIn: `linkedInConfig.targeting`, the caller's own targetingCriteria, has not been exercised
against a live LinkedIn account. Only exactly `"true"` enables it; off, a create naming it is
refused with a 400 before a job exists. Its ad-segment facets (`audienceMatchingSegments`,
`dynamicSegments`) additionally require `global_marketing_ops` on the project.

`LINKEDIN_ADS_CONVERSIONS_ENABLED` defaults to `"false"` (#552) and is the same kind of gate for
LinkedIn: `linkedInConfig.conversions` sends a Campaign Conversions PUT that has not been exercised
against a live LinkedIn account. Only exactly `"true"` enables it; off, a create naming it is
refused with a 400 before a job exists. The ids additionally require `global_marketing_ops`.

`LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED` defaults to `"false"` (#581) and gates
`linkedInConfig.adAccountId` naming one of the LF system LinkedIn row's account grants
(`bootstrap-system-account -linkedin-grant`). Only exactly `"true"` enables it; off, a create naming
any account but the connection's own is refused with a 400 before a job exists. While on, any
`adAccountId` requires `global_marketing_ops`, and the system scope's LinkedIn accounts list returns
the default account and the grants. Flip it only after the release that pauses, reads and budgets
granted-account campaigns is in prod and the grants are installed.

`SURVEY_SEQUENCE_WORKFLOW_ID` is a plain value defaulting to `""`, which selects the in-code
default `1841232183`: the LF Research survey-promo workflow a survey send series
(`send-workflows`) is copied from when a request names none. That id exists only in the LF
portal, so a deployment whose projects connect other portals should set its own; a request
naming `source_workflow_url` overrides it either way.

`REDDIT_METRICS_ENABLED` is likewise a plain (non-secret) value, defaulting to
`"false"`. It is a feature gate rather than a credential: the Reddit reporting
contract this service implements follows Reddit's official public OpenAPI document
but has NOT been exercised against a live ad account (LFXV2-3282) — behaviour the
schema cannot express, such as zero-activity rows and the account's attribution
window, is still unconfirmed — so the metrics read is shipped disabled and a
cluster must opt in explicitly. The gate fails closed:
any value other than exactly `"true"` (including an empty string, `"1"`, or
`"TRUE"`) leaves the capability off, so a typo in a values override cannot
accidentally enable an unverified integration. The env var is read per call
(`RedditDispatcher.ReadMetrics`), not at construction, so flipping it needs a
pod restart to pick up the new `app.environment` value but no rebuild — and the
disabled path costs one env read. With the gate off the read returns
`ErrMetricsUnsupported`, which the service maps to the same 400 a platform with
no metrics adapter at all returns. The same flag, read the same way, also gates the
Reddit audience and keyword performance reads (`GET /projects/{project_id}/reddit-ads/audience`
and `.../reddit-ads/keywords`, `RedditDispatcher.AudienceEnabled` / `KeywordsEnabled`,
LFXV2-2665), which run on the same reporting operation with breakdowns: off, each answers 400 "not supported" before the project's
campaign scope is looked up or Reddit is contacted.

`MICROSOFT_METRICS_ENABLED` is the same kind of gate on the same terms, and defaults
to `"false"` for the same reason: the Microsoft Advertising v13 Reporting contract
was implemented from Microsoft's published documentation and has NOT been exercised
against a live ad account (LFXV2-3260). It fails closed identically — only exactly
`"true"` enables the read — and is likewise read per call
(`MicrosoftDispatcher.ReadMetrics`), so flipping it needs a pod restart but no
rebuild. Microsoft's pipeline has more surface to be wrong about than any other
platform's: an asynchronous submit/poll/download returning a zipped CSV rather than
one synchronous JSON GET. With the gate off, a Microsoft metrics read returns the
same 400 as Reddit's.

`TWITTER_METRICS_ENABLED` is the third gate of this kind, defaulting to `"false"`, and it
covers BOTH X Ads features built on stats jobs (LFXV2-2665) — the ACCOUNT MONITOR and the
AUDIENCE READ (`twitter-ads/audience`, `twitterAudienceEnabled` in
`internal/dispatch/twitter_audience.go`, checked by the orchestrator before the scope lookup):
their asynchronous stats-jobs contract
(`active_entities`, job create, job status, a downloaded gzip results file) follows X's
published documentation and has NOT been exercised against a live X Ads account. Only exactly
`"true"` enables it, read per call (`twitterMonitorEnabled` in
`internal/dispatch/twitter_monitor.go`); off, the monitor and the audience read answer the same
400 as a platform without them. X's per-campaign metrics read is not behind it. Because those
features' limits (the audience read's per-account job budget, the account-keyed write pacer)
are per PROCESS, the binding guarantee is at RUNTIME: only the pod holding the per-account
Postgres advisory lease (`postgres.StatsJobLease`) creates X stats jobs; any other answers the
audience read 503 and skips monitor submissions. The chart does NOT refuse a multi-replica render
with the flag on (#381): `templates/NOTES.txt` prints a warning instead, computed by
`lfx-v2-campaign-service.xStatsMultiReplica` in `templates/_helpers.tpl` when the EFFECTIVE flag
is on together with `replicaCount` > 1 or `autoscaling.enabled` with `maxReplicas` > 1.
"Effective" because the env list renders `app.environment` first and `app.extraEnv` after it and
Kubernetes keeps a repeated name's LAST occurrence: the last `app.extraEnv` entry wins, an
`app.environment` entry counts its `value` when the key is present and its `valueFrom` otherwise,
and a `valueFrom` on the effective entry (unreadable at render time) counts as on
(`TestDeploymentWarnsOnXStatsJobsWithMultipleReplicas`, which also pins that every such render
succeeds and carries the requested `spec.replicas`). ArgoCD does not show `NOTES.txt`; the warning
reaches a `helm install`/`upgrade` or a dry run, and the lease is what actually holds. The cost of
running the flag above one replica is availability, not the account's job limit: behind a load
balancer most X audience reads land on a pod that does not own the account's lease and answer 503,
so run one replica while the flag is on.

It used to be a render-time `fail`, and was downgraded for the reason the Reddit guard below was
retired: deployments run three replicas, so turning the flag on would have stopped ArgoCD
generating the manifest at all. The poller audit that justified it (#381): the lease covers BOTH
stats-job paths — the audience read and the account monitor's report submission
(`TwitterDispatcher.SubmitAccountReport` calls `ownStatsJobs` before contacting X) — and nothing
polls either. No ticker, cron or subscriber in `internal/` or `cmd/` reaches
`ConnectionService.MonitorTwitterAdsAccount` or the audience read (the service's tickers are the
dispatch recovery, stuck-claim, job-retention, archived-asset and indexer-relay sweeps, the
shutdown in-flight wait and the wizard progress stream); the
`lfx-self-serve` Monitoring tab reads the X panel only on a user action (platform switch, date
range, its Refresh button) and, when a report is still building, tells the user to refresh rather
than re-reading on a timer; and `lfx-v2-argocd` defines no CronJob for this service. The one
loop is `cmd/verify-metrics -poll-interval`, an operator CLI that runs its own in-process
orchestrator against a live account for a bounded `-timeout`, not a deployed poller.

The chart therefore has NO replica guard. A second one, refusing more than one replica
unconditionally over the Reddit reporting budget, was added and then retired; the chart pins the
opposite (`TestDeploymentAllowsMultipleReplicasForReddit`), because a render-time `fail`
stopped ArgoCD generating the manifest at all for a production deployment that runs three
replicas, holding up every unrelated change to this service.

The constraint it was protecting was real, and is now met at runtime instead (#380). The Reddit
reporting budget (`internal/platform/reddit/report_budget.go`, 45 of an ad account's 60 report
requests a minute) WAS a per-process ledger, so N replicas each carried a separate 45. It is now a
SHARED ledger in Postgres (`postgres.RedditReportLedger`, table `reddit_report_slots`, migration
000055): slot rows counted and inserted under a per-account transaction advisory lock on the
database's clock, so the 45 holds account-wide at any replica count. It fails closed — when
Postgres cannot be asked, a Reddit report attempt is refused (503) rather than sent unbudgeted. `REDDIT_METRICS_ENABLED` does not
bound it: that flag gates only the Reddit metrics and audience reads, while the Reddit account
monitor (`connection-reddit-ads/account-monitor` → `ConnectionService.MonitorRedditAdsAccount` →
`RedditDispatcher.readAccountMonitor`) is ungated and always routed. (Gating the monitor behind
the flag was rejected: it is a shipped, flag-free feature with its own alert rules,
`internal/service/rules/monitor_reddit.go`, and gating it would turn its working answer into a
400.) But always *routed* is not always *polling* — no ticker, cron or subscriber in `internal/`
or `cmd/` invokes the monitor or the insight reads, so the old per-process N-fold ceiling was
reachable only by concurrent user traffic, never a sustained rate.

The shared ledger is a sliding window of slot rows, not a refilling token bucket: a bucket of 45
refilled at 45 a minute admits up to 90 inside one 60 s window, past Reddit's 60. Two shortcuts
that were considered before it do not work and should not be proposed: a lease like the X stats jobs'
makes non-owner pods REFUSE (`domain.ErrStatsJobLeaseNotHeld`), tolerable for flag-gated
background jobs but not for a user-facing monitor endpoint; and dividing the 45 by the replica
count leaves 15 at three replicas, below the 24 a single audience read
(`MaxReportCampaigns` 8 × 3 breakdowns) already needs.

The pod template carries the Prometheus scrape annotations by default (LFXV2-3221):
`prometheus.io/scrape` and `prometheus.io/path` come from `values.yaml`, while
`prometheus.io/port` is rendered from `service.port` in the TEMPLATE rather than
hardcoded in values, so the scrape port cannot drift from the port the container
listens on. `prometheus.io/port` is RESERVED: the template omits that key from the
user's `podAnnotations` before merging, so setting it there has no effect and cannot
render a duplicate annotation whose value would win. A deployment that overrides
`podAnnotations` replaces the map and must re-declare the other scrape keys. `/metrics` is scraped on the pod IP and is deliberately
absent from the HTTPRoute and RuleSet, exactly like the health probes.

Probes: `livez` restarts a hung process (never touches the DB); `readyz` gates
traffic on DB connectivity. The `startupProbe` on `/readyz` carries a ~90s
`failureThreshold` budget for a database cold start. This budget is meaningful
because the process does NOT exit when the DB is unreachable at boot — the
container boots in 503 mode and retries the pool in the background (see the
`internal/container` concept), so `/readyz` stays 503 and the pod is kept alive
across the window rather than crash-looping.

**Rollout strategy is `Recreate`, and the chart pins `Replace=true` to make that
deployable.** The Deployment sets `strategy.type: Recreate` (not the RollingUpdate
default) because this service runs its own schema migrations at boot and a
backward-incompatible one — `000014` dropping `UNIQUE (brief_id, platform)` — must not go
live while the previous pod still serves writes; RollingUpdate surges the new pod before
terminating the old, Recreate orders it the other way.

That ordering holds at ANY replica count — the controller scales the old ReplicaSet to zero
and waits for every pod to terminate before scaling up the new one — so the guarantee does not
rest on running a single replica. What DOES scale with replica count is the cost: at the three
replicas deployments run, every rollout takes the service to zero pods for the migration
window, a full restart rather than a brief gap on the only pod. The `podDisruptionBudget`
(`minAvailable: 2` in production) does not soften this: the Deployment controller DELETES those
pods rather than evicting them, and a PDB constrains only evictions. `/readyz` gates traffic
until migrations finish, so the window is a hard outage, not a period of errors. Moving
migrations to a PreSync Job is what retires this and lets RollingUpdate return.

The catch is the cutover: flipping
an EXISTING Deployment from RollingUpdate to Recreate is rejected by the API server with
`spec.strategy.rollingUpdate: Forbidden: may not be specified when strategy type is
'Recreate'`. The old object carries a `strategy.rollingUpdate` block the API server
DEFAULTED (maxSurge/maxUnavailable 25%) when it was RollingUpdate; that block is owned by
no field manager, so ArgoCD's server-side-apply sync will not strip it, and it may not
coexist with `type: Recreate`. This failure is scoped to a **pre-existing** Deployment:
it only arises when an object first created as RollingUpdate is flipped to Recreate. A
freshly created Deployment emits `type: Recreate` directly and never gets a defaulted
`rollingUpdate` block, so `Replace=true` is a one-time need per pre-existing environment,
not a standing requirement. The metadata annotation
`argocd.argoproj.io/sync-options: Replace=true` resolves this by making ArgoCD apply the
Deployment with `kubectl replace` rather than a merge/SSA patch — a full replace discards
the orphaned defaulted field, so the flip self-heals with no manual `kubectl patch`. The
chart MERGES `Replace=true` into any operator-supplied `sync-options` (that annotation is
one comma-separated value, so a bare literal ahead of `.Values.annotations` would be a
duplicate map key an override silently wins, dropping `Replace=true` and reopening the
failure) — an operator's own options are kept and `Replace=true` is always present. The
two settings are a unit: `Recreate` without `Replace=true` reopens the forbidden-cutover
failure, so `parity_test.go` pins both: `TestDeploymentUsesRecreateStrategy` asserts
`type: Recreate` and `Replace=true` on the default render, and
`TestDeploymentMergesReplaceIntoOperatorSyncOptions` asserts the merge preserves an
operator sync-option under one key and stays idempotent. A manual
one-shot recovery, if ever needed, is
`kubectl patch ... --type=merge -p '{"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}'`
— it must set `type` and null `rollingUpdate` in ONE write, because a bare remove of
`rollingUpdate` while `type` is still RollingUpdate is immediately re-defaulted.

**`Replace=true` is a transitional workaround, not an invariant.** A full replace makes
ArgoCD PUT this Deployment on every sync rather than apply it, which costs two things an
ordinary apply does not: a field present on the LIVE object but omitted from the rendered
manifest is removed, and server-side-apply field-ownership conflict detection is bypassed,
so a value another controller owns is overwritten rather than flagged. `spec.replicas` is
the concrete case here — the chart pins it AND the ArgoCD Application sets
`ignoreDifferences: /spec/replicas` with `RespectIgnoreDifferences=true`, so an ordinary sync
leaves an out-of-band `kubectl scale` or an external HPA's value alone, but a full replace
overwrites it back to `replicaCount` — which, since deployments set their own (production sets
three), is the value their own values file carries, not the chart default. That is low risk today
(no HPA in the chart), but nothing reminds anyone to remove it. It is
retired once schema migrations move out of pod boot into a PreSync Job, after which the
Deployment returns to RollingUpdate and both the strategy override and the annotation are
dropped — the cutover hazard the pairing exists to prevent no longer exists once no pod
migrates the shared schema at boot.

**Rolling back is not the deploy run backwards.** `pool.go` only ever calls `m.Up()`,
so reverting the image leaves the schema at whatever version the newer binary migrated it
to. That is harmless for a migration that only ADDS a column the old code ignores, and it
is not harmless for one that adds a CONSTRAINT the old code has no error mapping for: the
old binary meets a 23505 it was never written to see and answers 500 where the new one
answers 409. The audience build lease (`000018`) is the current example.

So for `000018` the order is **database first, then image**: run its `.down.sql` (via
`migrate ... goto 17`) while the new image is still serving, then roll the Deployment
back. The reverse order leaves a window whose length is however long the rollback takes
to notice. `000018`'s down uses `DROP INDEX CONCURRENTLY` precisely so the drop does not
block writes from the pod still serving during that window.

**Database-first is a property of the individual down migration, not a rule for the
repo.** It is safe exactly when the down is benign to the binary STILL SERVING during the
window — and database-first is the ordering that maximises that window, so a down that is
not benign is worst run first. `000018` qualifies: dropping the lease returns the new
binary to the old behaviour (two concurrent builds for one brief each create a full set of
HubSpot lists) without breaking any statement it issues. Others plainly do not.
`000005_create_campaign_audiences_table.down.sql` does `DROP TABLE campaign_audiences`,
and `000015_brief_actor_columns.down.sql` drops `created_by` / `updated_by` on
`campaign_briefs` — both remove schema the current binary reads and writes, so running
either ahead of the image rollback turns the whole window into 500s. (`000015`'s own
header already says it exists for migration symmetry, not as a routine operation: the
actor is recorded nowhere else, so its down destroys the audit trail outright.) Rollback
ORDER has to be decided per migration, and for a multi-version `goto` per migration
CROSSED — `goto 17` from 18 runs one down; `goto 4` runs fourteen, and the order is only
as safe as the least benign of them.

Two things this does not mean. It is not an argument for skipping the down migration and
leaving the constraint in place, which trades a documented procedure for a silent
error-surface regression. And it is not a claim that the newer schema is what makes the
old binary unsafe — the old binary's behaviour on the old schema is by definition what
rolling back is asking for, including whatever the new constraint existed to prevent.

An image-only rollback is also why a contract migration waits for the code it relies on to
be a release old, not just present. The campaigns slot index (`000022`'s
`uq_campaigns_brief_platform_variant_live`) is dropped by `000051` one release AFTER the per-slot
adopt/claim advisory lock ships (LFXV2-2023): the binary before the lock takes no slot lock, so
if the drop shipped with the lock, rolling the image back would leave that binary's adopt free
to race a slot-2 claim on a schema with nothing else to stop it. Staged a release apart, the
binary a rollback returns to either still has the index or already takes the lock. That also
means the lock release must BOOT on the contracted schema, not just behave on it: startup runs
`postgres.Migrate`, and the pool is closed on any error from its required-index check. So the
lock release lists the legacy index in `requiredIndexes` as `optional` — absent is a healthy boot —
and the release that ships `000051` removes the entry (and the `optional` mechanism) altogether.
The four-column `uq_campaigns_brief_platform_variant_slot_version_live` stays required throughout
and its absence still fails startup with `ErrMissingRequiredIndex`. Pinned live by
`TestMigrateRefusesADroppedDispatchIndex`. Rolling back `000051` itself (its down re-creates the
index) fails once any slot holds two live campaigns; resolve the extra versions by hand first.
The lock release reaches that check at all only because `postgres.Migrate` starts on a CLEAN
schema recorded exactly one version newer than the image (here `000051`, so the image rolled
back to must embed migrations through `000050`; #260's own release, which stops at `000046`, is
too far behind and refuses to boot) without migrating it or
rewriting its version — golang-migrate's `Up()` would otherwise refuse the unknown version first —
while still running the invalid- and required-index checks; a DIRTY newer schema, or a clean one
two or more versions ahead, is still refused. That leans on expand/contract — every release must
stay compatible with the next release's schema — since nothing marks a newer migration as
compatible. Pinned live by
`TestMigrateStartsOnACleanSchemaNewerThanThisImage`.

One limitation of that rollback is stated rather than engineered away. Dropping the index lets
the N-1 binary's `new_version` succeed at slot 2 and above, and that binary predates `000051`'s
allocation safeguards: it numbers from the latest LIVE row, claims without refusing a used
deleted version, and deletes without the slot lock. So while the image is rolled back to the
`000050` release, a `new_version` on a slot whose latest version was deleted reuses that version
and its platform name (on Microsoft, the name lookup returns the deleted upstream campaign).
Operators should not send `new_version` for such a slot until the image is rolled forward. First
creates, re-dispatches, and `new_version` on slots with no deleted version are unaffected.

**Rolling back past the multi-campaign release (cs-1, `000054`).** `000054` only ADDS two
nullable columns (`campaigns.funnel_stage`, `campaigns.tactic`) and their CHECKs, so the image
before it boots on the new schema (one version newer, clean — see above) and never reads or
writes them; leave the schema in place. What an image-only rollback DOES lose is the duplicate
protection this release added to `new_version`: the older binary ignores
`expected_slot_versions`, so a repeat of a `new_version` request (a double click after the job
finished, a reload, a second tab) creates ANOTHER campaign, and it ignores `funnel_stage` and
`tactic`, so campaigns it creates are unlabelled. The runbook is therefore, in order:

1. **Turn off the self-serve multi-campaign flag first** — remove every platform from
   `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS` (argocd values) and let self-serve roll,
   so no `new_version` request is sent while the older image serves.
2. **Resolve in-flight labelled Microsoft rows.** The older image composes Microsoft names without
   the stage token, so a labelled row it later re-dispatches (a cleared stuck claim, a retained
   partial) would miss its `| ToFU |`-named upstream campaign and create a duplicate. List them —
   `SELECT id, brief_id, slot_version, status, platform_campaign_id FROM campaigns WHERE platform =
   'microsoft-ads' AND funnel_stage IS NOT NULL AND status NOT IN ('created', 'created_degraded',
   'deleted')` — and settle each (confirm the upstream campaign and record it, or delete the row)
   before the rollback, and do not clear any such claim while rolled back.
3. Then roll the campaign-service image back.
4. Do NOT run `000054`'s down unless the labels are being abandoned: it always applies, and it
   discards every stored stage and tactic (a Microsoft campaign created with a stage keeps the
   stage token in its upstream name, which is then the only record of it).

Rolling forward again needs no data step: labelled rows keep their labels, and rows created while
rolled back are unlabelled (legacy).

**Deploying `000056` (composite tenant FK on `campaigns`, #534).** Before the rollout, run
`SELECT count(*) FROM campaigns c JOIN campaign_briefs b ON b.id = c.brief_id WHERE b.project_id
<> c.project_id` against the production database; it must be 0. A nonzero count makes `000056`
refuse (it never rewrites data), and the refusal leaves the version recorded as 56 DIRTY, so every
new pod fails at boot with `ErrDirty` and a restart does not retry it. Recovery, in order: confirm
the schema still has `campaigns_brief_id_fkey` and no `campaigns_brief_project_fkey` (the file is
one transaction, so it rolled back), resolve each mismatched row by hand, run `migrate force 55`
against the database, then restart the pods. A briefly busy table does NOT cause this: the ADD
retries lock timeouts and deadlocks with a 2s lock timeout per attempt, giving up its lock-queue
place between attempts. The 2s bounds lock ACQUISITION, not total time: failed attempts add up to
about 45s at most, and on top of that the successful attempt holds SHARE ROW EXCLUSIVE on both
tables while it validates every existing `campaigns` row, so writes wait for that scan too (reads
never wait). The scan and the precheck have no time limit of their own and scale with the table,
so check `SELECT count(*) FROM campaigns` before deploying. Giving up after 15 failed attempts also leaves version 56 dirty. Recover the same way: end the blocking transaction (find it in
`pg_stat_activity`), `migrate force 55`, restart. Rolling back past `000056` needs
no schema step: the older image writes only matching pairs, so leave the composite key in place.

**Rolling back past the Google multi-campaign release (cs-2).** No migration: cs-2 only adds
`google-ads` to the `new_version` and label allowlists and makes Google's budget, campaign and ad
group names carry the claimed row's slot suffix (`<brief>-2`), stage token and tactic. The image
before it refuses `new_version`, `funnel_stage` and `tactic` for Google with a 400, and composes
every Google name with the bare brief id and no label segments. In order:

1. **Remove `google-ads` from `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS`** (argocd
   values) and let self-serve roll, so it stops offering a second or labelled Google campaign that
   the older image would refuse.
2. **Resolve non-terminal slot ≥2 and labelled Google rows.** The older image would re-dispatch
   such a row (a cleared stuck claim, a retained partial) under slot 1's legacy names: on slot ≥2
   that is DUPLICATE_CAMPAIGN_NAME against slot 1, a DUPLICATE_NAME budget reuse looking up slot
   1's budget name, or `adoptExisting` adopting slot 1 as the second campaign; on a labelled slot 1
   it misses the `| ToFU | …`-named upstream campaign and creates a duplicate. List them —
   `SELECT id, brief_id, variant, slot_version, status, platform_campaign_id FROM campaigns WHERE
   platform = 'google-ads' AND (slot_version > 1 OR funnel_stage IS NOT NULL OR tactic IS NOT NULL)
   AND status NOT IN ('created', 'created_degraded', 'deleted')` — and settle each (confirm the
   upstream campaign and record it, or delete the row) before the rollback, and do not clear any
   such claim while rolled back. Completed rows are unaffected: they are read and toggled by id,
   and a plain retry reuses the slot's LATEST live row (slot 2 included) rather than recomposing
   a name, so they are never re-dispatched.
3. Then roll the campaign-service image back.

Rolling forward again needs no data step.

**Rolling back past the Reddit and Meta multi-campaign release (cs-3).** No migration: cs-3 only
adds `reddit-ads` and `meta-ads` to the `new_version` and label allowlists and makes their
8-segment campaign names carry the claimed row's stage (segment 8), tactic and `#<slot>` token
(segment 5). The image before it refuses `new_version`, `funnel_stage` and `tactic` for Reddit
and Meta with a 400, and composes every Reddit and Meta name in its legacy form (`Intent`, Reddit
`ToFU`, Meta `MoFU`). In order:

1. **Remove `reddit-ads` and `meta-ads` from `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS`**
   (argocd values) and let self-serve roll, so it stops offering a second or labelled Reddit or
   Meta campaign that the older image would refuse.
2. **Resolve non-terminal slot ≥2 and labelled Reddit and Meta rows.** The older image would
   re-dispatch such a row (a cleared stuck claim, a retained partial) under the legacy name.
   Neither platform refuses a duplicate name, so that creates a second paid campaign, named exactly
   like slot 1's, which the data pipeline then merges with slot 1 (its key is a hash of the 8
   segments). List them — `SELECT id, brief_id, platform, slot_version, status,
   platform_campaign_id FROM campaigns WHERE platform IN ('reddit-ads', 'meta-ads') AND
   (slot_version > 1 OR funnel_stage IS NOT NULL OR tactic IS NOT NULL) AND status NOT IN
   ('created', 'created_degraded', 'deleted')` — and settle each (confirm in the ad account
   whether a campaign exists under the row's labelled name and record it, or delete the row)
   before the rollback, and do not clear any such claim while rolled back. Completed rows are
   unaffected: they are read and toggled by id, and a plain retry reuses the slot's LATEST live
   row rather than recomposing a name.
3. Then roll the campaign-service image back.

Rolling forward again needs no data step.

**Rolling back past the LinkedIn and X multi-campaign release (cs-4).** No migration: cs-4 only
adds `linkedin-ads` and `twitter-ads` to the `new_version` and label allowlists and makes their
8-segment campaign names carry the claimed row's stage (segment 8), tactic and `#<slot>` token
(segment 5, after `Prospecting`). The image before it refuses `new_version`, `funnel_stage` and
`tactic` for LinkedIn and X with a 400, and composes every LinkedIn and X name in its legacy form
(`Prospecting`, `MoFU`). Both clients REUSE a campaign by that name on create, so the older image
is more dangerous here than on Reddit or Meta. In order:

1. **Remove `linkedin-ads` and `twitter-ads` from `LFX_CUTOVER_CAMPAIGN_SERVICE_MULTI_CAMPAIGN_PLATFORMS`**
   (argocd values) and let self-serve roll, so it stops offering a second or labelled LinkedIn or
   X campaign that the older image would refuse.
2. **Resolve non-terminal slot ≥2 and labelled LinkedIn and X rows.** The older image would
   re-dispatch such a row (a cleared stuck claim, a retained partial) under the legacy name. On
   an UNLABELLED slot ≥2 row that name is slot 1's, so the older client's by-name lookup FINDS
   slot 1's live campaign and binds slot 2 to it — on LinkedIn it then creates slot 2's creatives
   under slot 1's campaign; on X it finds slot 1's line item by its (token-free) name under that
   campaign and attaches slot 2's promoted tweet to it. On a LABELLED row (any
   slot) the legacy name matches no labelled campaign, so it misses the campaign the row already
   created and creates a duplicate under the legacy name (which, for a labelled slot ≥2 row, can
   equal slot 1's and be reused as above). List them — `SELECT id, brief_id, platform,
   slot_version, status, platform_campaign_id FROM campaigns WHERE platform IN ('linkedin-ads',
   'twitter-ads') AND (slot_version > 1 OR funnel_stage IS NOT NULL OR tactic IS NOT NULL) AND
   status NOT IN ('created', 'created_degraded', 'deleted')` — and settle each (confirm in the ad
   account whether a campaign exists under the row's labelled or `#<slot>` name and record it, or
   delete the row) before the rollback, and do not clear any such claim while rolled back.
   Completed rows are unaffected: they are read and toggled by id, and a plain retry reuses the
   slot's LATEST live row rather than recomposing a name.
3. Then roll the campaign-service image back.

Rolling forward again needs no data step.

Independently of any rollback: before clearing a stuck claim on a labelled Google or Microsoft row,
confirm in the ad account whether that attempt created a campaign. A later request that re-claims
the cleared slot carries its OWN labels, so its name differs from the cleared attempt's and the
platform's duplicate-name refusal no longer catches the repeat. The same holds for a labelled
LinkedIn or X row, where it is the client's reuse-by-name lookup that no longer finds the cleared
attempt's campaign.

See [charts/lfx-v2-campaign-service/templates/deployment.yaml](../../../charts/lfx-v2-campaign-service/templates/deployment.yaml).
