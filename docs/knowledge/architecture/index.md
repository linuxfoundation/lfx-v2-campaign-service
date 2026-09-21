# Architecture

* [Campaign Service — API & Platform Catalog](api-catalog.md) - Reference catalog of all campaign endpoints, platform account attributes, and data structures for the Go service.
* [Campaign Service — Architecture](overview.md) - The Campaign Service is the backend for LFX Self Serve marketing campaign operations.
* [Campaign Service — Build Summary](build-summary.md) - **Status:** Architecture Review — Aligning with platform patterns
* [Campaign Connections — Database Schema](channel-connections-schema.md) - Database schema for storing per-project connections to marketing platforms.
* [MegaLinter and secret scanning](megalinter-secrets.md) - How MegaLinter, gitleaks, secretlint, and grype are configured for this repo, including local Docker runs.
* [Local pre-PR review](local-pre-pr-review.md) - Repo-owned review content for the local pre-PR review — the code-review rules and the empirical learnings knowledge base — its known unresolved target-only pattern limitation, and the interim state until the central lifecycle publishes; not the review lifecycle or its configuration.
* [GHCR stale image cleanup](ghcr-image-cleanup.md) - How the scheduled and on-demand GitHub Actions workflow removes stale GHCR image versions, tagged and untagged, for the campaign-service container package.
