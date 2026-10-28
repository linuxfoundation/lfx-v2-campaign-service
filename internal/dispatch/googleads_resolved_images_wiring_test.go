// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The dispatch-layer half of the stored-asset creative path (LFXV2-2665), end to end: a config
// naming assets, through the resolver, onto CampaignInput.ResolvedImages, and out as base64 in an
// assets:mutate body.
//
// Why this file has to exist. ResolvedImages is assigned at exactly ONE site —
// `ResolvedImages: resolvedImages` in googleads.go — and nothing below the dispatch layer can
// notice if that line is deleted. The resolver's own tests (googleads_creative_assets_test.go)
// still pass, because they call resolveGoogleCreativeAssets directly. The platform side's tests
// (internal/platform/googleads/resolved_images_test.go) still pass, because they construct the
// input themselves. Both suites stay green while every stored-asset campaign fails on the money
// path — a 601-style failure discovered only by Google, after the budget and campaign exist.
//
// Why ONE channel is enough. Demand Gen, Display and Performance Max share that single assignment
// and share fetchSlotImages below it; what differs between them is the per-channel mapper, and
// each of those is already pinned platform-side. Three near-identical dispatch tests would be
// three copies of one assertion.
//
// Why this reaches the ad mutate when googleads_display_wiring_test.go says it cannot. That file's
// constraint is about FETCHING: a URL-backed creative needs the image fetch's dial guard relaxed,
// and the hole would have to be exported from the one fetch that must never have one. A
// pre-resolved entry is never fetched — there is nowhere to fetch it from — so this path reaches
// assets:mutate over the ordinary fake API server with no test-only hole anywhere.

const (
	gaWireMarketingAssetID = "33333333-3333-4333-8333-333333333333"
	gaWireLogoAssetID      = "44444444-4444-4444-8444-444444444444"
)

// gaAssetLibrary serves several assets scoped to one (project, brief), as the real repository
// does, and counts GetAsset so a test can prove a refused asset was never materialised.
type gaAssetLibrary struct {
	projectID, briefID string
	assets             map[string]*model.CreativeAsset
	sizeOverrides      map[string]int64

	mu       sync.Mutex
	getCalls int
}

func (l *gaAssetLibrary) GetAsset(_ context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	l.mu.Lock()
	l.getCalls++
	l.mu.Unlock()
	if projectID != l.projectID || briefID != l.briefID {
		return nil, domain.ErrNotFound
	}
	a, ok := l.assets[assetID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return a, nil
}

func (l *gaAssetLibrary) GetAssetSize(_ context.Context, projectID, briefID, assetID string) (int64, error) {
	if projectID != l.projectID || briefID != l.briefID {
		return 0, domain.ErrNotFound
	}
	a, ok := l.assets[assetID]
	if !ok {
		return 0, domain.ErrNotFound
	}
	if n, ok := l.sizeOverrides[assetID]; ok {
		return n, nil
	}
	return int64(len(a.Bytes)), nil
}

func (l *gaAssetLibrary) loads() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.getCalls
}

// gaWireLibrary holds one image per REQUIRED Demand Gen slot: a 1.91:1 marketing image and a
// square logo. Both are real PNGs because the platform client decodes them — geometry is checked
// on the bytes, not on a declared mime type — and both are minimum-size because a near-uniform
// PNG encodes to a few hundred bytes at any dimension, so size is not what these fixtures prove.
func gaWireLibrary(t *testing.T) *gaAssetLibrary {
	t.Helper()
	brief := testBrief()
	return &gaAssetLibrary{
		projectID: brief.ProjectID,
		briefID:   brief.ID,
		assets: map[string]*model.CreativeAsset{
			gaWireMarketingAssetID: {ID: gaWireMarketingAssetID, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, 600, 314)},
			gaWireLogoAssetID:      {ID: gaWireLogoAssetID, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, 128, 128)},
		},
	}
}

// demandGenAssetWireConfig is a complete Demand Gen creative whose images are named as STORED
// ASSETS rather than URLs — the object form {"imageAssetId": "<uuid>"} that carries the id under
// the key collectSnapshotAssetIDs matches on.
func demandGenAssetWireConfig(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": map[string]any{
		"budget":  50,
		"channel": "demand-gen",
		"demandGenCreative": map[string]any{
			"marketingImages": []any{map[string]any{"imageAssetId": gaWireMarketingAssetID}},
			"logoImages":      []any{map[string]any{"imageAssetId": gaWireLogoAssetID}},
			"headlines":       []string{"Join us at KubeCon"},
			"descriptions":    []string{"Talks, workshops and hallway track."},
			"businessName":    "Linux Foundation",
		},
	}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func gaWireDispatcher(t *testing.T, store creativeAssetReader, budget *AssetReserver) (*GoogleAdsDispatcher, *targetingCapture) {
	t.Helper()
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	d.SetCreativeAssetRepo(store)
	d.SetAssetReserver(budget)
	return d, cap
}

// The whole chain, asserted on the one artefact that proves it: the STORED BYTES, base64-encoded,
// in the assets:mutate body. Nothing short of that distinguishes "the ids were accepted" from
// "the images were uploaded" — and the ids themselves never appear in the body, so asserting on
// them would pass with ResolvedImages dropped.
func TestGoogleAds_StoredAssetBytesReachTheAssetMutate(t *testing.T) {
	store := gaWireLibrary(t)
	budget := NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second)
	d, cap := gaWireDispatcher(t, store, budget)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, demandGenAssetWireConfig(t))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("nil campaign on success")
	}

	cap.mu.Lock()
	body := string(cap.assets)
	cap.mu.Unlock()
	if body == "" {
		t.Fatal("no assets:mutate call; the creative never reached the image upload")
	}
	for name, id := range map[string]string{"marketing image": gaWireMarketingAssetID, "logo": gaWireLogoAssetID} {
		want := base64.StdEncoding.EncodeToString(store.assets[id].Bytes)
		if !strings.Contains(body, want) {
			t.Errorf("the %s's stored bytes are not in the assets:mutate body; the config's asset id never became image bytes", name)
		}
		// The id is this service's own key and has no meaning to Google. Seeing it on the wire
		// would mean the resolver was skipped and the token forwarded verbatim.
		if strings.Contains(body, id) {
			t.Errorf("the %s's asset id was sent to Google verbatim", name)
		}
	}

	// Every reservation taken for the upload is released on the success path too, not only on
	// failure — the releaser runs after the dispatch, not only in the error unwind.
	rel, ok := budget.reserve(context.Background(), MaxConcurrentVariantAssetBytes)
	if !ok {
		t.Fatal("the budget was not whole after a successful dispatch; a reservation leaked")
	}
	rel()
}

// The per-image cap, from the OUTSIDE: a config whose asset is over googleads.MaxImageBytes is
// refused as a preflight refusal — nothing upstream created, no budget to orphan — and the blob
// is never loaded. Asserting it here as well as at the resolver is deliberate: this is the level
// at which "refused" has to mean "no campaign exists".
func TestGoogleAds_AnOversizedStoredAssetIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	store := gaWireLibrary(t)
	store.sizeOverrides = map[string]int64{gaWireMarketingAssetID: googleads.MaxImageBytes + 1}
	d, cap := gaWireDispatcher(t, store, NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, demandGenAssetWireConfig(t))
	if err == nil {
		t.Fatal("an asset over the per-image cap was accepted")
	}
	if camp != nil {
		t.Errorf("a campaign was returned for a refused config: %+v", camp)
	}
	if !isPreflightRefusal(err) {
		t.Errorf("err = %v, want a preflight refusal", err)
	}
	if n := store.loads(); n != 0 {
		t.Errorf("GetAsset was called %d times; the oversized asset was materialised anyway", n)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget || len(cap.campaigns) != 0 {
		t.Error("the refusal came after the campaign shell was created; it must be a preflight refusal")
	}
}
