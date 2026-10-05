// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The dispatch-layer half of the PR #239 review fixes. Each test below pins one
// defect that reached review, in the shape that would have caught it:
//
//   - an omitted deviceBidModifiers.bidModifier silently excluding a device;
//   - the status cascade reaching only the first of several ad groups;
//   - keyword actions refusing every ad group but the first;
//   - a sitelink's finalUrl persisted whole into config_snapshot.

// ---------------------------------------------------------------------------
// Device bid modifiers: absent is not zero
// ---------------------------------------------------------------------------

// A deviceBidModifiers entry that omits bidModifier must be REFUSED, and refused
// before anything is created. A plain float64 decodes the absence to 0, and 0 is not
// "no adjustment" — it is the -100% opt-out, which stops the campaign serving on that
// device with no error anywhere. The refusal is the only way a JSON caller can tell
// the two apart, because the pointer is flattened by the time the client sees it.
func TestGoogleAds_DeviceBidModifier_OmittedIsRefusedBeforeAnyCreate(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"deviceBidModifiers":[{"device":"TABLET"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil {
		t.Fatal("expected a deviceBidModifiers entry with no bidModifier to be refused")
	}
	if camp != nil {
		t.Errorf("nothing may be created for a refused input, got campaign %+v", camp)
	}
	if !strings.Contains(err.Error(), "bidModifier") {
		t.Errorf("the error must name the field the caller has to supply, got: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("the refusal must happen BEFORE the budget mutate — a later one strands a paid campaign")
	}
}

// The refusal runs before the channel is resolved, so it applies on BOTH channels
// rather than only the one whose preflight happens to look at device modifiers.
func TestGoogleAds_DeviceBidModifier_OmittedIsRefusedOnDemandGenToo(t *testing.T) {
	opts, _ := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","deviceBidModifiers":[{"device":"TABLET"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err == nil {
		t.Fatal("expected the same refusal on demand-gen")
	}
}

// An EXPLICIT zero is a legitimate input — the caller asking for the -100% opt-out on
// purpose — and must be carried through, not refused. Refusing it would be the
// over-refusal this guard must not commit.
func TestGoogleAdsDeviceBidModifiers_ExplicitZeroIsCarried(t *testing.T) {
	zero := 0.0
	half := 0.5
	out, err := googleAdsDeviceBidModifiers([]googleAdsDeviceBidModifierConfig{
		{Device: "TABLET", BidModifier: &zero},
		{Device: "MOBILE", BidModifier: &half},
	})
	if err != nil {
		t.Fatalf("an explicit bidModifier must be accepted: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d modifiers, want 2", len(out))
	}
	if out[0].BidModifier != 0 || out[1].BidModifier != 0.5 {
		t.Errorf("modifiers = %+v, want the values the caller supplied", out)
	}
}

// ---------------------------------------------------------------------------
// The status cascade reaches every ad group
// ---------------------------------------------------------------------------

// toggleCapture records the ad group and ad resource names each status mutate named,
// which is what proves the cascade reached a group rather than merely issuing a call.
type toggleCapture struct {
	mu        sync.Mutex
	paths     []string
	adGroups  []string
	adGroupAd []string
}

func (c *toggleCapture) snapshot() ([]string, []string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...), append([]string(nil), c.adGroups...), append([]string(nil), c.adGroupAd...)
}

// toggleServers serves the status-toggle cascade, echoing ONE result per requested
// operation — the client reads a short response as UNCONFIRMED, so a fixed-size fake
// would fail every multi-group test and hide a genuine count mismatch. Handlers never
// call t.Fatal: each runs on its own goroutine, where FailNow is invalid.
func toggleServers(t *testing.T) ([]googleads.Option, *toggleCapture) {
	t.Helper()
	cap := &toggleCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Operations []struct {
				Update struct {
					ResourceName string `json:"resourceName"`
				} `json:"update"`
			} `json:"operations"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode %s request: %v", r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(req.Operations))
		for _, op := range req.Operations {
			names = append(names, op.Update.ResourceName)
		}
		cap.mu.Lock()
		cap.paths = append(cap.paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			cap.adGroupAd = append(cap.adGroupAd, names...)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			cap.adGroups = append(cap.adGroups, names...)
		}
		cap.mu.Unlock()

		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, `{"resourceName":"`+n+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}, cap
}

// multiGroupCampaign is a campaign whose persisted Result blob records TWO ad groups,
// the second holding two ads — the shape a multi ad group + multi RSA create writes.
// The scalar adGroupId/adId pair is present too, exactly as the create path writes it:
// a copy of the first group, kept for readers that predate AdGroups.
func multiGroupCampaign() *model.Campaign {
	return &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result: json.RawMessage(`{
			"customerId":"1234567890",
			"adGroupId":"333","adId":"444",
			"keywordCriteriaIds":["901"],
			"adGroups":[
				{"name":"Training","id":"333","adIds":["444"],"keywordCriteriaIds":["901"]},
				{"name":"Certification","id":"334","adIds":["445","446"],"keywordCriteriaIds":["902"]}
			]
		}`),
	}
}

// PAUSE must reach EVERY ad group and EVERY ad, not just the first pair. Pausing the
// campaign while a second group stays ENABLED is the less visible half of the defect;
// the activate direction is the one that reports a running campaign that mostly is not.
func TestGoogleAds_ToggleStatus_PauseCascadesToEveryAdGroupAndAd(t *testing.T) {
	opts, cap := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, multiGroupCampaign(), model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	paths, groups, ads := cap.snapshot()
	if len(paths) != 3 {
		t.Fatalf("issued %d API calls, want 3 (campaign, one batched adGroups, one batched adGroupAds): %v", len(paths), paths)
	}
	if !strings.HasSuffix(paths[0], "campaigns:mutate") {
		t.Errorf("first call = %q, want campaigns:mutate (parent before children on PAUSE)", paths[0])
	}
	wantGroups := []string{
		"customers/1234567890/adGroups/333",
		"customers/1234567890/adGroups/334",
	}
	if strings.Join(groups, ",") != strings.Join(wantGroups, ",") {
		t.Errorf("ad group updates = %v, want every group in creation order %v", groups, wantGroups)
	}
	wantAds := []string{
		"customers/1234567890/adGroupAds/333~444",
		"customers/1234567890/adGroupAds/334~445",
		"customers/1234567890/adGroupAds/334~446",
	}
	if strings.Join(ads, ",") != strings.Join(wantAds, ",") {
		t.Errorf("ad updates = %v, want every ad under its OWN group %v", ads, wantAds)
	}
}

// ACTIVATE must reach every group too, children before the campaign — so the campaign
// never reports ENABLED while a group it owns is still paused.
func TestGoogleAds_ToggleStatus_ActivateCascadesToEveryAdGroupChildrenFirst(t *testing.T) {
	opts, cap := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, multiGroupCampaign(), model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	paths, groups, ads := cap.snapshot()
	if len(paths) != 3 {
		t.Fatalf("issued %d API calls, want 3: %v", len(paths), paths)
	}
	if !strings.HasSuffix(paths[2], "campaigns:mutate") {
		t.Errorf("last call = %q, want campaigns:mutate (children before parent on ACTIVATE)", paths[2])
	}
	if len(groups) != 2 || len(ads) != 3 {
		t.Errorf("activated %d group(s) and %d ad(s), want 2 and 3: %v / %v", len(groups), len(ads), groups, ads)
	}
}

// The scalar pair is a COPY of the first AdGroups entry, so a reader that merges both
// sources sees group 333 twice. Google rejects two operations against one resource, so
// the duplicate is collapsed rather than sent — and rather than refused, which would
// fail a toggle that is perfectly well specified.
func TestGoogleAds_ToggleStatus_DoesNotRepeatTheFirstAdGroup(t *testing.T) {
	opts, cap := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, multiGroupCampaign(), model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	_, groups, ads := cap.snapshot()
	seen := map[string]int{}
	for _, n := range append(append([]string(nil), groups...), ads...) {
		seen[n]++
		if seen[n] > 1 {
			t.Errorf("resource %q appears %d times in one mutate; Google rejects duplicate operations on the same resource", n, seen[n])
		}
	}
}

// A row written before AdGroups existed carries only the scalar pair, and is
// single-group by construction. It must keep toggling exactly as it always did.
func TestGoogleAds_ToggleStatus_LegacyScalarOnlyRowStillCascades(t *testing.T) {
	opts, cap := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444"}`),
	}
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	_, groups, ads := cap.snapshot()
	if len(groups) != 1 || groups[0] != "customers/1234567890/adGroups/333" {
		t.Errorf("ad group updates = %v, want the one group the legacy blob records", groups)
	}
	if len(ads) != 1 || ads[0] != "customers/1234567890/adGroupAds/333~444" {
		t.Errorf("ad updates = %v, want the one ad the legacy blob records", ads)
	}
}

// A campaign whose SECOND group failed to create is still activatable through the
// first — refusing the whole campaign would strand one that is otherwise ready, which
// is over-refusal. The incomplete group is reported, not silently dropped, but the
// groups that can take the toggle still get it.
func TestGoogleAds_ToggleStatus_ActivatesTheGroupsThatExistWhenOneFailed(t *testing.T) {
	opts, cap := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result: json.RawMessage(`{
			"customerId":"1234567890",
			"adGroupId":"333","adId":"444",
			"adGroups":[
				{"name":"Training","id":"333","adIds":["444"],"keywordCriteriaIds":["901"]},
				{"name":"Certification","id":""}
			]
		}`),
	}
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	_, groups, ads := cap.snapshot()
	if len(groups) != 1 || len(ads) != 1 {
		t.Errorf("toggled %v / %v, want only the one group that was fully created", groups, ads)
	}
}

// Activation is gated on keyword targeting existing SOMEWHERE in the campaign, not on
// the first group having it. A campaign whose second group carries the keywords can
// deliver, so refusing it is over-refusal — the exact failure the campaign-wide read
// of the blob prevents.
func TestGoogleAds_ToggleStatus_ActivateAllowedWhenOnlyALaterGroupHasKeywords(t *testing.T) {
	opts, _ := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result: json.RawMessage(`{
			"customerId":"1234567890",
			"adGroupId":"333","adId":"444",
			"adGroups":[
				{"name":"Training","id":"333","adIds":["444"]},
				{"name":"Certification","id":"334","adIds":["445"],"keywordCriteriaIds":["902"]}
			]
		}`),
	}
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus must not refuse a campaign whose later group carries the keywords: %v", err)
	}
}

// And the gate still holds when NO group has keywords: a campaign with no keyword
// criterion anywhere cannot deliver, so activating it would report false success.
func TestGoogleAds_ToggleStatus_ActivateStillRefusedWhenNoGroupHasKeywords(t *testing.T) {
	opts, _ := toggleServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result: json.RawMessage(`{
			"customerId":"1234567890",
			"adGroupId":"333","adId":"444",
			"adGroups":[
				{"name":"Training","id":"333","adIds":["444"]},
				{"name":"Certification","id":"334","adIds":["445"]}
			]
		}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("expected ErrCampaignNotProvisioned, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Keyword actions reach every ad group
// ---------------------------------------------------------------------------

// An action naming the SECOND ad group of a multi-group campaign must be accepted. It
// was refused before, which was fail-closed and therefore never unsafe, but it made
// keyword actions unusable on exactly the campaigns multi-group support created.
func TestGoogleAds_ApplyKeywordActions_AcceptsALaterAdGroup(t *testing.T) {
	var sawMutate atomic.Bool
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The client resolves each criterion's TYPE before mutating. `negative` is omitted,
		// which is how Google sends a positive keyword — protobuf JSON drops default values.
		if strings.Contains(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"902"},"adGroup":{"id":"334"}}]}`)
			return
		}
		sawMutate.Store(true)
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/334~902"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	if _, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderGoogleAds, multiGroupCampaign(), pauseAction("334", "902")); err != nil {
		t.Fatalf("an action on the campaign's second ad group must be accepted: %v", err)
	}
	if !sawMutate.Load() {
		t.Error("the action was accepted but never reached Google")
	}
}

// The bound itself is unchanged in strength: an ad group this campaign does not record
// is still refused locally, before Google is contacted. Without it a caller holding a
// criterion id from any campaign in the shared account could remove it through a
// campaign they do own.
func TestGoogleAds_ApplyKeywordActions_StillRefusesAForeignAdGroup(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	actions := []model.KeywordAction{{AdGroupID: "999", CriterionID: "902", Action: model.KeywordActionRemove}}
	_, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderGoogleAds, multiGroupCampaign(), actions)
	if !errors.Is(err, domain.ErrKeywordActionInvalid) {
		t.Fatalf("expected ErrKeywordActionInvalid for an ad group this campaign does not record, got %T: %v", err, err)
	}
	if reached.Load() {
		t.Error("the refusal must happen locally — REMOVE is irreversible, so nothing may be sent")
	}
}

// A group that exists with keyword criteria but whose ADS never got created is still
// this campaign's group: its keywords are real and serving nothing, and pausing or
// removing them is a legitimate thing to ask for. The membership test must not borrow
// the status toggle's narrower "can this group take a toggle" list.
func TestGoogleAds_ApplyKeywordActions_AcceptsAGroupWithNoAds(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"902"},"adGroup":{"id":"334"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/334~902"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result: json.RawMessage(`{
			"customerId":"1234567890",
			"adGroupId":"333","adId":"444",
			"adGroups":[
				{"name":"Training","id":"333","adIds":["444"]},
				{"name":"Certification","id":"334","keywordCriteriaIds":["902"]}
			]
		}`),
	}
	if _, err := d.ApplyKeywordActions(context.Background(), "proj", model.ProviderGoogleAds, camp, pauseAction("334", "902")); err != nil {
		t.Fatalf("a group with criteria but no ads is still this campaign's: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Sitelink URLs in the persisted snapshot
// ---------------------------------------------------------------------------

// config_snapshot is persisted UNENCRYPTED, and a caller-supplied URL can carry a
// credential in its path, query or fragment. The snapshot keeps scheme+host only —
// while the FULL url still reaches Google, which is what the deep copy is for. Both
// halves are asserted here: redacting the one Google receives would be a worse bug
// than the one being fixed.
func TestGoogleAds_Dispatch_SnapshotRedactsSitelinkURLButSendsItWhole(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	const full = "https://events.example/kc/register?token=s3cr3t-registration-key"
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"sitelinks":[{"text":"Register","finalUrl":"` + full + `"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("Dispatch returned no campaign")
	}

	snapshot := string(camp.ConfigSnapshot)
	if strings.Contains(snapshot, "s3cr3t-registration-key") {
		t.Errorf("the persisted snapshot still carries the sitelink URL's query: %s", snapshot)
	}
	if !strings.Contains(snapshot, "https://events.example") {
		t.Errorf("the snapshot must keep scheme+host so the row is still readable: %s", snapshot)
	}

	cap.mu.Lock()
	sentAssets := string(cap.assets)
	cap.mu.Unlock()
	if !strings.Contains(sentAssets, "s3cr3t-registration-key") {
		t.Errorf("the FULL url must still reach Google — redacting the outbound one would break the sitelink: %s", sentAssets)
	}
}

// The deep copy is the point: cfg is passed by value but its Sitelinks slice shares a
// backing array with the caller's, so sanitizing in place would redact the URL the
// create path is about to send. Asserted directly on the helper, because an aliasing
// bug of this kind is invisible whenever the snapshot happens to be built last.
func TestGoogleAdsSnapshotConfig_DoesNotMutateTheCallersSitelinks(t *testing.T) {
	const full = "https://events.example/kc/register?token=s3cr3t"
	cfg := googleAdsConfig{Sitelinks: []googleAdsSitelinkConfig{{Text: "Register", FinalURL: full}}}

	snapshot := googleAdsSnapshotConfig(cfg)
	if cfg.Sitelinks[0].FinalURL != full {
		t.Errorf("the caller's sitelink was mutated to %q; the full url must still reach Google", cfg.Sitelinks[0].FinalURL)
	}
	if strings.Contains(snapshot.Sitelinks[0].FinalURL, "s3cr3t") {
		t.Errorf("the snapshot sitelink was not sanitized: %q", snapshot.Sitelinks[0].FinalURL)
	}
}
