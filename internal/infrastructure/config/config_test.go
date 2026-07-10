// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package config

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDatabaseSettings_Success(t *testing.T) {
	cfg := &Config{
		PGHost:      "db.example.com",
		PGPort:      "5432",
		PGUser:      "campaign",
		PGDatabase:  "campaign",
		PGEngine:    "postgres",
		DatabaseURL: "postgres://campaign:secret@db.example.com:5432/campaign",
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
		PGHost:      "db.example.com",
		PGPort:      "5432",
		PGUser:      "campaign",
		PGDatabase:  "campaign",
		PGEngine:    "mysql",
		DatabaseURL: "postgres://campaign:secret@db.example.com:5432/campaign",
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
