// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The bug this column exists to fix, observed end-to-end on 2026-08-13: a brief that
// already held a Search campaign reported a Demand Gen dispatch as ALREADY DONE and
// returned the Search campaign's platform id, without ever calling Google. Both are
// platform="google-ads", so the (brief, platform) idempotency lookup could not tell
// them apart.
//
// variantForDispatch is what separates them. These pin the mapping directly: an
// orchestrator-level test would exercise the same function through several layers of
// fake, and would still pass if the lookup silently ignored the value.
func TestVariantForDispatch(t *testing.T) {
	cases := []struct {
		name     string
		platform model.Provider
		config   string
		want     string
	}{
		// Explicit "search" shares the DEFAULT slot rather than claiming its own: it
		// dispatches the identical Search campaign as an absent channel, and every row
		// created before this column was backfilled to 'default'. Giving it a separate slot
		// meant the updated UI would miss an older brief's existing campaign and create a
		// SECOND paid Search campaign. Caught in review of #130.
		{"explicit search shares the default slot", model.ProviderGoogleAds, `{"googleAdsConfig":{"channel":"search"}}`, model.VariantDefault},
		{"google demand-gen is a DIFFERENT slot", model.ProviderGoogleAds, `{"googleAdsConfig":{"channel":"demand-gen"}}`, "demand-gen"},
		// Absent channel means Search upstream, but the SLOT is 'default': every caller
		// written before this column omits it, and their existing rows were backfilled to
		// 'default'. Mapping absence to "search" instead would put a new dispatch in a
		// different slot from the campaign it is retrying, and create a duplicate.
		{"absent channel keeps the default slot", model.ProviderGoogleAds, `{"googleAdsConfig":{"budget":500}}`, model.VariantDefault},
		{"no config at all", model.ProviderGoogleAds, ``, model.VariantDefault},
		// Every other provider: one campaign per brief, unchanged in behaviour.
		{"meta ignores channel entirely", model.ProviderMetaAds, `{"metaConfig":{"objective":"traffic"}}`, model.VariantDefault},
		{"reddit ignores channel entirely", model.ProviderRedditAds, `{"redditConfig":{"objective":"awareness"}}`, model.VariantDefault},
		{"hubspot", model.ProviderHubSpot, `{"hubspotConfig":{"sourceEmailId":"e-1"}}`, model.VariantDefault},
		// Undecodable config is the dispatcher's error to report. Claiming the default slot
		// lets the dispatch reach that specific error instead of failing here vaguely.
		{"malformed config falls back rather than failing", model.ProviderGoogleAds, `{not json`, model.VariantDefault},
		// Case and whitespace are normalized so "Demand-Gen " cannot claim a third slot
		// alongside "demand-gen" — two rows that both mean the same campaign.
		{"case and space are normalized", model.ProviderGoogleAds, `{"googleAdsConfig":{"channel":"  Demand-Gen "}}`, "demand-gen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg json.RawMessage
			if tc.config != "" {
				cfg = json.RawMessage(tc.config)
			}
			if got := variantForDispatch(tc.platform, cfg); got != tc.want {
				t.Errorf("variantForDispatch(%s, %s) = %q, want %q", tc.platform, tc.config, got, tc.want)
			}
		})
	}
}

// Search and Demand Gen must land in DIFFERENT slots. This is the whole point: if they
// collide, the second dispatch reads the first's campaign and reports a false success.
func TestVariantForDispatchSeparatesGoogleChannels(t *testing.T) {
	search := variantForDispatch(model.ProviderGoogleAds, json.RawMessage(`{"googleAdsConfig":{"channel":"search"}}`))
	absent := variantForDispatch(model.ProviderGoogleAds, json.RawMessage(`{"googleAdsConfig":{"budget":500}}`))
	demandGen := variantForDispatch(model.ProviderGoogleAds, json.RawMessage(`{"googleAdsConfig":{"channel":"demand-gen"}}`))
	if search != absent {
		t.Errorf("explicit search (%q) and absent (%q) must SHARE a slot — they dispatch the same campaign, and a split would double-create on an older brief", search, absent)
	}
	if search == demandGen {
		t.Fatalf("search and demand-gen resolved to the same slot %q — one brief could not hold both, which is the bug this column fixes", search)
	}
}
