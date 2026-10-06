// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The status toggle used to derive "fully provisioned" from ONE channel's shape: an ad group,
// an ad, and at least one keyword criterion. That is Search's shape and only Search's.
//
// Demand Gen has an ad group and an ad but REFUSES keywords (the Search-only fence in
// campaign.go), so the keyword gate was unsatisfiable by construction — it told the operator
// to supply exactly the field the create path rejects. Performance Max has neither an ad
// group nor an ad; its creative is an asset group, so it failed the first gate and was
// refused with a sentence about ad groups it does not have.
//
// Both channels could therefore be CREATED by this service and never ACTIVATED through it.
// These tests pin each channel's own gate, and pin that the resource that actually serves is
// the one the cascade flips.

// toggleRecorder captures the mutate path order across the cascade. The slice is written by
// the handler goroutine and read by the test goroutine, so it is mutex-guarded on both sides
// (docs/reviews/knowledge-base/test-hygiene.md, httptest-handler-state-needs-synchronized-handoff).
type toggleRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (r *toggleRecorder) record(path, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	r.bodies = append(r.bodies, body)
}

func (r *toggleRecorder) seen() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...), append([]string(nil), r.bodies...)
}

// toggleDispatcher wires a dispatcher whose API server answers every :mutate with a result
// naming the resource that was asked for, so checkStatusMutateResults is satisfied and the
// cascade runs to completion.
func toggleDispatcher(t *testing.T) (*GoogleAdsDispatcher, *toggleRecorder) {
	t.Helper()
	rec := &toggleRecorder{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.record(r.URL.Path, string(raw))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "assetGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/assetGroups/4242"}]}`)
		case strings.Contains(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.Contains(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~777"}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/777"}]}`)
		}
	}))
	t.Cleanup(apiSrv.Close)
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	return d, rec
}

func pmaxCampaign(assetGroupID string) *model.Campaign {
	result := `{"assetGroupId":"` + assetGroupID + `","googleAdsUrl":"https://ads.google.com/"}`
	if assetGroupID == "" {
		result = `{"googleAdsUrl":"https://ads.google.com/"}`
	}
	return &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Variant:            googleAdsChannelPerformanceMax,
		Result:             []byte(result),
	}
}

// demandGenCampaign carries an ad group and an ad and NO keyword criteria — the only shape a
// Demand Gen create can produce, because keywords are refused on this channel.
func demandGenCampaign() *model.Campaign {
	return &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Variant:            googleAdsChannelDemandGen,
		Result:             []byte(`{"adGroupId":"333","adId":"777","googleAdsUrl":"https://ads.google.com/"}`),
	}
}

// TestToggleStatus_PerformanceMaxActivatesTheAssetGroup is the regression this whole fix
// exists for: before it, every Performance Max row was refused here.
func TestToggleStatus_PerformanceMaxActivatesTheAssetGroup(t *testing.T) {
	d, rec := toggleDispatcher(t)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, pmaxCampaign("4242"), model.CampaignRunActive); err != nil {
		t.Fatalf("a fully provisioned Performance Max campaign must activate: %v", err)
	}
	paths, bodies := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("want an asset group mutate then a campaign mutate, got %v", paths)
	}
	// Children first, campaign last: the campaign must not report ENABLED before the
	// resource that actually serves does.
	if !strings.Contains(paths[0], "assetGroups:mutate") {
		t.Errorf("the asset group must be flipped FIRST on activate, got %v", paths)
	}
	if !strings.Contains(paths[1], "campaigns:mutate") {
		t.Errorf("the campaign must be flipped LAST on activate, got %v", paths)
	}
	// An UPDATE, masked to status. A create shape here would rewrite the group's name,
	// campaign and final URLs.
	if !strings.Contains(bodies[0], `"update"`) || !strings.Contains(bodies[0], `"updateMask":"status"`) {
		t.Errorf("the asset group flip must be a status-masked update: %s", bodies[0])
	}
	if strings.Contains(bodies[0], `"create"`) {
		t.Errorf("the asset group flip must not send a create operation: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], googleads.StatusEnabled) {
		t.Errorf("the asset group must be set to %s: %s", googleads.StatusEnabled, bodies[0])
	}
}

// TestToggleStatus_PerformanceMaxPausesCampaignBeforeAssetGroup pins the opposite order.
// Pausing the parent first stops delivery immediately even if the child flip then fails.
func TestToggleStatus_PerformanceMaxPausesCampaignBeforeAssetGroup(t *testing.T) {
	d, rec := toggleDispatcher(t)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, pmaxCampaign("4242"), model.CampaignRunPaused); err != nil {
		t.Fatalf("pausing a Performance Max campaign must succeed: %v", err)
	}
	paths, _ := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("want a campaign mutate then an asset group mutate, got %v", paths)
	}
	if !strings.Contains(paths[0], "campaigns:mutate") {
		t.Errorf("the campaign must be paused FIRST, got %v", paths)
	}
	if !strings.Contains(paths[1], "assetGroups:mutate") {
		t.Errorf("the asset group must be paused after the campaign, got %v", paths)
	}
}

// TestToggleStatus_PerformanceMaxWithoutAssetGroupIsRefusedInItsOwnWords pins both halves of
// the refusal: that it fires, and that it does not name a resource this channel never has.
func TestToggleStatus_PerformanceMaxWithoutAssetGroupIsRefusedInItsOwnWords(t *testing.T) {
	d, rec := toggleDispatcher(t)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, pmaxCampaign(""), model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("want ErrCampaignNotProvisioned, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "asset group") {
		t.Errorf("the refusal must name the asset group, got: %v", err)
	}
	// The old message sent operators looking for an ad-group provisioning failure that, on
	// this channel, cannot have happened.
	if strings.Contains(err.Error(), "ad group/ad were not fully provisioned") {
		t.Errorf("a Performance Max refusal must not be phrased in terms of ad groups: %v", err)
	}
	if paths, _ := rec.seen(); len(paths) != 0 {
		t.Errorf("the refusal is a local state check and must contact nothing, got %v", paths)
	}
}

// TestToggleStatus_DemandGenActivatesWithoutKeywords is the second half of the same defect.
// Demand Gen clears the ad-group gate and used to die on the keyword gate, which it can
// never satisfy because this client refuses keywords on the channel.
func TestToggleStatus_DemandGenActivatesWithoutKeywords(t *testing.T) {
	d, rec := toggleDispatcher(t)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, demandGenCampaign(), model.CampaignRunActive); err != nil {
		t.Fatalf("Demand Gen must activate without keywords — this client refuses keywords on the channel: %v", err)
	}
	paths, _ := rec.seen()
	if len(paths) == 0 {
		t.Fatal("the activate must reach Google")
	}
	if !strings.Contains(paths[len(paths)-1], "campaigns:mutate") {
		t.Errorf("the campaign must be flipped LAST on activate, got %v", paths)
	}
	for _, p := range paths {
		if strings.Contains(p, "assetGroups:mutate") {
			t.Errorf("Demand Gen has no asset group; nothing should flip one: %v", paths)
		}
	}
}

// TestToggleStatus_SearchStillRequiresKeywords guards the fix against over-reach. The keyword
// gate is correct ON SEARCH — a Search campaign with no keyword criterion genuinely cannot
// deliver — and dropping it everywhere would trade one false report for another.
func TestToggleStatus_SearchStillRequiresKeywords(t *testing.T) {
	d, rec := toggleDispatcher(t)
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Variant:            model.VariantDefault,
		Result:             []byte(`{"adGroupId":"333","adId":"777","googleAdsUrl":"https://ads.google.com/"}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("Search without keyword criteria must still be refused, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "keyword targeting") {
		t.Errorf("the Search refusal must still name keyword targeting, got: %v", err)
	}
	if paths, _ := rec.seen(); len(paths) != 0 {
		t.Errorf("the refusal is a local state check and must contact nothing, got %v", paths)
	}
}

// TestToggleStatus_LegacyRowKeepsTheSearchGate pins the normalisation. A row written before
// variants existed carries an empty Variant, and must keep the rules it was created under
// rather than silently falling into a laxer arm.
func TestToggleStatus_LegacyRowKeepsTheSearchGate(t *testing.T) {
	d, _ := toggleDispatcher(t)
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             []byte(`{"adGroupId":"333","adId":"777","googleAdsUrl":"https://ads.google.com/"}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("an empty variant must normalise to the Search gate, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "keyword targeting") {
		t.Errorf("want the Search keyword refusal, got: %v", err)
	}
}

// TestToggleStatus_NonNumericAssetGroupIDIsRefused pins the resource-path guard. The id is
// interpolated into a resourceName, so a non-numeric one could address something other than
// the asset group this campaign recorded — UpdateCampaignStatus has always refused the same
// shape for the same reason.
func TestToggleStatus_NonNumericAssetGroupIDIsRefused(t *testing.T) {
	d, rec := toggleDispatcher(t)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, pmaxCampaign("garbage/4242"), model.CampaignRunActive)
	if err == nil {
		t.Fatal("a non-numeric asset group id must be refused")
	}
	if !strings.Contains(err.Error(), "not numeric") {
		t.Errorf("want a numeric-id refusal, got: %v", err)
	}
	if paths, _ := rec.seen(); len(paths) != 0 {
		t.Errorf("the refusal is local and must contact nothing, got %v", paths)
	}
}
