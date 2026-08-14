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
		// Undecodable config is still the dispatcher's error to report, but it must NOT claim
		// the DEFAULT slot to get there. The idempotency lookup runs before the dispatcher: on
		// a brief that already holds a Search/default campaign, the malformed request matched
		// that row and was answered as a reused success, so the dispatcher never ran and the
		// error it was meant to surface never surfaced. VariantInvalid is a slot no create
		// writes, so the lookup always misses and the dispatch reaches the real error.
		{"malformed config claims an unoccupiable slot, not the default one", model.ProviderGoogleAds, `{not json`, model.VariantInvalid},
		// An UNSUPPORTED channel must not land on a real slot either, and "default" is the
		// dangerous spelling: NormalizeVariant passes any non-empty value through, so it
		// resolved to the SEARCH slot. The idempotency fast path then found that brief's
		// existing Search campaign and returned its id as a SUCCESS -- the dispatcher never
		// ran, so its "unsupported channel" error never reached the caller, who was told a
		// campaign it never validly asked for had been created.
		{"reserved slot name does not claim the search slot", model.ProviderGoogleAds, `{"googleAdsConfig":{"channel":"default"}}`, model.VariantInvalid},
		{"an unknown channel does not claim a real slot", model.ProviderGoogleAds, `{"googleAdsConfig":{"channel":"performance-max"}}`, model.VariantInvalid},
		// The reserved sentinel itself, sent as a channel, must not round-trip onto its own
		// slot either -- it is not a channel any create writes.
		{"the invalid sentinel is not a usable channel", model.ProviderGoogleAds, `{"googleAdsConfig":{"channel":"_invalid"}}`, model.VariantInvalid},
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
