// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
)

var goodSigningKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

// A signed-asset setting that is SET but unusable must stop the pod, not silently turn the
// feature off (LFXV2-2665).
func TestNewContainer_RejectsUnusableSignedAssetConfig(t *testing.T) {
	cases := map[string]config.Config{
		"short key":   {CreativeAssetURLSigningKey: base64.StdEncoding.EncodeToString([]byte("short")), CreativeAssetPublicBaseURL: "https://lfx-api.example.org"},
		"http base":   {CreativeAssetURLSigningKey: goodSigningKey, CreativeAssetPublicBaseURL: "http://lfx-api.example.org"},
		"ttl too big": {CreativeAssetURLSigningKey: goodSigningKey, CreativeAssetPublicBaseURL: "https://lfx-api.example.org", CreativeAssetURLTTL: "2h"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := cfg
			cfg.Host, cfg.Port = "*", "8080"
			_, err := NewContainer(&cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "signed creative-asset URL configuration")
			assert.NotContains(t, err.Error(), goodSigningKey, "the error must never quote the key")
		})
	}
}

func TestNewContainer_SignedAssetsAlwaysMounted(t *testing.T) {
	for name, cfg := range map[string]config.Config{
		"feature off": {},
		"feature on":  {CreativeAssetURLSigningKey: goodSigningKey, CreativeAssetPublicBaseURL: "https://lfx-api.example.org"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := cfg
			cfg.Host, cfg.Port = "*", "8080"
			cont, err := NewContainer(&cfg)
			require.NoError(t, err)
			require.NotNil(t, cont.SignedAssets, "the signed-asset route must be wired on every path")
			assert.Equal(t, name == "feature on", cont.assetSigner != nil)
		})
	}
}

// bindCreativeAssetSharing is what makes imageAssetId work in production: deleting the Reddit
// binding compiles and leaves every other test green while every asset-backed Reddit dispatch
// is refused as "not enabled". It also makes the Meta dispatch and the serving route share ONE
// asset-memory budget.
func TestBindCreativeAssetSharing(t *testing.T) {
	cfg := &config.Config{Host: "*", Port: "8080", CreativeAssetURLSigningKey: goodSigningKey, CreativeAssetPublicBaseURL: "https://lfx-api.example.org"}
	cont, err := NewContainer(cfg)
	require.NoError(t, err)

	m := registerDispatchers(nil, nil, nil, postgres.NewCreativeAssetRepo(nil), nil)
	cont.bindCreativeAssetSharing(m, postgres.NewCreativeAssetRepo(nil))
	rd, ok := m[model.ProviderRedditAds].(*dispatch.RedditDispatcher)
	require.True(t, ok)
	assert.True(t, rd.CreativeAssetURLsEnabled(), "the Reddit dispatcher must receive the signer")

	// ONE budget, asserted as IDENTITY: every asset-holding dispatcher and the signed-asset
	// route charge the container's reserver. A non-nil check passed while X still charged
	// registerDispatchers' own reserver (#311 review). LinkedIn holds no asset bytes and has no
	// reserver to share.
	require.NotNil(t, cont.assetReserver)
	md, ok := m[model.ProviderMetaAds].(*dispatch.MetaDispatcher)
	require.True(t, ok)
	assert.True(t, md.UsesAssetReserver(cont.assetReserver), "Meta must charge the shared reserver")
	msd, ok := m[model.ProviderMicrosoftAds].(*dispatch.MicrosoftDispatcher)
	require.True(t, ok)
	assert.True(t, msd.UsesAssetReserver(cont.assetReserver), "Microsoft must charge the shared reserver")
	td, ok := m[model.ProviderTwitterAds].(*dispatch.TwitterDispatcher)
	require.True(t, ok)
	assert.True(t, td.UsesAssetReserver(cont.assetReserver), "X must charge the shared reserver")
	assert.True(t, cont.SignedAssets.UsesBudget(cont.assetReserver), "the signed-asset route must charge the shared reserver")

	unbound := registerDispatchers(nil, nil, nil, nil, nil)
	cont.bindCreativeAssetSharing(unbound, nil)
	ru, ok := unbound[model.ProviderRedditAds].(*dispatch.RedditDispatcher)
	require.True(t, ok)
	assert.False(t, ru.CreativeAssetURLsEnabled(), "a nil store must leave the Reddit path unbound, not bound to a typed nil")
}

// Both live paths — the fast path and the cold-start retry — must call
// bindCreativeAssetSharing, or a pod that cold-started would serve no signed assets and refuse
// every Reddit imageAssetId, with nothing failing at startup.
func TestBindCreativeAssetSharingCalledOnBothLivePaths(t *testing.T) {
	src, err := os.ReadFile("container.go")
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(src), "c.bindCreativeAssetSharing(dispatchers,"),
		"bindCreativeAssetSharing must be called once in wireLiveBackends and once in retryDatabaseInit")
}
