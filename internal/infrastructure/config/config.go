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
	// MockLocalPrincipal, when non-empty, disables in-app JWT verification and
	// attributes every request to this principal. Local development only; see
	// constants.EnvMockLocalPrincipal.
	MockLocalPrincipal string
	// InCluster reports whether this process is running as a Kubernetes pod. It exists
	// so auth.New can REFUSE MockLocalPrincipal in a deployment rather than trust the
	// chart's empty default; see the comment there. Derived from two independent signals
	// so that one env override cannot both enable the bypass and hide the cluster —
	// see runningInCluster.
	InCluster bool

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

// serviceAccountDir is where the kubelet projects a pod's service-account token. Its
// presence is a cluster signal that does NOT come from the environment, which is the
// whole reason it is consulted: see runningInCluster.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// runningInCluster reports whether this process is a Kubernetes pod, from the OR of two
// independent signals.
//
// KUBERNETES_SERVICE_HOST alone is NOT sufficient, and the comment that used to sit here
// claiming the chart could not set it was wrong: templates/deployment.yaml renders every
// key of app.environment and appends app.extraEnv verbatim, so an override can declare
// this exact name with an empty value — and an explicit container env entry takes
// precedence over the kubelet's service variables. The single override that enables the
// mock principal could therefore also conceal the cluster, which is precisely the
// combination the InCluster check exists to prevent.
//
// The chart now refuses to render either input (see the reserved-name guard in
// templates/deployment.yaml), so that path is closed at deploy time. This second signal
// closes it at RUNTIME as well, for the deploys that never go through the chart — a
// hand-applied manifest, a kubectl patch, an ArgoCD override. Suppressing it needs
// automountServiceAccountToken: false, which is a separate, visible, and unrelated change
// rather than one line in the same env block.
//
// Both are checked, not just the file: KUBERNETES_SERVICE_HOST still catches a pod that
// legitimately runs without an automounted token.
func runningInCluster(getenv func(string) string, saDir string) bool {
	if getenv(constants.EnvKubernetesServiceHost) != "" {
		return true
	}
	// Only a directory counts. A stat error of any kind — absent, or unreadable — is not
	// a cluster signal, and the env check above is the one that has to carry those cases.
	info, err := os.Stat(saDir)
	return err == nil && info.IsDir()
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
		// os.Getenv, not envOrDefault: there is no default for a verification
		// bypass, and unset must stay unset.
		MockLocalPrincipal: strings.TrimSpace(os.Getenv(constants.EnvMockLocalPrincipal)),
		InCluster:          runningInCluster(os.Getenv, serviceAccountDir),
		// NOT envOrDefault: an explicitly-empty NATS_URL is the documented switch
		// that disables index publishing, and envOrDefault cannot express it (it
		// collapses unset and empty into the default).
		NATSUrl: envOrDefaultUnlessSet(constants.EnvNATSURL, constants.DefaultNATSURL),

		SnowflakeAccount:    os.Getenv(constants.EnvSnowflakeAccount),
		SnowflakeUser:       os.Getenv(constants.EnvSnowflakeUser),
		SnowflakePrivateKey: os.Getenv(constants.EnvSnowflakePrivateKey),
		SnowflakeWarehouse:  os.Getenv(constants.EnvSnowflakeWarehouse),
		SnowflakeRole:       os.Getenv(constants.EnvSnowflakeRole),

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

// ResolveDatabaseURL resolves the DSN as the SERVER does without touching the process flag set,
// for subcommands (LoadConfig runs flag.Parse on the global set). Reading DATABASE_URL alone is
// wrong in-cluster: the chart injects PG* and leaves it unset. "" means no database configured.
func ResolveDatabaseURL() (string, error) {
	c := &Config{DatabaseURL: os.Getenv(constants.EnvDatabaseURL)}
	c.loadDatabaseFromEnv()
	if err := c.ValidateDatabaseSettings(); err != nil {
		return "", err
	}
	return c.DatabaseURL, nil
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
		"&{Debug:%v Host:%q Port:%q JWKSUrl:%q Audience:%q Issuer:%q NATSUrl:%q DatabaseURL:%q CredentialEncryptionKey:%q PGHost:%q PGPort:%q PGUser:%q PGDatabase:%q PGEngine:%q}",
		c.Debug,
		c.Host,
		c.Port,
		c.JWKSUrl,
		c.Audience,
		c.Issuer,
		redactNATSURL(c.NATSUrl),
		redactDatabaseURL(c.DatabaseURL),
		redactSecret(c.CredentialEncryptionKey),
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
