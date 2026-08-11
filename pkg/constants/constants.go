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
	// The flag exists because Reddit's reporting endpoint has no public documentation
	// (LFXV2-2995): the request shape, response shape, and spend currency unit the client
	// uses are a best-effort GUESS. A guessed read that returns 200 looks authoritative to
	// every consumer, and the caveats live only in code comments the response never carries.
	// Default-off keeps that from reaching anyone until the contract is verified against a
	// live Reddit ad account, at which point the default flips and this constant goes away.
	EnvRedditMetricsEnabled = "REDDIT_METRICS_ENABLED"

	// LLM settings, used ONLY to generate email copy (LFXV2-2775). Optional as a GROUP:
	// with url or key unset, the GenerateEmailCopy endpoint returns 503 (service unavailable).
	// The service itself starts successfully with or without these configured.
	// The secret is the LF LiteLLM proxy's own key — the proxy holds the Bedrock credentials.
	// AI_MODEL is not a secret; unset selects llm.DefaultModel.
	EnvAIProxyURL = "AI_PROXY_URL"
	EnvAIAPIKey   = "AI_API_KEY"
	EnvAIModel    = "AI_MODEL"

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
