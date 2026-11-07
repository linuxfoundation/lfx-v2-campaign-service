// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// #477: the Search campaign assets and shared negative keyword lists must reach the outbound
// requests through the wire config, resolve stored-asset entries like every other image slot,
// and be sanitized in the persisted snapshot.

const (
	gaSearchImageAssetID = "55555555-5555-4555-8555-555555555555"
	gaSearchLogoAssetID  = "66666666-6666-4666-8666-666666666666"
	// gaSearchLandscapeAssetID is a stored 1.91:1 image for searchAssets.landscapeAdImages.
	gaSearchLandscapeAssetID = "77777777-7777-4777-8777-777777777777"
	gaSharedSet              = "customers/1234567890/sharedSets/501"
)

// lastSegment is the text after the final "/" of a resource name.
func lastSegment(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// searchAssetMutateResults answers a googleAds:mutate carrying asset and campaign-asset
// operations: temporary asset -n becomes asset 800+n-1, and each link names the real asset
// and the field type its operation asked for.
func searchAssetMutateResults(body []byte) (string, error) {
	type create struct {
		ResourceName string `json:"resourceName"`
		Asset        string `json:"asset"`
		FieldType    string `json:"fieldType"`
	}
	var req struct {
		MutateOperations []struct {
			AssetOperation *struct {
				Create create `json:"create"`
			} `json:"assetOperation"`
			CampaignAssetOperation *struct {
				Create create `json:"create"`
			} `json:"campaignAssetOperation"`
		} `json:"mutateOperations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", err
	}
	real := func(temp string) string {
		var n int
		_, _ = fmt.Sscanf(lastSegment(temp), "-%d", &n)
		return fmt.Sprintf("%d", 800+n-1)
	}
	const prefix = `{"resourceName":"customers/1234567890/`
	parts := make([]string, 0, len(req.MutateOperations))
	for _, op := range req.MutateOperations {
		switch {
		case op.AssetOperation != nil:
			id := real(op.AssetOperation.Create.ResourceName)
			parts = append(parts, `{"assetResult":`+prefix+`assets/`+id+`"}}`)
		case op.CampaignAssetOperation != nil:
			c := op.CampaignAssetOperation.Create
			parts = append(parts, `{"campaignAssetResult":`+prefix+
				`campaignAssets/222~`+real(c.Asset)+`~`+c.FieldType+`"}}`)
		}
	}
	return `{"mutateOperationResponses":[` + strings.Join(parts, ",") + `]}`, nil
}

// campaignSharedSetResults answers a campaignSharedSets:mutate with one link per operation.
func campaignSharedSetResults(body []byte) (string, error) {
	var req struct {
		Operations []struct {
			Create struct {
				Campaign  string `json:"campaign"`
				SharedSet string `json:"sharedSet"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(req.Operations))
	for _, op := range req.Operations {
		parts = append(parts, `{"resourceName":"customers/1234567890/campaignSharedSets/`+
			lastSegment(op.Create.Campaign)+`~`+lastSegment(op.Create.SharedSet)+`"}`)
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`, nil
}

// searchAssetLibrary holds a square ad image and a square logo for the brief.
func searchAssetLibrary(t *testing.T) *gaAssetLibrary {
	t.Helper()
	brief := testBrief()
	png := func(id string, side int) *model.CreativeAsset {
		return &model.CreativeAsset{
			ID: id, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, side, side),
		}
	}
	return &gaAssetLibrary{
		projectID: brief.ProjectID,
		briefID:   brief.ID,
		assets: map[string]*model.CreativeAsset{
			gaSearchImageAssetID: png(gaSearchImageAssetID, 300),
			gaSearchLogoAssetID:  png(gaSearchLogoAssetID, 128),
			gaSearchLandscapeAssetID: {
				ID: gaSearchLandscapeAssetID, MimeType: model.MimeTypePNG,
				Bytes: msTestPNG(t, 600, 314),
			},
		},
	}
}

// The whole chain: stored-asset entries under searchAssets resolve to bytes that reach the
// googleAds:mutate, linked under the right field types, and the shared lists reach the attach.
func TestGoogleAds_SearchAssetsAndSharedListsReachTheCascade(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsSharedNegativeListsEnabled, "true")
	store := searchAssetLibrary(t)
	budget := NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second)
	d, cap := gaWireDispatcher(t, store, budget)
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": map[string]any{
		"budget": 50,
		"searchAssets": map[string]any{
			"adImages":           []any{map[string]any{"imageAssetId": gaSearchImageAssetID}},
			"landscapeAdImages":  []any{map[string]any{"imageAssetId": gaSearchLandscapeAssetID}},
			"businessLogoImages": []any{map[string]any{"imageAssetId": gaSearchLogoAssetID}},
			"businessName":       "Linux Foundation",
		},
		"sharedNegativeKeywordLists": []string{gaSharedSet},
	}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, raw)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	googleAds, shared := cap.googleAdsMutates, cap.sharedSets
	cap.mu.Unlock()

	if len(googleAds) != 1 {
		t.Fatalf("got %d googleAds:mutate calls, want 1", len(googleAds))
	}
	body := string(googleAds[0])
	stored := map[string]string{
		"ad image": gaSearchImageAssetID, "landscape ad image": gaSearchLandscapeAssetID,
		"logo": gaSearchLogoAssetID,
	}
	for name, id := range stored {
		if !strings.Contains(body, base64.StdEncoding.EncodeToString(store.assets[id].Bytes)) {
			t.Errorf("the %s's stored bytes are not in the googleAds:mutate body", name)
		}
		if strings.Contains(body, id) {
			t.Errorf("the %s's asset id was sent to Google verbatim", name)
		}
	}
	// Both ad image shapes link under the one AD_IMAGE field type (#538).
	if n := strings.Count(body, `"fieldType":"AD_IMAGE"`); n != 2 {
		t.Errorf("googleAds:mutate body carries %d AD_IMAGE links, want 2 (square and landscape)", n)
	}
	for _, want := range []string{
		`"fieldType":"BUSINESS_LOGO"`,
		`"fieldType":"BUSINESS_NAME"`, `"text":"Linux Foundation"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("googleAds:mutate body lacks %s", want)
		}
	}
	if len(shared) != 1 || !strings.Contains(string(shared[0]), `"sharedSet":"`+gaSharedSet+`"`) {
		t.Errorf("campaignSharedSets bodies = %s, want one naming sharedSets/501", shared)
	}
}

// Both fields are refused before the budget on every channel but Search, and another account's
// shared set and an over-long name are refused before the budget on Search. The shared-list
// gate is ON here, so these are the client's own checks, kept as defence in depth.
func TestGoogleAds_SearchAssetFieldsAreRefusedBeforeAnyCreate(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsSharedNegativeListsEnabled, "true")
	const (
		searchOnly   = "supported on Search campaigns only"
		listsRefused = "shared negative keyword lists are not supported"
		nameAsset    = `"searchAssets":{"businessName":"LF"}`
		imageAsset   = `"searchAssets":{"adImages":["https://cdn.example.org/a.png"]}`
		ownList      = `"sharedNegativeKeywordLists":["` + gaSharedSet + `"]`
		foreignList  = `"sharedNegativeKeywordLists":["customers/9999999999/sharedSets/501"]`
	)
	longName := `"searchAssets":{"businessName":"` + strings.Repeat("a", 26) + `"}`
	cases := []struct {
		name, channel, field, want string
	}{
		{"search assets on demand gen", "demand-gen", nameAsset, searchOnly},
		{"search assets on performance max", "performance-max", nameAsset, searchOnly},
		{"search assets on display", "display", imageAsset, searchOnly},
		{"search assets on video", "video", nameAsset, searchOnly},
		{"shared lists on demand gen", "demand-gen", ownList, listsRefused},
		{"shared lists on display", "display", ownList, listsRefused},
		{"shared lists on performance max", "performance-max", ownList, listsRefused},
		{"another account's shared list", "search", foreignList, "belongs to customer \"9999999999\""},
		{"over-long business name", "search", longName, "display width of 26"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"` + tc.channel +
				`",` + tc.field + `}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate")
			}
		})
	}
}

// All three image lists are enumerated, so a stored-asset entry in any of them resolves — and the
// enumeration hands the resolver the config's own slices, so the canonical id is written back.
func TestGoogleAdsCreativeSlots_EnumeratesTheSearchAssets(t *testing.T) {
	braced := "{" + strings.ToUpper(gaSearchImageAssetID) + "}"
	cfg := &googleAdsConfig{SearchAssets: &googleAdsSearchAssetsConfig{
		AdImages:           googleAdsImageRefs{{ImageAssetID: braced}},
		LandscapeAdImages:  googleAdsImageRefs{{ImageAssetID: gaSearchLandscapeAssetID}},
		BusinessLogoImages: googleAdsImageRefs{{ImageAssetID: gaSearchLogoAssetID}},
	}}
	slots := googleAdsCreativeSlots(cfg)
	labels := make([]string, 0, len(slots))
	for _, s := range slots {
		labels = append(labels, s.label)
	}
	wantLabels := []string{"search adImages", "search landscapeAdImages", "search businessLogoImages"}
	if !reflect.DeepEqual(labels, wantLabels) {
		t.Fatalf("slots = %v, want %v", labels, wantLabels)
	}
	if slots[0].refs != &cfg.SearchAssets.AdImages ||
		slots[1].refs != &cfg.SearchAssets.LandscapeAdImages ||
		slots[2].refs != &cfg.SearchAssets.BusinessLogoImages {
		t.Error("the slots do not address the config's own lists")
	}

	d := &GoogleAdsDispatcher{}
	d.SetCreativeAssetRepo(searchAssetLibrary(t))
	resolved, release, err := d.resolveGoogleCreativeAssets(context.Background(), testBrief(), cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer release()
	for _, id := range []string{gaSearchImageAssetID, gaSearchLandscapeAssetID, gaSearchLogoAssetID} {
		if _, ok := resolved[googleAdsImageRefScheme+id]; !ok {
			t.Errorf("stored asset %s was not resolved", id)
		}
	}
	if got := cfg.SearchAssets.AdImages[0].ImageAssetID; got != gaSearchImageAssetID {
		t.Errorf("the ad image id was not canonicalized in place: %q", got)
	}
	want := []string{googleAdsImageRefScheme + gaSearchImageAssetID}
	if got := googleAdsSearchAssets(cfg.SearchAssets).AdImages; !reflect.DeepEqual(got, want) {
		t.Errorf("mapped ad images = %v, want the canonical stored-asset entry", got)
	}
}

func TestGoogleAdsSearchAssets_MapsEachFieldAndNilIsZero(t *testing.T) {
	got := googleAdsSearchAssets(&googleAdsSearchAssetsConfig{
		AdImages:           googleAdsImageRefsFromURLs([]string{"a"}),
		LandscapeAdImages:  googleAdsImageRefsFromURLs([]string{"w"}),
		BusinessLogoImages: googleAdsImageRefsFromURLs([]string{"l"}),
		BusinessName:       "LF",
	})
	want := googleads.SearchCampaignAssets{
		AdImages: []string{"a"}, LandscapeAdImages: []string{"w"},
		BusinessLogoImages: []string{"l"}, BusinessName: "LF",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapped = %+v, want %+v", got, want)
	}
	var zero googleads.SearchCampaignAssets
	if got := googleAdsSearchAssets(nil); !reflect.DeepEqual(got, zero) {
		t.Errorf("nil config produced %+v, want the zero value", got)
	}
}

// All three image lists are sanitized, on a deep copy, and a config carrying ONLY the Search assets
// does not take the early return.
func TestGoogleAdsSnapshotConfig_SanitizesTheSearchAssets(t *testing.T) {
	assets := &googleAdsSearchAssetsConfig{
		AdImages:          googleAdsImageRefsFromURLs([]string{signedCreativeURL}),
		LandscapeAdImages: googleAdsImageRefsFromURLs([]string{"https://cdn.example.org/w.png?sig=wide-secret"}),
		BusinessLogoImages: googleAdsImageRefs{
			{URL: "https://cdn.example.org/l.png?sig=logo-secret"},
			{ImageAssetID: gaSearchLogoAssetID},
		},
		BusinessName: "Linux Foundation",
	}
	snapshot := googleAdsSnapshotConfig(googleAdsConfig{SearchAssets: assets})
	if snapshot.SearchAssets == nil {
		t.Fatal("a config carrying only the Search assets dropped them")
	}
	if snapshot.SearchAssets == assets {
		t.Error("the snapshot points at the caller's Search assets")
	}
	for name, got := range map[string]string{
		"adImages":           snapshot.SearchAssets.AdImages[0].URL,
		"landscapeAdImages":  snapshot.SearchAssets.LandscapeAdImages[0].URL,
		"businessLogoImages": snapshot.SearchAssets.BusinessLogoImages[0].URL,
	} {
		if strings.Contains(got, "secret") || strings.Contains(got, "s3cr3t") {
			t.Errorf("%s: snapshot still carries the signing query: %q", name, got)
		}
		if !strings.HasPrefix(got, "https://cdn.example.org") {
			t.Errorf("%s: snapshot lost the host: %q", name, got)
		}
	}
	if got := snapshot.SearchAssets.BusinessLogoImages[1].ImageAssetID; got != gaSearchLogoAssetID {
		t.Errorf("the stored asset id was changed to %q; it is the snapshot guard's key", got)
	}
	if got := assets.AdImages[0].URL; got != signedCreativeURL {
		t.Errorf("the caller's config was mutated to %q; the signed url must stay fetchable", got)
	}
	if snapshot.SearchAssets.BusinessName != "Linux Foundation" {
		t.Errorf("business name = %q, want it kept", snapshot.SearchAssets.BusinessName)
	}
}

// sharedListsConfig is a create on channel naming one shared negative keyword list, with extra
// merged into the config.
func sharedListsConfig(t *testing.T, channel string, lists []string,
	extra map[string]any) json.RawMessage {
	t.Helper()
	cfg := map[string]any{
		"budget": 50, "channel": channel, "sharedNegativeKeywordLists": lists,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": cfg})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sharedNegativeKeywordLists names existing shared sets, checked against the account only, so it
// sits behind GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED: anything but exactly "true" refuses
// any non-empty list, on any channel, before any request.
func TestGoogleAds_SharedNegativeListsGate(t *testing.T) {
	own := []string{gaSharedSet}
	cases := []struct {
		name, env, channel string
		lists              []string
	}{
		{"unset", unsetEnv, "search", own},
		{"empty", "", "search", own},
		{"false", "false", "search", own},
		{"wrong case", "TRUE", "search", own},
		{"leading space", " true", "search", own},
		{"blank entry", "", "search", []string{"  "}},
		{"off search", "", "demand-gen", own},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsSharedNegativeListsEnabled, tc.env)
			opts, cap := targetingServers(t)
			conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
			cfg := sharedListsConfig(t, tc.channel, tc.lists, nil)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatal("expected the dispatch to be refused while the gate is off")
			}
			for _, want := range []string{
				"sharedNegativeKeywordLists is disabled on this deployment",
				"omit sharedNegativeKeywordLists",
				"; or ask an operator to set " + constants.EnvGoogleAdsSharedNegativeListsEnabled + ` to "true"`,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
			var pce *preCreateError
			if !errors.As(err, &pce) {
				t.Errorf("error = %v, want a not-created error so the claim is released", err)
			}
			got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
			if got != err.Error() {
				t.Errorf("job message = %q, want the gate's own sentence %q", got, err.Error())
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget || len(cap.campaigns) != 0 || len(cap.sharedSets) != 0 {
				t.Error("the refusal must happen BEFORE any request")
			}
		})
	}

	// Exactly "true" lets the same list through to the cascade's attach.
	t.Run("exactly true", func(t *testing.T) {
		setGateEnv(t, constants.EnvGoogleAdsSharedNegativeListsEnabled, "true")
		opts, cap := targetingServers(t)
		conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
		d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
		cfg := sharedListsConfig(t, "search", own, nil)
		if _, err := d.Dispatch(context.Background(), testBrief(),
			model.ProviderGoogleAds, cfg); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		cap.mu.Lock()
		defer cap.mu.Unlock()
		if len(cap.sharedSets) != 1 ||
			!strings.Contains(string(cap.sharedSets[0]), `"sharedSet":"`+gaSharedSet+`"`) {
			t.Errorf("campaignSharedSets bodies = %s, want one naming sharedSets/501",
				cap.sharedSets)
		}
	})
}

// The gate leaves searchAssets alone: with it unset, a business name reaches the cascade.
func TestGoogleAds_SharedNegativeListsGateLeavesSearchAssetsAlone(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsSharedNegativeListsEnabled, unsetEnv)
	opts, cap := targetingServers(t)
	conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
	d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"search",` +
		`"searchAssets":{"businessName":"Linux Foundation"}}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(),
		model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.googleAdsMutates) != 1 ||
		!strings.Contains(string(cap.googleAdsMutates[0]), `"text":"Linux Foundation"`) {
		t.Errorf("googleAds:mutate bodies = %s, want the business name", cap.googleAdsMutates)
	}
}

// The shared set type check (#548) from the outside: a list that is not a negative keyword
// list reaches the job as the client's own sentence, naming the set's id and type, and a read
// Google declines to answer reaches it as "answered HTTP n before anything was created". Both
// are not-created, so the claim is released, and nothing is written.
func TestGoogleAds_SharedListTypeCheckJobMessages(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{name: "negative placements set", status: 200,
			body: `{"results":[{"sharedSet":{"resourceName":"` + gaSharedSet +
				`","type":"NEGATIVE_PLACEMENTS","status":"ENABLED"}}]}`,
			want: []string{`(shared set "501") is a shared set of type NEGATIVE_PLACEMENTS, ` +
				`not NEGATIVE_KEYWORDS`, "omit it"}},
		{name: "set not in the account", status: 200, body: `{"results":[]}`,
			want: []string{`(shared set "501") was not found in account "1234567890"`}},
		{name: "read unavailable", status: 503, body: `{"error":{"code":14}}`,
			want: []string{"answered HTTP 503 before anything was created",
				"nothing was created"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsSharedNegativeListsEnabled, "true")
			opts, cap := targetingServers(t)
			cap.sharedSetReply = func() (int, string) { return tc.status, tc.body }
			conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
			cfg := sharedListsConfig(t, "search", []string{gaSharedSet}, nil)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || camp != nil {
				t.Fatalf("camp = %+v, err = %v; want a not-created refusal", camp, err)
			}
			var pce *preCreateError
			if !errors.As(err, &pce) {
				t.Errorf("error = %v, want a not-created error so the claim is released", err)
			}
			got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("job message = %q, want it to contain %q", got, want)
				}
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget || len(cap.campaigns) != 0 || len(cap.sharedSets) != 0 {
				t.Error("the refusal must happen BEFORE any write")
			}
		})
	}
}
