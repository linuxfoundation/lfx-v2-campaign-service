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

	NATSUrl string

	// DatabaseURL is the PostgreSQL DSN. Empty disables the database layer
	// (e.g. for tests or a metadata-only run). Prefer composing from PG*
	// fields via loadDatabaseFromEnv so the password is not interpolated by Helm.
	DatabaseURL string
	// CredentialEncryptionKey is the base64-encoded 32-byte AES-256 key for
	// connection credential encryption.
	CredentialEncryptionKey string

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
		NATSUrl:  envOrDefault(constants.EnvNATSURL, constants.DefaultNATSURL),

		DatabaseURL:             os.Getenv(constants.EnvDatabaseURL),
		CredentialEncryptionKey: os.Getenv(constants.EnvCredentialEncryptionKey),
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
		"&{Debug:%v Host:%q Port:%q JWKSUrl:%q Audience:%q Issuer:%q NATSUrl:%q DatabaseURL:%q CredentialEncryptionKey:%q PGHost:%q PGPort:%q PGUser:%q PGDatabase:%q PGEngine:%q}",
		c.Debug,
		c.Host,
		c.Port,
		c.JWKSUrl,
		c.Audience,
		c.Issuer,
		c.NATSUrl,
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
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, hasPass := u.User.Password(); !hasPass {
		return dsn
	}
	u.User = url.UserPassword(u.User.Username(), "xxxxx")
	return u.String()
}

func redactSecret(v string) string {
	if v == "" {
		return ""
	}
	return "xxxxx"
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
