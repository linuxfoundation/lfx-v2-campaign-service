// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines application-wide constants.
package constants

// Environment variable names used to configure the service.
const (
	EnvPort     = "PORT"
	EnvHost     = "HOST"
	EnvDebug    = "DEBUG"
	EnvJWKSURL  = "JWKS_URL"
	EnvAudience = "JWT_AUDIENCE"
	EnvIssuer   = "JWT_ISSUER"
	// EnvMockLocalPrincipal DISABLES JWT verification and treats every request as the
	// named principal. Local development only. The chart declares it with no value and
	// the chart parity test pins it as deliberately not deployable, so setting it is a
	// choice someone has to make on a laptop, never something a deploy can do by default.
	EnvMockLocalPrincipal = "JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL"
	// EnvKubernetesServiceHost is set by the kubelet in every pod. It is read ONLY to
	// refuse EnvMockLocalPrincipal in a deployment: the chart declares that key under
	// app.environment, so an override could otherwise ship a pod with authentication
	// switched off. Nothing in the chart can set or clear this one.
	EnvKubernetesServiceHost = "KUBERNETES_SERVICE_HOST"
	// EnvNATSURL is the NATS server URL used to publish Query Service index updates.
	// Empty DISABLES indexing: every endpoint still serves and campaigns still dispatch;
	// only the search index stops being fed (the Query Service rebuilds a resource's
	// document on its next write).
	EnvNATSURL = "NATS_URL"
	// EnvIndexerServiceToken is the SERVICE credential the index relay stamps onto replayed
	// messages. Optional: with it unset the relay leaves rows pending rather than publishing
	// messages the indexer would reject for a missing authorization header.
	EnvIndexerServiceToken = "INDEXER_SERVICE_TOKEN"
	// EnvDatabaseURL is an optional PostgreSQL connection string (DSN).
	// Prefer composing from PG* variables when running in-cluster.
	EnvDatabaseURL = "DATABASE_URL"
	// EnvCredentialEncryptionKey is the base64-encoded 32-byte AES-256 key used
	// to encrypt connection credentials. Sourced from a Kubernetes secret.
	EnvCredentialEncryptionKey = "CREDENTIAL_ENCRYPTION_KEY"

	// EnvCampaignJobRetention is how long a TERMINAL campaign job (succeeded, partial or
	// failed) is kept before the retention sweeper deletes it — a Go duration string such
	// as "4320h". Optional: unset uses postgres.DefaultJobRetention (180 days).
	//
	// These rows are the audit trail of real ad spend, so both failure modes fall back to
	// the default rather than to a shorter window: an unparseable value and a non-positive
	// one both leave the default in place. Shortening retention must be an explicit,
	// well-formed operator choice, never the accidental result of a typo.
	EnvCampaignJobRetention = "CAMPAIGN_JOB_RETENTION"

	// EnvEventURLNAT64Prefixes is a comma-separated list of the deployment's
	// NETWORK-SPECIFIC RFC 6052 NAT64 translation prefixes (e.g. "2001:db8:64::/96").
	//
	// It exists because a network-specific prefix cannot be discovered in-process: it is
	// carved from the operator's own global unicast space and is indistinguishable from
	// any other public prefix by inspection. On a cluster that uses one, the translator —
	// not this service — makes the IPv4 connection, so an address encoding 169.254.169.254
	// passes every SSRF check here unless the prefix is declared. Unset is correct for a
	// cluster with no NAT64; the well-known 64:ff9b::/96 is always decoded regardless.
	EnvEventURLNAT64Prefixes = "EVENT_URL_NAT64_PREFIXES"

	// EnvRedditMetricsEnabled opts a deployment IN to Reddit metrics reads. Unset or any
	// value other than "true" leaves them off, and the metrics endpoint answers 400
	// "not supported for this campaign's platform" for Reddit campaigns.
	//
	// The flag originally existed because Reddit's reporting endpoint had no public
	// documentation (LFXV2-2995) and the request shape, response shape and spend currency
	// unit were a best-effort GUESS. That reason is retired: LFXV2-3282 replaced the guess
	// with Reddit's official public OpenAPI spec, and the client was corrected against it —
	// see internal/platform/reddit/metrics.go, whose CONTRACT SOURCE note names the spec.
	//
	// The gate nevertheless STAYS ON, because the remaining unknown is a DIFFERENT one: no
	// request has ever been made against a live Reddit ad account. Matching a published
	// schema does not establish what the endpoint returns for a campaign with no activity,
	// whether ends_at includes its final hour, or whether the account's attribution window
	// shifts the figures. An unexercised read that returns 200 looks authoritative to every
	// consumer, and the caveats live only in code comments the response never carries.
	// Default-off keeps that from reaching anyone until one live read confirms the shape, at
	// which point the default flips and this constant goes away. RedditDispatcher.ReadMetrics
	// carries the same statement at the gate itself; keep the two in step.
	//
	// It also gates the Reddit audience and keyword performance reads (get-reddit-ads-audience,
	// get-reddit-ads-keywords, LFXV2-2665, RedditDispatcher.AudienceEnabled / KeywordsEnabled),
	// which run on the same reporting operation with GENDER/COUNTRY/COMMUNITY or KEYWORD
	// breakdowns added — more of the same unexercised contract.
	EnvRedditMetricsEnabled = "REDDIT_METRICS_ENABLED"

	// EnvMicrosoftMetricsEnabled opts a deployment IN to Microsoft Advertising metrics
	// reads. Unset or any value other than "true" leaves them off, and the metrics
	// endpoint answers 400 "not supported for this campaign's platform" for Microsoft
	// campaigns — the same shape Reddit uses above, for the same reason.
	//
	// Microsoft's v13 Reporting contract was implemented from published documentation and
	// has NOT been exercised against a live Microsoft Advertising account: no credentials
	// were available. The pipeline is also unlike every other platform's — an asynchronous
	// submit/poll/download returning a zipped CSV — so there is more surface to be wrong
	// about than a single JSON GET. Default-off keeps an unverified read from looking
	// authoritative until someone runs it against a real account, at which point the
	// default flips and this constant goes away.
	EnvMicrosoftMetricsEnabled = "MICROSOFT_METRICS_ENABLED"

	// EnvRedditKeywordTargetingWritesEnabled opts a deployment IN to BOTH Reddit ad-group targeting
	// writes (LFXV2-2665): REMOVING keywords (remove-keyword-targeting) and ADDING negative keywords
	// to excluded_keywords (add-negative-keywords) — the same whole-targeting PATCH, so one switch.
	// Only the exact value "true" enables them; unset or any other value fails closed and both
	// endpoints answer 400 "not supported", the shape the metrics gates above use. The
	// keyword-targeting READ is not gated.
	//
	// Reddit has no per-keyword write: the ad group's targeting object is replaced as a whole on
	// PATCH (per secondary references to the v3 OpenAPI document, which could not be fetched from
	// the authoring environment), so the write sends back every targeting dimension it read. That
	// round trip has NOT been exercised against a live ad account, and a read representation the
	// write interprets differently would change geo, community or interest targeting on a live,
	// spending ad group. Default-off until one live write confirms it, at which point the default
	// flips and this constant goes away.
	EnvRedditKeywordTargetingWritesEnabled = "REDDIT_KEYWORD_TARGETING_WRITES_ENABLED"

	// EnvTwitterMetricsEnabled opts a deployment IN to the two X (Twitter) Ads features built on
	// X's asynchronous stats-jobs API: the ACCOUNT MONITOR
	// (GET /projects/{project_id}/connection-twitter-ads/account-monitor) and the AUDIENCE READ
	// (GET /projects/{project_id}/twitter-ads/audience). Only the exact value "true" enables them;
	// unset or any other value fails closed and both endpoints answer 400 "not supported", the
	// same shape as Reddit and Microsoft above.
	//
	// The monitor reads X's asynchronous stats-jobs API (active_entities, job create, job
	// status, a downloaded gzip results file), implemented from X's published documentation
	// and NOT exercised against a live X Ads account: no credentials were available. It gates
	// only these two stats-job features — X's per-campaign metrics read is a different, synchronous endpoint and
	// is not affected. Once the contract is verified the default flips and this constant goes
	// away.
	EnvTwitterMetricsEnabled = "TWITTER_METRICS_ENABLED"

	// EnvForceSystemAdsAccount, when exactly "true", makes the LF-owned system account
	// (model.SystemProjectID) the PRIMARY credential source for every paid-ads dispatch,
	// so every campaign authenticates as the marketing-ops account regardless of any
	// per-project connection. Read once at credsSource construction (see
	// internal/dispatch/creds.go); default-off, enabled per-environment via the ArgoCD
	// overlay like the cutover flags. Excludes HubSpot/email by construction (the forced
	// path gates on Provider.IsPaidAds()). See specs/006-force-system-ads-account.
	EnvForceSystemAdsAccount = "LFX_FORCE_SYSTEM_ADS_ACCOUNT"

	// LLM settings, used ONLY to generate email copy (LFXV2-2775). Optional as a GROUP:
	// with url or key unset, the GenerateEmailCopy endpoint returns 503 (service unavailable).
	// The service itself starts successfully with or without these configured.
	// The secret is the LF LiteLLM proxy's own key — the proxy holds the Bedrock credentials.
	// AI_MODEL is not a secret; unset selects llm.DefaultModel.
	EnvAIProxyURL = "AI_PROXY_URL"
	EnvAIAPIKey   = "AI_API_KEY"
	EnvAIModel    = "AI_MODEL"

	// Signed creative-asset URLs (LFXV2-2665): how a Reddit dispatch hands Reddit a stored
	// creative asset, which Reddit only accepts as a public URL it fetches. Optional as a
	// PAIR: with the key or the base URL unset the feature is OFF — the serving route answers
	// 404 and a Reddit dispatch that references a stored asset is refused before any upstream
	// call. EVERY value that is SET is validated, even while the feature is off for want of its
	// other half, and an unusable one (key under 32 bytes or not base64, base URL not https or
	// not a bare origin, TTL outside 5m..60m) fails startup. See
	// docs/knowledge/architecture/signed-creative-asset-urls.md.
	// The key is a SECRET (standard base64, >= 32 bytes) sourced from the chart's Secret.
	EnvCreativeAssetURLSigningKey = "CREATIVE_ASSET_URL_SIGNING_KEY"
	// The public https origin Reddit can reach this service at (the gateway host).
	EnvCreativeAssetPublicBaseURL = "CREATIVE_ASSET_PUBLIC_BASE_URL"
	// How long a minted URL is valid; a Go duration, default 10m, hard max 60m.
	EnvCreativeAssetURLTTL = "CREATIVE_ASSET_URL_TTL"

	// Snowflake (read-only) settings, used ONLY to resolve an event's past editions when
	// building an audience. All are OPTIONAL: with none set, audience building still works
	// and produces a country-only audience, recording the narrower scope in its summary.
	// A partially-configured warehouse is treated as unconfigured rather than failing boot,
	// since indexing an event's history is an enrichment, not a correctness requirement.
	EnvSnowflakeAccount    = "SNOWFLAKE_ACCOUNT"
	EnvSnowflakeUser       = "SNOWFLAKE_USER"
	EnvSnowflakePrivateKey = "SNOWFLAKE_PRIVATE_KEY"
	EnvSnowflakeWarehouse  = "SNOWFLAKE_WAREHOUSE"
	EnvSnowflakeRole       = "SNOWFLAKE_ROLE"

	// PostgreSQL connection settings (composed into a DSN in-process).
	EnvPGHost     = "PGHOST"
	EnvPGPort     = "PGPORT"
	EnvPGUser     = "PGUSER"
	EnvPGPassword = "PGPASSWORD"
	EnvPGDatabase = "PGDATABASE"
	EnvPGEngine   = "PGENGINE"
)

// Default PostgreSQL port when PGPORT is unset.
const (
	DefaultPGPort = "5432"
)

// Default configuration values. These mirror the defaults wired into the Helm
// chart so local runs match in-cluster behavior.
const (
	// DefaultHTTPPort is the default port the HTTP server listens on.
	DefaultHTTPPort = "8080"
	// DefaultHost is the default bind interface ("*" binds all interfaces).
	DefaultHost = "*"
	// DefaultJWKSURL is the default JSON Web Key Set endpoint for JWT validation.
	DefaultJWKSURL = "http://lfx-platform-heimdall.lfx.svc.cluster.local:4457/.well-known/jwks"
	// DefaultAudience is the default intended audience for JWT tokens.
	DefaultAudience = "lfx-v2-campaign-service"
	// DefaultIssuer is the default expected JWT issuer.
	DefaultIssuer = "heimdall"
	// DefaultNATSURL is the default NATS server URL.
	DefaultNATSURL = "nats://lfx-platform-nats.lfx.svc.cluster.local:4222"
)
