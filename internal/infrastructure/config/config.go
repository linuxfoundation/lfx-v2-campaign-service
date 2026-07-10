// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package config provides application configuration loaded from CLI flags and environment variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
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
	c.PGPort = strings.TrimSpace(os.Getenv(constants.EnvPGPort))
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
			Host:   c.PGHost + ":" + c.PGPort,
			Path:   "/" + c.PGDatabase,
		}
		c.DatabaseURL = u.String()
	}
}

// ValidateDatabaseSettings validates PostgreSQL settings when any are supplied.
// An empty database configuration remains allowed (metadata-only / unit-test mode).
// Password is never included in errors.
func (c *Config) ValidateDatabaseSettings() error {
	if c == nil {
		return errors.New("config is nil")
	}

	if eng := strings.ToLower(c.PGEngine); eng != "" {
		if eng != "postgres" && eng != "postgresql" {
			return fmt.Errorf("unsupported database engine %q; only postgres is supported", c.PGEngine)
		}
	}

	// No PG* fields and no composed/explicit URL: optional database mode.
	if c.PGHost == "" && c.PGUser == "" && c.PGDatabase == "" && !c.passwordPresent && c.DatabaseURL == "" {
		return nil
	}

	// Explicit DATABASE_URL without PG* composition is fine.
	if c.DatabaseURL != "" && c.PGHost == "" && c.PGUser == "" && c.PGDatabase == "" && !c.passwordPresent {
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
	if c.DatabaseURL == "" {
		if !c.passwordPresent {
			missing = append(missing, constants.EnvPGPassword)
		}
		if len(missing) == 0 {
			return errors.New("unable to compose database URL from PostgreSQL settings")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required database settings: %s", strings.Join(missing, ", "))
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
	return fmt.Sprintf("%s:%s/%s", c.PGHost, c.PGPort, c.PGDatabase)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
