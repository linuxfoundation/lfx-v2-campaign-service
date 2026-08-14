// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
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

// A Demand Gen claim must not be answered with the brief's SEARCH row.
//
// This is the shape the slot key exists to prevent, and the fake used to hide it: the read
// path was made variant-aware while ClaimCampaignDispatch still keyed on (brief, platform)
// alone, so a Demand Gen dispatch missed on the read, then had its claim answered with the
// existing Search campaign — reported as a reused success. The dispatcher never ran, and the
// caller was told a Demand Gen campaign existed when only a Search one did.
func TestClaimDoesNotReturnAnotherVariantsCampaign(t *testing.T) {
	repo := &fakeCampaignRepo{existing: map[string]*model.Campaign{}}
	// The brief already holds a completed SEARCH campaign in the default slot.
	// Seeded through storeRow, not by writing the map directly: storeRow is what the
	// repository uses, and it also indexes a DEFAULT-slot row under the bare
	// (brief, platform) key. Seeding only the qualified key would leave the bare key empty,
	// so a variant-blind claim would find nothing and "correctly" claim -- the test would
	// pass against the very bug it is named for.
	repo.storeRow(&model.Campaign{
		BriefID: "b1", Platform: model.ProviderGoogleAds, Variant: model.VariantDefault,
		PlatformCampaignID: "search-111", Status: model.CampaignStatusCreated,
	})

	claimed, existing, err := repo.ClaimCampaignDispatch(
		context.Background(), "cncf", "b1", model.ProviderGoogleAds, "demand-gen", "job-1", nil)
	if err != nil {
		t.Fatalf("ClaimCampaignDispatch: %v", err)
	}
	if !claimed {
		t.Fatalf("the demand-gen slot is free and must be claimable; got existing=%+v", existing)
	}
	// And the claim must record the slot it took, as the real INSERT does — otherwise the
	// row it writes is indistinguishable from a default-slot claim.
	if existing == nil || existing.Variant != "demand-gen" {
		t.Errorf("claimed row variant = %v, want demand-gen", existing)
	}
}

// Releasing one slot's claim must not release another's. The real DELETE is keyed on all
// three columns; a fake keyed on two would free a sibling variant's pending row and let a
// concurrent dispatch re-claim a slot that is still in flight.
func TestReleasingOneSlotDoesNotFreeAnother(t *testing.T) {
	repo := &fakeCampaignRepo{existing: map[string]*model.Campaign{}}
	repo.storeRow(&model.Campaign{
		BriefID: "b1", Platform: model.ProviderGoogleAds, Variant: model.VariantDefault, Status: "pending",
	})
	repo.storeRow(&model.Campaign{
		BriefID: "b1", Platform: model.ProviderGoogleAds, Variant: "demand-gen", Status: "pending",
	})

	if err := repo.DeleteDispatchClaim(context.Background(), "b1", model.ProviderGoogleAds, "demand-gen"); err != nil {
		t.Fatalf("DeleteDispatchClaim: %v", err)
	}
	if _, ok := repo.existing[slotKey("b1", model.ProviderGoogleAds, "demand-gen")]; ok {
		t.Error("the demand-gen claim was not released")
	}
	if _, ok := repo.existing[slotKey("b1", model.ProviderGoogleAds, model.VariantDefault)]; !ok {
		t.Error("releasing demand-gen also freed the SEARCH slot's in-flight claim")
	}
}

// An invalid variant must be refused BEFORE the claim, so no `_invalid` row is ever
// written and two malformed requests cannot collide on a shared slot.
//
// Routing invalid input through VariantInvalid was wrong even though no CREATE writes that
// slot — the CLAIM does. ClaimCampaignDispatch inserts a pending row keyed on the variant,
// so two concurrent malformed requests both derived VariantInvalid, one won the claim and
// the other was marked Skipped. aggregateStatus excludes skipped platforms from the failure
// tally and a wholly-skipped job terminalizes as SUCCEEDED — so the caller sent invalid
// config and was told it worked. Reported by Copilot on PR #130.
func TestOrchestrator_InvalidVariantIsRefusedAndClaimsNoSlot(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b-invalid", ProjectID: "cncf"}
	// Valid JSON, unsupported Google channel -> VariantInvalid.
	cfg := json.RawMessage(`{"googleAdsConfig":{"channel":"performance-max"}}`)

	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)

	// The job must NOT report success for a config it refused.
	if j.Status == model.JobSucceeded {
		t.Errorf("a job whose only platform had an unsupported channel reported SUCCEEDED")
	}
	// And nothing may have been created upstream.
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns for an invalid config, want 0", len(camps.upserted))
	}
}
