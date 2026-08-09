// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package config provides application configuration loaded from CLI flags and environment variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Config holds application configuration.
type Config struct {
	Host  string
	Port  string
	Debug bool

	JWKSUrl  string
	Audience string
	Issuer   string

	// NATSUrl is the NATS server URL. Used to publish Query Service index updates; empty
	// disables indexing without affecting any other capability.
	NATSUrl string

	// Snowflake read-only credentials for audience building. Optional as a GROUP: when
	// account/user/key are not all present the warehouse is treated as unconfigured and
	// audience building degrades to country-only rather than failing.
	SnowflakeAccount    string
	SnowflakeUser       string
	SnowflakePrivateKey string
	SnowflakeWarehouse  string
	SnowflakeRole       string

	// LLM settings for email copy generation. Optional as a GROUP, on the same reasoning as
	// Snowflake above: with proxy URL or key missing the model is unconfigured and email
	// staging degrades to the cloned template's own body. Empty AIModel = llm.DefaultModel.
	AIProxyURL string
	AIAPIKey   string
	AIModel    string

	// IndexerServiceToken is the SERVICE credential the index relay stamps onto replayed
	// messages. Outbox rows store no token — the table is retained for audit, so a per-request
	// JWT written there would persist as a live credential — and the indexer requires a
	// non-empty authorization header on every message.
	IndexerServiceToken string

	// DatabaseURL is the PostgreSQL DSN. Empty disables the database layer
	// (e.g. for tests or a metadata-only run). Prefer composing from PG*
	// fields via loadDatabaseFromEnv so the password is not interpolated by Helm.
	DatabaseURL string
	// CredentialEncryptionKey is the base64-encoded 32-byte AES-256 key for
	// connection credential encryption.
	CredentialEncryptionKey string
	// EventURLNAT64Prefixes are the deployment's network-specific RFC 6052 NAT64
	// prefixes, applied by the event-URL fetcher's SSRF guard in addition to the
	// well-known 64:ff9b::/96. See constants.EnvEventURLNAT64Prefixes.
	EventURLNAT64Prefixes []string

	PGHost     string
	PGPort     string
	PGUser     string
	PGDatabase string
	PGEngine   string
	// passwordPresent is true when PGPASSWORD was non-empty (value is not retained).
	passwordPresent bool
	// pgPortPresent is true when PGPORT was explicitly set (before applying the default).
	pgPortPresent bool
}

// LoadConfig loads configuration from CLI flags, then environment variables, then defaults.
// Priority: CLI flags > env vars > defaults.
//
// For local unit tests that need LoadConfig without conflicting with the
// process-wide flag set, prefer constructing Config and calling
// loadDatabaseFromEnv / ValidateDatabaseSettings directly.
func LoadConfig() *Config {
	slog.Info("loading application configuration")

	defaultPort := os.Getenv(constants.EnvPort)
	if defaultPort == "" {
		defaultPort = constants.DefaultHTTPPort
	}
	defaultHost := os.Getenv(constants.EnvHost)
	if defaultHost == "" {
		defaultHost = constants.DefaultHost
	}

	portF := flag.String("p", defaultPort, "listen port")
	hostF := flag.String("bind", defaultHost, "interface to bind on")
	dbgF := flag.Bool("d", false, "enable debug logging")
	flag.Parse()

	cfg := &Config{
		Port:     *portF,
		Host:     *hostF,
		Debug:    *dbgF,
		JWKSUrl:  envOrDefault(constants.EnvJWKSURL, constants.DefaultJWKSURL),
		Audience: envOrDefault(constants.EnvAudience, constants.DefaultAudience),
		Issuer:   envOrDefault(constants.EnvIssuer, constants.DefaultIssuer),
		// NOT envOrDefault: an explicitly-empty NATS_URL is the documented switch
		// that disables index publishing, and envOrDefault cannot express it (it
		// collapses unset and empty into the default).
		NATSUrl: envOrDefaultUnlessSet(constants.EnvNATSURL, constants.DefaultNATSURL),

		SnowflakeAccount:    os.Getenv(constants.EnvSnowflakeAccount),
		SnowflakeUser:       os.Getenv(constants.EnvSnowflakeUser),
		SnowflakePrivateKey: os.Getenv(constants.EnvSnowflakePrivateKey),
		SnowflakeWarehouse:  os.Getenv(constants.EnvSnowflakeWarehouse),
		SnowflakeRole:       os.Getenv(constants.EnvSnowflakeRole),

		AIProxyURL: os.Getenv(constants.EnvAIProxyURL),
		AIAPIKey:   os.Getenv(constants.EnvAIAPIKey),
		AIModel:    os.Getenv(constants.EnvAIModel),

		IndexerServiceToken:     os.Getenv(constants.EnvIndexerServiceToken),
		DatabaseURL:             os.Getenv(constants.EnvDatabaseURL),
		CredentialEncryptionKey: os.Getenv(constants.EnvCredentialEncryptionKey),
		EventURLNAT64Prefixes:   splitCSV(os.Getenv(constants.EnvEventURLNAT64Prefixes)),
	}

	if os.Getenv(constants.EnvDebug) == "true" {
		cfg.Debug = true
	}

	cfg.loadDatabaseFromEnv()

	return cfg
}

// loadDatabaseFromEnv fills PostgreSQL fields from PG* environment variables
// and, when complete, composes DatabaseURL in-process so the password is not
// interpolated by Helm. An explicit DATABASE_URL is kept when PG* are incomplete.
func (c *Config) loadDatabaseFromEnv() {
	c.PGHost = strings.TrimSpace(os.Getenv(constants.EnvPGHost))
	rawPort := strings.TrimSpace(os.Getenv(constants.EnvPGPort))
	c.pgPortPresent = rawPort != ""
	c.PGPort = rawPort
	if c.PGPort == "" {
		c.PGPort = constants.DefaultPGPort
	}
	c.PGUser = strings.TrimSpace(os.Getenv(constants.EnvPGUser))
	c.PGDatabase = strings.TrimSpace(os.Getenv(constants.EnvPGDatabase))
	c.PGEngine = strings.TrimSpace(os.Getenv(constants.EnvPGEngine))

	password := os.Getenv(constants.EnvPGPassword)
	c.passwordPresent = password != ""
	if c.PGHost != "" && c.PGUser != "" && c.passwordPresent && c.PGDatabase != "" {
		u := &url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(c.PGUser, password),
			Host:   net.JoinHostPort(c.PGHost, c.PGPort),
			Path:   "/" + c.PGDatabase,
		}
		c.DatabaseURL = u.String()
	}
}

// ValidateDatabaseSettings validates PostgreSQL settings when any are supplied.
// Callers that load from the environment must run loadDatabaseFromEnv first
// (LoadConfig does this). Password is never stored on Config and is never
// included in errors.
//
// An empty database configuration remains allowed for unit tests and
// metadata-only local runs (no-DB mode). Production charts inject PG* so
// this path is not used in-cluster.
func (c *Config) ValidateDatabaseSettings() error {
	if c == nil {
		return errors.New("config is nil")
	}

	if eng := strings.ToLower(c.PGEngine); eng != "" && eng != "postgres" && eng != "postgresql" {
		return fmt.Errorf("unsupported database engine %q; only postgres is supported", c.PGEngine)
	}

	// Truly empty: no PG* intent and no composed/explicit URL → optional no-DB mode.
	// An explicit PGPORT or PGENGINE alone counts as partial configuration (FR-009).
	if c.PGHost == "" && c.PGUser == "" && c.PGDatabase == "" && !c.passwordPresent &&
		!c.pgPortPresent && c.PGEngine == "" && c.DatabaseURL == "" {
		return nil
	}

	// Explicit DATABASE_URL without any PG* composition fields is fine.
	if c.DatabaseURL != "" && c.PGHost == "" && c.PGUser == "" && c.PGDatabase == "" &&
		!c.passwordPresent && !c.pgPortPresent && c.PGEngine == "" {
		return nil
	}

	var missing []string
	if c.PGHost == "" {
		missing = append(missing, constants.EnvPGHost)
	}
	if c.PGUser == "" {
		missing = append(missing, constants.EnvPGUser)
	}
	if c.PGDatabase == "" {
		missing = append(missing, constants.EnvPGDatabase)
	}
	// Once any PG* intent exists, require PGPASSWORD even if DATABASE_URL is
	// already set — otherwise a partial PG* set can hide behind an explicit URL.
	if !c.passwordPresent {
		missing = append(missing, constants.EnvPGPassword)
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required database settings: %s", strings.Join(missing, ", "))
	}

	// PG* fields look complete but DatabaseURL is still empty. Password is not
	// retained on Config, so validation cannot recompose the URL — callers must
	// set DatabaseURL (normally via loadDatabaseFromEnv).
	if c.DatabaseURL == "" {
		return errors.New("DatabaseURL is empty despite complete PG* fields; call loadDatabaseFromEnv or set DatabaseURL")
	}
	return nil
}

// ServerAddress returns the address the HTTP server should bind to.
func (c *Config) ServerAddress() string {
	if c.Host == "*" {
		return ":" + c.Port
	}
	return c.Host + ":" + c.Port
}

// RedactedDatabaseHost returns host:port/db for safe logging (no credentials).
func (c *Config) RedactedDatabaseHost() string {
	if c == nil {
		return ""
	}
	if c.PGHost == "" {
		return ""
	}
	return net.JoinHostPort(c.PGHost, c.PGPort) + "/" + c.PGDatabase
}

// String returns a redacted representation safe for logs and debug formatting
// (FR-008). DatabaseURL passwords and CredentialEncryptionKey are masked.
// Implements fmt.Stringer so fmt verbs like %v / %+v / %s use this form.
func (c *Config) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"&{Debug:%v Host:%q Port:%q JWKSUrl:%q Audience:%q Issuer:%q NATSUrl:%q DatabaseURL:%q "+
			"CredentialEncryptionKey:%q AIProxyURL:%q AIModel:%q AIAPIKey:%q "+
			"PGHost:%q PGPort:%q PGUser:%q PGDatabase:%q PGEngine:%q}",
		c.Debug,
		c.Host,
		c.Port,
		c.JWKSUrl,
		c.Audience,
		c.Issuer,
		redactNATSURL(c.NATSUrl),
		redactDatabaseURL(c.DatabaseURL),
		redactSecret(c.CredentialEncryptionKey),
		// The AI settings are printed rather than omitted, and the key is masked rather
		// than dropped. Omission is safe but says nothing: "copy generation is not
		// running" is diagnosed by knowing WHETHER a proxy and key are configured, and
		// redactSecret answers exactly that — "" for unset, "xxxxx" for present — without
		// putting the credential in a log. The model is not a secret; the URL is reduced
		// to its non-secret components rather than printed raw (see redactAIProxyURL).
		//
		// All three are trimmed FIRST, because llm.NewClient trims them and stores the
		// normalized form: these arrive from Kubernetes secrets, where a trailing newline
		// is the commonest way a value is malformed. Printing the originals would make the
		// diagnostic disagree with construction on exactly the values it exists to report —
		// a newline-only URL would render as `[redacted]` and a newline-only key as
		// `xxxxx`, both reading as CONFIGURED, while NewClient returns ErrNotConfigured for
		// them; and a padded model would be logged differently from the model actually sent.
		redactAIProxyURL(strings.TrimSpace(c.AIProxyURL)),
		strings.TrimSpace(c.AIModel),
		redactSecret(strings.TrimSpace(c.AIAPIKey)),
		c.PGHost,
		c.PGPort,
		c.PGUser,
		c.PGDatabase,
		c.PGEngine,
	)
}

// GoString implements fmt.GoStringer so %#v also uses the redacted form.
func (c *Config) GoString() string {
	return c.String()
}

func redactDatabaseURL(dsn string) string {
	if dsn == "" {
		return ""
	}
	// Always mask: keyword DSNs, malformed URLs, and query-embedded
	// passwords are not safely parseable as userinfo (FR-008).
	return "[redacted]"
}

// redactNATSURL strips any credentials from a NATS URL while KEEPING the host.
//
// A NATS URL may carry userinfo (nats://user:pass@host:4222), and String() promises a
// log-safe representation — so printing it verbatim would put the broker password in the
// pod logs of anything that logs the config. Unlike redactDatabaseURL this does not mask
// wholesale: the broker host is genuinely useful when diagnosing an indexing outage, and
// a NATS URL is always a parseable URL (there is no keyword-DSN form to worry about), so
// the credential portion can be removed precisely.
func redactNATSURL(u string) string {
	at := strings.LastIndexByte(u, '@')
	if at < 0 {
		return u // no userinfo: nothing to redact
	}
	if scheme := strings.Index(u, "://"); scheme >= 0 && scheme+3 <= at {
		return u[:scheme+3] + "***@" + u[at+1:]
	}
	return "***@" + u[at+1:]
}

// redactAIProxyURL reduces AI_PROXY_URL to its scheme, with the host masked. Everything
// else — userinfo, host, path, query, fragment — is dropped or replaced.
//
// The value LOOKS secret-free — it is a service endpoint, and the key that authenticates
// to it lives in its own field — but "looks secret-free" is not a property of the field,
// it is a property of whatever an operator typed into it. A URL has two places a secret
// rides for free: userinfo (`https://user:token@proxy/`, which Go's transport turns into
// a Basic credential) and the query (`?api-key=…`, the shape several LiteLLM deployments
// document). Both survive `%q` intact, and String() is the form every config log line
// uses, so printing raw makes the pod log the disclosure channel.
//
// Four rounds of review narrowed this to one rule, and each round narrowed it the same
// way: url.Parse decides where the DELIMITERS fall, never what a component CONTAINS. A
// pasted credential lands in the scheme ("sk-secret://host" parses cleanly), in the path
// ("https://proxy/sk-secret/v1"), and — the case that closed the last gap — in the HOST,
// because `AI_PROXY_URL=https://sup3r-s3cret/` is a well-formed absolute URL whose entire
// informative content is the token. Each round the surviving component was defended as
// "structurally safe"; each time that meant "url.Parse put it in this field", which is
// not the same claim. So the rule is: reproduce a component only when it is BOTH
// structurally incapable of holding a secret AND load-bearing for the diagnosis.
//
// Exactly one component clears that bar. The scheme is reproduced only after it is
// checked to be literally "http" or "https", so what is printed is one of two constants
// this function chose — it cannot carry operator input at all. The host is masked to
// `xxxxx`, the same marker redactSecret uses, which still answers the question this
// string exists to answer: whether a proxy is configured, and whether it is being reached
// over TLS. "Which proxy" is not worth a credential-shaped host in a pod log, and an
// operator who needs it has the deployment manifest.
//
// Note this is redaction for DISPLAY only; llm.NewClient REJECTS a proxy URL carrying
// userinfo, a query or a fragment, so a value reaching a live client has none of them —
// but it accepts a path and any host, and String() runs before it in any case.
func redactAIProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	// A missing Host is the tell for an OPAQUE url ("mailto:u:p@host"), whose entire
	// content lands in a field this function does not render and therefore cannot
	// vouch for — url.Parse only splits out userinfo for an authority-form url. Both
	// that and an unparseable value mask, for the same reason: nothing is known safe.
	if err != nil || u.Host == "" {
		return "[redacted]"
	}
	// A non-http(s) scheme masks wholesale rather than rendering as `scheme://xxxxx`,
	// because the scheme is the only component this function still reproduces and an
	// unrecognised one is exactly the case where it may be the secret.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "[redacted]"
	}
	return u.Scheme + "://xxxxx"
}

// splitCSV parses a comma-separated env var into its non-empty, space-trimmed entries.
//
// Returns nil for an empty value AND for an all-blank one — the two are deliberately NOT
// distinguished. There is no configuration a caller could express with `","` that it cannot
// express by leaving the variable unset, so treating them alike removes a state rather than
// hiding one. Blank entries are dropped rather than passed through: a trailing comma or an
// "a, ,b" typo would otherwise become an empty string that every consumer has to re-check,
// and the one consumer here PANICS on a value it cannot parse.
func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func redactSecret(v string) string {
	if v == "" {
		return ""
	}
	return "xxxxx"
}

// envOrDefaultUnlessSet returns def only when key is UNSET. A key that is present
// but empty returns "" — unlike envOrDefault, which treats empty as absent. Use this
// for settings where empty is a meaningful operator choice rather than a mistake
// (NATS_URL="" disables index publishing; see constants.EnvNATSURL).
func envOrDefaultUnlessSet(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
