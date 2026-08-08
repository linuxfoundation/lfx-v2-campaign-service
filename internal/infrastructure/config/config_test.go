// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package config

import (
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDatabaseSettings_Success(t *testing.T) {
	cfg := &Config{
		PGHost:          "db.example.com",
		PGPort:          "5432",
		PGUser:          "campaign",
		PGDatabase:      "campaign",
		PGEngine:        "postgres",
		DatabaseURL:     "postgres://campaign:secret@db.example.com:5432/campaign",
		passwordPresent: true,
	}
	assert.NoError(t, cfg.ValidateDatabaseSettings())
}

func TestValidateDatabaseSettings_EmptyOptional(t *testing.T) {
	cfg := &Config{}
	assert.NoError(t, cfg.ValidateDatabaseSettings())
}

func TestValidateDatabaseSettings_ExplicitDatabaseURL(t *testing.T) {
	cfg := &Config{DatabaseURL: "postgres://campaign:secret@db.example.com:5432/campaign"}
	assert.NoError(t, cfg.ValidateDatabaseSettings())
}

func TestValidateDatabaseSettings_MissingFields(t *testing.T) {
	cfg := &Config{PGHost: "localhost"}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGUSER")
	assert.Contains(t, err.Error(), "PGDATABASE")
	assert.NotContains(t, err.Error(), "secret")
}

func TestValidateDatabaseSettings_UnsupportedEngine(t *testing.T) {
	cfg := &Config{
		PGHost:          "db.example.com",
		PGPort:          "5432",
		PGUser:          "campaign",
		PGDatabase:      "campaign",
		PGEngine:        "mysql",
		DatabaseURL:     "postgres://campaign:secret@db.example.com:5432/campaign",
		passwordPresent: true,
	}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported database engine")
}

func TestLoadDatabaseFromEnv_ComposesURL(t *testing.T) {
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "5433")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "s3cret-value")
	t.Setenv("PGDATABASE", "campaign")
	t.Setenv("PGENGINE", "postgresql")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()

	require.NoError(t, cfg.ValidateDatabaseSettings())
	assert.Equal(t, "localhost", cfg.PGHost)
	assert.Equal(t, "5433", cfg.PGPort)
	assert.Equal(t, "app", cfg.PGUser)
	assert.Equal(t, "campaign", cfg.PGDatabase)

	u, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	assert.Equal(t, "postgres", u.Scheme)
	assert.Equal(t, "app", u.User.Username())
	pass, ok := u.User.Password()
	require.True(t, ok)
	assert.Equal(t, "s3cret-value", pass)
	assert.Equal(t, "localhost:5433", u.Host)
	assert.Equal(t, "/campaign", u.Path)

	redacted := cfg.RedactedDatabaseHost()
	assert.Equal(t, "localhost:5433/campaign", redacted)
	assert.NotContains(t, redacted, "s3cret")
}

func TestLoadDatabaseFromEnv_PasswordSpecialCharacters(t *testing.T) {
	const password = "p@ss:w/ord"
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", password)
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()

	require.NoError(t, cfg.ValidateDatabaseSettings())
	u, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	pass, ok := u.User.Password()
	require.True(t, ok)
	assert.Equal(t, password, pass)
	assert.Contains(t, cfg.DatabaseURL, "p%40ss%3Aw%2Ford")
	assert.NotContains(t, cfg.RedactedDatabaseHost(), password)
}

func TestValidateDatabaseSettings_CompleteFieldsMissingURL(t *testing.T) {
	cfg := &Config{
		PGHost:          "localhost",
		PGPort:          "5432",
		PGUser:          "app",
		PGDatabase:      "campaign",
		passwordPresent: true,
		// DatabaseURL intentionally unset — simulates hand-built Config
		// without loadDatabaseFromEnv.
	}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DatabaseURL is empty")
	assert.Contains(t, err.Error(), "loadDatabaseFromEnv")
}

func TestValidateDatabaseSettings_DatabaseURLWithPartialPGRequiresPassword(t *testing.T) {
	// Explicit DATABASE_URL must not mask a partial PG* set (FR-009).
	cfg := &Config{
		PGHost:      "localhost",
		PGUser:      "app",
		PGDatabase:  "campaign",
		DatabaseURL: "postgres://app:secret@localhost:5432/campaign",
	}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGPASSWORD")
}

func TestLoadDatabaseFromEnv_IPv6Host(t *testing.T) {
	t.Setenv("PGHOST", "2001:db8::1")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "x")
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()

	u, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	assert.Equal(t, "[2001:db8::1]:5432", u.Host)
	assert.Equal(t, "[2001:db8::1]:5432/campaign", cfg.RedactedDatabaseHost())
}

func TestLoadDatabaseFromEnv_IncompleteSkipsURL(t *testing.T) {
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	assert.Empty(t, cfg.DatabaseURL)
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cret")
}

func TestValidateDatabaseSettings_PartialEngineOnly(t *testing.T) {
	cfg := &Config{PGEngine: "postgres"}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGHOST")
}

func TestValidateDatabaseSettings_PartialPortOnly(t *testing.T) {
	cfg := &Config{PGPort: "5432", pgPortPresent: true}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGHOST")
}

func TestLoadDatabaseFromEnv_EngineOnlyFailsValidation(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "")
	t.Setenv("PGENGINE", "postgres")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required database settings")
}

func TestLoadDatabaseFromEnv_PortOnlyFailsValidation(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "")
	t.Setenv("PGENGINE", "")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	assert.True(t, cfg.pgPortPresent)
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required database settings")
}

func TestLoadDatabaseFromEnv_DefaultPort(t *testing.T) {
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "x")
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	assert.Equal(t, "5432", cfg.PGPort)
}

func TestConfigString_RedactsSecrets(t *testing.T) {
	cfg := &Config{
		Host:                    "*",
		Port:                    "8080",
		DatabaseURL:             "postgres://campaign:s3cret-value@db.example.com:5432/campaign", // secretlint-disable-line -- intentional fake DSN
		CredentialEncryptionKey: "TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE=",
		PGHost:                  "db.example.com",
		PGUser:                  "campaign",
		PGDatabase:              "campaign",
		passwordPresent:         true,
	}

	for _, formatted := range []string{
		cfg.String(),
		cfg.GoString(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
	} {
		assert.NotContains(t, formatted, "s3cret-value")
		assert.NotContains(t, formatted, "TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE=")
		assert.Contains(t, formatted, "[redacted]")
		assert.Contains(t, formatted, "xxxxx")
		assert.Contains(t, formatted, "db.example.com")
	}
}

func TestConfigString_RedactsKeywordAndMalformedDSN(t *testing.T) {
	cases := []string{
		"host=localhost user=app password=s3cret-value dbname=campaign",
		"://not-a-valid-url password=s3cret-value",
		"postgres://campaign@db.example.com:5432/campaign?password=s3cret-value",
	}
	for _, dsn := range cases {
		cfg := &Config{DatabaseURL: dsn}
		formatted := cfg.String()
		assert.NotContains(t, formatted, "s3cret-value", "dsn=%q", dsn)
		assert.Contains(t, formatted, "[redacted]", "dsn=%q", dsn)
	}
}

// TestEnvOrDefaultUnlessSet_DistinguishesUnsetFromEmpty pins the ONLY behavioural
// difference from envOrDefault: an explicitly-empty value survives instead of
// falling through to the default. This is what makes NATS_URL="" a usable
// disable switch for index publishing.
func TestEnvOrDefaultUnlessSet_DistinguishesUnsetFromEmpty(t *testing.T) {
	const key = "LFX_TEST_UNSET_VS_EMPTY"

	t.Run("unset falls back to the default", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(key))
		if got := envOrDefaultUnlessSet(key, "fallback"); got != "fallback" {
			t.Fatalf("unset: got %q, want %q", got, "fallback")
		}
	})

	t.Run("explicitly empty stays empty", func(t *testing.T) {
		t.Setenv(key, "")
		// envOrDefault would return "fallback" here — that difference is the point.
		if got := envOrDefaultUnlessSet(key, "fallback"); got != "" {
			t.Fatalf("explicit empty: got %q, want %q (the disable switch is unreachable)", got, "")
		}
	})

	t.Run("set wins over the default", func(t *testing.T) {
		t.Setenv(key, "nats://custom:4222")
		if got := envOrDefaultUnlessSet(key, "fallback"); got != "nats://custom:4222" {
			t.Fatalf("set: got %q, want %q", got, "nats://custom:4222")
		}
	})
}

// TestConfigString_RedactsNATSCredentials pins that String() does not leak the broker password.
// A NATS URL may carry userinfo (nats://user:pass@host:4222), String() promises a log-safe
// representation, and anything that logs the config would otherwise put that password in the
// pod logs. The host is deliberately KEPT — it is what makes an indexing outage diagnosable.
func TestConfigString_RedactsNATSCredentials(t *testing.T) {
	cfg := &Config{NATSUrl: "nats://svcuser:sup3r-s3cret@nats.lfx.svc:4222"} // secretlint-disable-line -- fixture asserting the password is redacted

	for _, formatted := range []string{cfg.String(), cfg.GoString(), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg)} {
		assert.NotContains(t, formatted, "sup3r-s3cret", "the broker password must never reach a log line")
		assert.NotContains(t, formatted, "svcuser", "the broker username is part of the credential")
		// The host must survive: redacting wholesale would make an outage undiagnosable.
		assert.Contains(t, formatted, "nats.lfx.svc:4222")
	}
}

// TestRedactNATSURL_Shapes covers the forms a NATS URL actually takes, including the ones with
// nothing to redact (where masking would needlessly hide the host).
func TestRedactNATSURL_Shapes(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"nats://nats.lfx.svc:4222":      "nats://nats.lfx.svc:4222",
		"nats://u:p@nats.lfx.svc:4222":  "nats://***@nats.lfx.svc:4222", // secretlint-disable-line -- fixture
		"nats://token@nats.lfx.svc:422": "nats://***@nats.lfx.svc:422",
		"u:p@host:4222":                 "***@host:4222", // secretlint-disable-line -- fixture: no scheme
	}
	for in, want := range cases {
		assert.Equal(t, want, redactNATSURL(in), "input %q", in)
	}
}

// TestResolveDatabaseURL: the subcommand path must compose the same DSN the server does from the
// PG* the chart injects, not read DATABASE_URL alone — which is unset in-cluster.
func TestResolveDatabaseURL(t *testing.T) {
	t.Setenv("PGHOST", "db.internal")
	t.Setenv("PGUSER", "svc")
	t.Setenv("PGPASSWORD", "p@ss word")
	t.Setenv("PGDATABASE", "campaigns")
	dsn, err := ResolveDatabaseURL()
	assert.NoError(t, err)
	assert.Equal(t, "postgres://svc:p%40ss%20word@db.internal:5432/campaigns", dsn)
}
