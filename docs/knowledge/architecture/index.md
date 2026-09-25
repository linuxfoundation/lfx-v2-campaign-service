# Architecture

* [Campaign Service — API & Platform Catalog](api-catalog.md) - Reference catalog of all campaign endpoints, platform account attributes, and data structures for the Go service.
* [Campaign Service — Architecture](overview.md) - The Campaign Service is the backend for LFX Self Serve marketing campaign operations.
* [Campaign Service — Build Summary](build-summary.md) - **Status:** Architecture Review — Aligning with platform patterns
* [Campaign Connections — Database Schema](channel-connections-schema.md) - Database schema for storing per-project connections to marketing platforms.
* [MegaLinter and secret scanning](megalinter-secrets.md) - How MegaLinter, gitleaks, secretlint, and grype are configured for this repo, including local Docker runs.
* [Email creation wizard](email-wizard.md) - How the briefs service plans, generates, edits, clones and addresses a HubSpot email draft across turns, with a Postgres-backed session and a hand-written SSE progress stream.
* [Local pre-PR review](local-pre-pr-review.md) - How the single pre-PR review block in CLAUDE.md runs the central general reviewer and the repo-owned learnings reviewer over the whole branch once, before a PR exists.
* [Account-Monitor Endpoints](account-monitor-endpoints.md) - Four new account-scoped monitor endpoints ported from the LFX One BFF's four separate rule engines, one per ad platform.
* [GHCR stale image cleanup](ghcr-image-cleanup.md) - How the scheduled and on-demand GitHub Actions workflow removes stale GHCR image versions, tagged and untagged, for the campaign-service container package.
