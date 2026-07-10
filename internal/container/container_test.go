// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	assert.NotContains(t, err.Error(), "password=")
}
