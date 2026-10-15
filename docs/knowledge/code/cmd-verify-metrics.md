---
type: "Go Package"
title: "cmd/verify-metrics"
description: "Local, read-only operator CLI that runs a provider's gated campaign metrics read (Reddit, Microsoft, X; also LinkedIn and Meta) against a live account through the service's own connection resolution and dispatcher, with the deployment flag forced on in-process only and every secret redacted, so a contract can be verified before its flag is flipped."
resource: "cmd/verify-metrics"
---

# cmd/verify-metrics

The first step of decision D8: verify a GATED metrics contract against a live ad account BEFORE
its deployment flag is flipped. `REDDIT_METRICS_ENABLED` (#167), `MICROSOFT_METRICS_ENABLED`
(#168) and `TWITTER_METRICS_ENABLED` stay off until someone has seen the real platform answer; this
command is how they see it without turning the endpoint on for everyone.

## Usage

```sh
# Same database and key settings the service uses: DATABASE_URL or PG*, plus
# CREDENTIAL_ENCRYPTION_KEY (see README "Build and run locally").
go run ./cmd/verify-metrics -provider reddit-ads -project <slug|uuid|lf-system> \
    -campaign <platform campaign id> -window last_7_days [-raw] [-timeout 3m]
```

- `-provider`: `reddit-ads`, `microsoft-ads`, `twitter-ads`, `linkedin-ads`, `meta-ads`.
  `google-ads` is refused with a usage error (exit 2): its metrics read is not gated, and its
  client is out of scope for this tool. `hubspot` is refused too.
- `-project`: the connection scope key exactly as the service keys it (slug or UUID). `lf-system`
  is an alias for `model.SystemProjectID` (`system:linuxfoundation`). A project with no
  connection falls back to the system row exactly as the service does, and
  `LFX_FORCE_SYSTEM_ADS_ACCOUNT=true` in the environment is honoured (and reported).
- `-campaign`: the PLATFORM's campaign id, not this service's campaign UUID. If the project holds
  a campaign row with that platform id, its provenance blob is passed along so the dispatcher's
  account-match guard runs as it does in the service; otherwise the read goes by platform id with
  no provenance (the guard's documented "unknown" case), and the output says which.
- `-window`: any `model.MetricsWindow`; provider-specific window refusals come from the
  dispatcher, as they would from the endpoint.
- `-raw`: also print every upstream request/response exchange (method, URL, status, request and
  response bodies; a Microsoft report zip is unpacked into its CSV), redacted. Printed on failure
  too, since a failing read's raw response is usually the finding.

Exit status: 0 success, 1 a failed read or setup, 2 a usage error.

## Same path as the service

It builds the dispatcher with the service's own constructor (`dispatch.NewRedditDispatcher` and
siblings) on the connection repository and AES-GCM encryptor, and calls `ReadMetrics` — the
`service.MetricsReader` method `Orchestrator.ReadCampaignMetrics` calls for the published
get-campaign-metrics endpoint. Database and key loading mirror the `bootstrap-system-account`
subcommand (`config.ResolveDatabaseURL`, `CREDENTIAL_ENCRYPTION_KEY`). The only differences are
the in-process flag, the HTTP client (below), and that no platform base URL can be overridden from
the command line — tests inject one through the unexported `endpoints` seam, so a typo cannot send
live credentials to another host.

The gate is forced by setting the flag in THIS PROCESS's environment (the dispatchers read it per
call); the deployment's value is untouched.

## Read-only by construction

- The database pool is the command's own, every session `SET SESSION CHARACTERISTICS AS
  TRANSACTION READ ONLY` (pinned by a live-database test), wrapped as a `postgres.Pool` for the
  repositories.
- The dispatcher receives a connection reader exposing only `Get` and `Disconnected`, so no type
  assertion can reach a repository write, and an encryptor whose `Encrypt` is refused.
- Every upstream request passes `allowedRequest` before it is sent: GET and HEAD, plus POST only to
  an OAuth token endpoint, Reddit's `/ad_accounts/{id}/reports`, and Microsoft Reporting's
  `GenerateReport/Submit` and `/Poll`. Anything else (create, update, PATCH, PUT, DELETE) fails
  in-process and the platform never sees it. A token refresh is the one side effect on the
  platform: it mints an access token, as every service read does; nothing is persisted.

## Redaction

Every byte printed goes through a scrubber holding every secret the run observed: each string
value of the credential document the dispatcher decrypted (recorded by wrapping the encryptor), every
credential-bearing request header (Bearer tokens, OAuth 1.0a parameters, Microsoft's developer
token), and the tokens a token exchange issued. Token exchanges are printed as a status line only.
URLs go through `pkg/redact.URL` with secret-named query values (`sig`, `*token*`, …) withheld;
bodies have secret-named JSON keys withheld and embedded URLs reduced by `pkg/redact.SnapshotText`.
