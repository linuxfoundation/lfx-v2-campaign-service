# Code

* [cmd/campaign-service](cmd-campaign-service.md) - The LFX V2 Campaign Service.
* [internal/container](internal-container.md) - Dependency injection: opens the PostgreSQL pool, runs migrations, and wires Readyz to the pool.
* [internal/dispatch](internal-dispatch.md) - Per-platform PlatformDispatcher adapters bridging the orchestrator to the ad-platform API clients.
* [internal/infrastructure/config](internal-infrastructure-config.md) - Application configuration from CLI flags and env vars, including PG* composition into a PostgreSQL DSN.
* [internal/infrastructure/postgres](internal-infrastructure-postgres.md) - PostgreSQL pool (otelpgx), migrations, repositories, and Ready() for readiness probes.
* [internal/middleware](internal-middleware.md) - Package middleware provides HTTP middleware for the service.
* [internal/platform/reddit](internal-platform-reddit.md) - Reddit Ads API v3 client: OAuth2 token refresh and Campaign -> Ad Group -> Ad creation.
* [internal/platform/linkedin](internal-platform-linkedin.md) - LinkedIn Marketing API client: OAuth2 dark-post campaigns with targeting and up-front validation.
* [internal/platform/meta](internal-platform-meta.md) - Meta (Facebook/Instagram) Ads Graph API client: Campaign -> Ad Set -> Ad creation with objective mapping and geo/budget validation.
* [internal/platform/twitter](internal-platform-twitter.md) - X (Twitter) Ads v12 client: OAuth 1.0a signing and the campaign -> line_item -> promoted_tweet creation flow.
* [internal/platform/googleads](internal-platform-googleads.md) - Google Ads API REST client: OAuth2 refresh-token auth, request layer with 429 retry, GAQL search (GA-1), and PAUSED campaign creation via campaignBudget→campaign :mutate with the no-idempotency-key ambiguity contract (GA-2).
* [internal/platform/hubspot](internal-platform-hubspot.md) - HubSpot API client (email channel): bearer auth, request layer with 429 retry, marketing-email + CRM-list + event-def operations.
* [internal/platform/microsoft](internal-platform-microsoft.md) - Microsoft Advertising (Bing Ads) Campaign Management REST v13 client: OAuth2 refresh-token + developer-token auth, request layer with 429 retry and status-aware error classification incl. BatchErrors (MS-1), and PAUSED find-or-create Campaign->AdGroup->Ad creation over the POST /<Entity> + POST /<Entity>/QueryBy… transport with the case-insensitive-unique-name idempotency contract (MS-2/MS-2.5).
* [internal/platform/snowflake](internal-platform-snowflake.md) - Read-only Snowflake client (email channel): resolves past-edition EVENT_NAME strings from PLATINUM_LFX_ONE for HubSpot BEHAVIORAL_EVENT filters.
* [internal/service](internal-service.md) - Campaign service business logic, including Readyz (DB-backed readiness) and Livez (process-only liveness).
* [pkg/constants](pkg-constants.md) - Application-wide constants, including PG* and DATABASE_URL environment variable names.
* [pkg/log](pkg-log.md) - Package log provides structured logging utilities for context-aware logging.
* [pkg/utils](pkg-utils.md) - Package utils provides OpenTelemetry SDK setup utilities.
* [design](design.md) - Package design contains the DSL for the campaign service Goa API generation.
