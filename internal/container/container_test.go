// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validEncryptionKey is a base64-encoded 32-byte AES-256 key for tests (not a
// secret; all-zero bytes).
func validEncryptionKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

// shrinkDBTimers shrinks the DB-init timers for the duration of a test so the
// cold-start-retry path doesn't wait real seconds.
func shrinkDBTimers(t *testing.T) {
	t.Helper()
	origTimeout, origInterval := startupDBTimeout, dbRetryInterval
	startupDBTimeout = 200 * time.Millisecond
	dbRetryInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		startupDBTimeout = origTimeout
		dbRetryInterval = origInterval
	})
}

func TestNewContainer_NoDatabase(t *testing.T) {
	cfg := &config.Config{
		Host: "*",
		Port: "8080",
	}

	cont, err := NewContainer(cfg)
	require.NoError(t, err)
	require.NotNil(t, cont)
	assert.NotNil(t, cont.Service)
	assert.NotNil(t, cont.Connections)
	require.NoError(t, cont.Close())
}

func TestNewContainer_UnsupportedEngine(t *testing.T) {
	cfg := &config.Config{
		Host:       "*",
		Port:       "8080",
		PGHost:     "localhost",
		PGPort:     "5432",
		PGUser:     "app",
		PGDatabase: "campaign",
		PGEngine:   "mysql",
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported database engine")
	assert.NotContains(t, err.Error(), "password=")
}

func TestNewContainer_IncompletePGSettings(t *testing.T) {
	cfg := &config.Config{
		Host:   "*",
		Port:   "8080",
		PGHost: "localhost",
		PGUser: "app",
		// missing PGDatabase / password → validation error
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database configuration")
	assert.Contains(t, err.Error(), "PGDATABASE")
	assert.Contains(t, err.Error(), "PGPASSWORD")
	assert.NotContains(t, err.Error(), "password=")
}

// TestNewContainer_UnreachableDBBootsIn503Mode verifies the cold-start fix: when
// the database is configured but unreachable, NewContainer does NOT fail — it
// returns a wired container (503 mode) so the process boots, and a background
// goroutine retries. This is what makes the startupProbe budget real.
func TestNewContainer_UnreachableDBBootsIn503Mode(t *testing.T) {
	shrinkDBTimers(t)
	cfg := &config.Config{
		Host: "*",
		Port: "8080",
		// Port 1 has nothing listening → connection refused (transient, retryable).
		DatabaseURL:             "postgres://app@127.0.0.1:1/campaign?sslmode=disable",
		CredentialEncryptionKey: validEncryptionKey(),
	}

	cont, err := NewContainer(cfg)
	require.NoError(t, err, "an unreachable DB must NOT fail startup — boot in 503 mode")
	require.NotNil(t, cont)
	assert.NotNil(t, cont.Service, "campaign service must be wired (reports not-ready)")
	assert.NotNil(t, cont.Connections, "connection service must be wired (returns 503)")
	// The health service must report NOT ready while the pool is still coming up
	// (distinct from no-DB mode, which reports ready).
	assert.False(t, cont.Service.(interface{ ServiceReady() bool }).ServiceReady(),
		"during a cold start /readyz must report not-ready, not OK")
	// Close must stop the background goroutine cleanly (no hang, no panic).
	require.NoError(t, cont.Close())
}

// TestNewContainer_BadEncryptionKeyFailsFast verifies a config error (not a
// transient DB problem) still fails fast — the process should exit, not boot.
func TestNewContainer_BadEncryptionKeyFailsFast(t *testing.T) {
	shrinkDBTimers(t)
	cfg := &config.Config{
		Host:                    "*",
		Port:                    "8080",
		DatabaseURL:             "postgres://app@127.0.0.1:1/campaign?sslmode=disable",
		CredentialEncryptionKey: "not-a-valid-base64-32-byte-key",
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential encryptor")
}

// TestNotReady verifies the cold-start health placeholder always reports
// not-ready (so /readyz stays 503 until the real pool is swapped in).
func TestNotReady(t *testing.T) {
	assert.False(t, notReady{}.Ready(context.Background()))
}
