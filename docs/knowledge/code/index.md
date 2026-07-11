# Code

* [cmd/campaign-service](cmd-campaign-service.md) - The LFX V2 Campaign Service.
* [internal/container](internal-container.md) - Dependency injection: opens the PostgreSQL pool, runs migrations, and wires Readyz to the pool.
* [internal/infrastructure/config](internal-infrastructure-config.md) - Application configuration from CLI flags and env vars, including PG* composition into a PostgreSQL DSN.
* [internal/infrastructure/postgres](internal-infrastructure-postgres.md) - PostgreSQL pool (otelpgx), migrations, repositories, and Ready() for readiness probes.
* [internal/middleware](internal-middleware.md) - Package middleware provides HTTP middleware for the service.
* [internal/platform/twitter](internal-platform-twitter.md) - X (Twitter) Ads v12 client: OAuth 1.0a signing and the campaign -> line_item -> promoted_tweet creation flow.
* [internal/service](internal-service.md) - Campaign service business logic, including Readyz (DB-backed readiness) and Livez (process-only liveness).
* [pkg/constants](pkg-constants.md) - Application-wide constants, including PG* and DATABASE_URL environment variable names.
* [pkg/log](pkg-log.md) - Package log provides structured logging utilities for context-aware logging.
* [pkg/utils](pkg-utils.md) - Package utils provides OpenTelemetry SDK setup utilities.
* [design](design.md) - Package design contains the DSL for the campaign service Goa API generation.
