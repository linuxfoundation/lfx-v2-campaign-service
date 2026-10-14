// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

func f64(v float64) *float64 { return &v }

// TestResolveBidding is the platform-independent rule table (LFXV2-2665).
func TestResolveBidding(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *biddingConfig
		want    bidPlan
		wantErr string
	}{
		{name: "omitted is automatic", cfg: nil, want: bidPlan{strategy: model.BiddingAutomatic}},
		{name: "empty strategy is automatic", cfg: &biddingConfig{}, want: bidPlan{strategy: model.BiddingAutomatic}},
		{name: "explicit automatic", cfg: &biddingConfig{Strategy: "automatic"}, want: bidPlan{strategy: model.BiddingAutomatic}},
		{name: "manual", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(1.25)}, want: bidPlan{strategy: model.BiddingManual, amount: 1.25}},
		{name: "manual at six decimals", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(0.000001)}, want: bidPlan{strategy: model.BiddingManual, amount: 0.000001}},
		{name: "manual at the ceiling", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(1_000_000)}, want: bidPlan{strategy: model.BiddingManual, amount: 1_000_000}},
		{name: "automatic with an amount", cfg: &biddingConfig{Strategy: "automatic", BidAmount: f64(1)}, wantErr: "only accepted with strategy \"manual\""},
		{name: "automatic with a zero amount", cfg: &biddingConfig{BidAmount: f64(0)}, wantErr: "only accepted with strategy \"manual\""},
		{name: "manual without an amount", cfg: &biddingConfig{Strategy: "manual"}, wantErr: "requires bidAmount"},
		{name: "manual zero", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(0)}, wantErr: "greater than zero"},
		{name: "manual negative", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(-1)}, wantErr: "greater than zero"},
		{name: "manual NaN", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(math.NaN())}, wantErr: "finite"},
		{name: "manual Inf", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(math.Inf(1))}, wantErr: "finite"},
		{name: "manual over the ceiling", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(1_000_000.01)}, wantErr: "exceeds the maximum"},
		{name: "manual sub-micro", cfg: &biddingConfig{Strategy: "manual", BidAmount: f64(1.0000001)}, wantErr: "not a whole number of micro-units"},
		{name: "unknown strategy", cfg: &biddingConfig{Strategy: "target"}, wantErr: "must be \"automatic\" or \"manual\""},
		{name: "case matters", cfg: &biddingConfig{Strategy: "Manual", BidAmount: f64(1)}, wantErr: "must be \"automatic\" or \"manual\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBidding("metaConfig", tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want ErrCreateConfigInvalid containing %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "metaConfig.bidding") {
					t.Errorf("err = %v, must name the field metaConfig.bidding", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolveBidding = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestExactScaled(t *testing.T) {
	// Runtime addition, not a constant expression: Go folds 0.1+0.2 exactly to 0.3 at compile
	// time, while the float64 sum is 0.30000000000000004 — the value a JSON decode cannot produce
	// but a computed amount can.
	tenth, fifth := 0.1, 0.2
	cases := []struct {
		amount float64
		scale  int64
		want   int64
		ok     bool
	}{
		{0.29, 100, 29, true}, {1.005, 100, 0, false}, {2.34, 100, 234, true}, {1.5, 1, 0, false},
		{3, 1, 3, true}, {0.000001, 1_000_000, 1, true}, {0.0000015, 1_000_000, 0, false},
		{tenth + fifth, 100, 0, false}, {1_000_000, 1_000_000, 1_000_000_000_000, true},
	}
	for _, tc := range cases {
		got, ok := exactScaled(tc.amount, tc.scale)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("exactScaled(%v, %d) = %d, %v; want %d, %v", tc.amount, tc.scale, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBidPlanStamp(t *testing.T) {
	var nilCamp *model.Campaign
	bidPlan{strategy: model.BiddingManual, amount: 1}.stamp(nilCamp) // must not panic

	manual := &model.Campaign{}
	bidPlan{strategy: model.BiddingManual, amount: 2.5}.stamp(manual)
	if manual.BiddingStrategy == nil || *manual.BiddingStrategy != model.BiddingManual || manual.MaxCPCBid == nil || *manual.MaxCPCBid != 2.5 {
		t.Fatalf("manual stamp = %v / %v, want manual / 2.5", manual.BiddingStrategy, manual.MaxCPCBid)
	}
	auto := &model.Campaign{}
	bidPlan{strategy: model.BiddingAutomatic}.stamp(auto)
	if auto.BiddingStrategy == nil || *auto.BiddingStrategy != model.BiddingAutomatic || auto.MaxCPCBid != nil {
		t.Fatalf("automatic stamp = %v / %v, want automatic / nil", auto.BiddingStrategy, auto.MaxCPCBid)
	}
	unset := &model.Campaign{}
	bidPlan{}.stamp(unset)
	if unset.BiddingStrategy != nil {
		t.Fatal("a zero plan must not record a strategy")
	}
}

// biddingPreflightCase is one PreflightCreate expectation: refused (a 400 naming want) or not.
type biddingPreflightCase struct {
	name, config, want string
}

func runBiddingPreflight(t *testing.T, pre service.CreatePreflighter, platform model.Provider, cases []biddingPreflightCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pre.PreflightCreate(context.Background(), "cncf", platform, json.RawMessage(tc.config))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want ErrPreflightNotApplicable", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PreflightCreate = %v, want ErrCreateConfigInvalid containing %q", err, tc.want)
			}
		})
	}
}

// Every lane dispatcher must be a CreatePreflighter, or a bad bidding config would be refused only
// after the 202, as an opaque job failure.
var (
	_ service.CreatePreflighter = (*MicrosoftDispatcher)(nil)
	_ service.CreatePreflighter = (*MetaDispatcher)(nil)
	_ service.CreatePreflighter = (*RedditDispatcher)(nil)
	_ service.CreatePreflighter = (*TwitterDispatcher)(nil)
	_ service.CreatePreflighter = (*LinkedInDispatcher)(nil)
)

func TestMicrosoft_PreflightCreate_Bidding(t *testing.T) {
	runBiddingPreflight(t, &MicrosoftDispatcher{}, model.ProviderMicrosoftAds, []biddingPreflightCase{
		{name: "no config", config: ``},
		{name: "no bidding", config: `{"microsoftConfig":{"budget":50}}`},
		{name: "manual", config: `{"microsoftConfig":{"bidding":{"strategy":"manual","bidAmount":1.5}}}`},
		{name: "legacy cpcBid", config: `{"microsoftConfig":{"cpcBid":1.5}}`},
		{name: "manual with the same cpcBid", config: `{"microsoftConfig":{"cpcBid":1.5,"bidding":{"strategy":"manual","bidAmount":1.5}}}`},
		{name: "undecodable is left to dispatch", config: `{"microsoftConfig":{"bidding":"manual"}}`},
		{name: "automatic with cpcBid", config: `{"microsoftConfig":{"cpcBid":1.5,"bidding":{"strategy":"automatic"}}}`, want: "automatic (MaxClicks) strategy ignores"},
		{name: "manual with a different cpcBid", config: `{"microsoftConfig":{"cpcBid":2,"bidding":{"strategy":"manual","bidAmount":1.5}}}`, want: "two different starting bids"},
		{name: "manual below Microsoft's floor", config: `{"microsoftConfig":{"bidding":{"strategy":"manual","bidAmount":0.005}}}`, want: "below the minimum"},
		{name: "manual above Microsoft's ceiling", config: `{"microsoftConfig":{"bidding":{"strategy":"manual","bidAmount":1000.5}}}`, want: "exceeds the maximum"},
		{name: "legacy cpcBid below the floor", config: `{"microsoftConfig":{"cpcBid":0.001}}`, want: "cpcBid: "},
		{name: "manual without amount", config: `{"microsoftConfig":{"bidding":{"strategy":"manual"}}}`, want: "requires bidAmount"},
	})
}

func TestMeta_PreflightCreate_Bidding(t *testing.T) {
	runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
		{name: "no bidding", config: `{"metaConfig":{"budget":50}}`},
		{name: "manual on traffic", config: `{"metaConfig":{"objective":"traffic","bidding":{"strategy":"manual","bidAmount":1.25}}}`},
		{name: "manual on the default objective", config: `{"metaConfig":{"bidding":{"strategy":"manual","bidAmount":1}}}`},
		{name: "manual on leads", config: `{"metaConfig":{"objective":"leads","bidding":{"strategy":"manual","bidAmount":1}}}`},
		{name: "automatic on awareness", config: `{"metaConfig":{"objective":"awareness","bidding":{"strategy":"automatic"}}}`},
		{name: "manual on awareness", config: `{"metaConfig":{"objective":"awareness","bidding":{"strategy":"manual","bidAmount":1}}}`, want: "traffic or leads objective"},
		{name: "manual on conversions", config: `{"metaConfig":{"objective":"conversions","bidding":{"strategy":"manual","bidAmount":1}}}`, want: "traffic or leads objective"},
		{name: "manual with three decimals", config: `{"metaConfig":{"bidding":{"strategy":"manual","bidAmount":1.255}}}`, want: "more than two decimal places"},
		{name: "automatic with an amount", config: `{"metaConfig":{"bidding":{"bidAmount":1}}}`, want: "only accepted with strategy"},
	})
}

func TestReddit_PreflightCreate_Bidding(t *testing.T) {
	runBiddingPreflight(t, &RedditDispatcher{}, model.ProviderRedditAds, []biddingPreflightCase{
		{name: "no bidding", config: `{"redditConfig":{"budgetUsd":50}}`},
		{name: "manual on traffic", config: `{"redditConfig":{"objective":"traffic","bidding":{"strategy":"manual","bidAmount":0.75}}}`},
		{name: "automatic on conversions", config: `{"redditConfig":{"objective":"conversions","bidding":{"strategy":"automatic"}}}`},
		{name: "manual on the default objective", config: `{"redditConfig":{"bidding":{"strategy":"manual","bidAmount":1}}}`, want: "needs objective \"traffic\""},
		{name: "manual on awareness", config: `{"redditConfig":{"objective":"awareness","bidding":{"strategy":"manual","bidAmount":1}}}`, want: "needs objective \"traffic\""},
		{name: "manual sub-micro", config: `{"redditConfig":{"objective":"traffic","bidding":{"strategy":"manual","bidAmount":0.0000001}}}`, want: "micro-units"},
	})
}

func TestTwitter_PreflightCreate_Bidding(t *testing.T) {
	runBiddingPreflight(t, &TwitterDispatcher{}, model.ProviderTwitterAds, []biddingPreflightCase{
		{name: "no bidding", config: `{"twitterConfig":{"budgetAmount":50}}`},
		{name: "manual", config: `{"twitterConfig":{"bidding":{"strategy":"manual","bidAmount":2.5}}}`},
		{name: "manual without amount", config: `{"twitterConfig":{"bidding":{"strategy":"manual"}}}`, want: "twitterConfig.bidding: strategy \"manual\" requires bidAmount"},
		{name: "unknown strategy", config: `{"twitterConfig":{"bidding":{"strategy":"max"}}}`, want: "must be \"automatic\" or \"manual\""},
	})
}

func TestLinkedIn_PreflightCreate_Bidding(t *testing.T) {
	runBiddingPreflight(t, &LinkedInDispatcher{}, model.ProviderLinkedInAds, []biddingPreflightCase{
		{name: "manual whole cents", config: `{"linkedInConfig":{"bidding":{"strategy":"manual","bidAmount":2.34}}}`},
		{name: "undecodable is left to dispatch", config: `{"linkedInConfig":{"bidding":"manual"}}`},
		// Automatic is refused, explicitly or by omission: the Website conversions objective's only
		// auto-bidding target, MAX_CONVERSION, cannot be activated without a conversion association.
		{name: "no bidding is the automatic default", config: `{"linkedInConfig":{"budgetUsd":50}}`, want: "automatic bidding is not available on LinkedIn"},
		{name: "no linkedInConfig at all", config: ``, want: "automatic bidding is not available on LinkedIn"},
		{name: "explicit automatic", config: `{"linkedInConfig":{"bidding":{"strategy":"automatic"}}}`, want: "Website conversions objective"},
		{name: "manual sub-cent", config: `{"linkedInConfig":{"bidding":{"strategy":"manual","bidAmount":2.345}}}`, want: "not a whole number of cents"},
		{name: "automatic with an amount", config: `{"linkedInConfig":{"bidding":{"strategy":"automatic","bidAmount":2}}}`, want: "only accepted with strategy"},
	})
}

// msBiddingResult is what msBiddingCreate observed: the dispatched row and error, the campaign and
// ad group create bodies, and the number of upstream requests.
type msBiddingResult struct {
	camp                      *model.Campaign
	err                       error
	campaignBody, adGroupBody string
	requests                  int
}

// msBiddingCreate runs a Microsoft Dispatch against a fake API that captures the campaign and ad
// group create bodies.
func msBiddingCreate(t *testing.T, config string) msBiddingResult {
	t.Helper()
	var mu sync.Mutex
	var campaignBody, adGroupBody string
	requests := 0
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"at","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		requests++
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			campaignBody = string(body)
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			adGroupBody = string(body)
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(config))
	mu.Lock()
	defer mu.Unlock()
	return msBiddingResult{camp: camp, err: err, campaignBody: campaignBody, adGroupBody: adGroupBody, requests: requests}
}

// TestMicrosoft_ManualCreatedCampaignIsAcceptedByTheBidLever is the end-to-end promise of
// LFXV2-2665 on Microsoft: a campaign dispatched with manual bidding records manual + its starting
// bid, sends EnhancedCpc with the ad group CpcBid, and the bid lever — reading back the strategy
// the create sent — re-bids that very row's ad group.
func TestMicrosoft_ManualCreatedCampaignIsAcceptedByTheBidLever(t *testing.T) {
	r := msBiddingCreate(t, `{"microsoftConfig":{"budget":50,"bidding":{"strategy":"manual","bidAmount":1.5}}}`)
	camp, err, campaignBody, adGroupBody := r.camp, r.err, r.campaignBody, r.adGroupBody
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(campaignBody, `"BiddingScheme":{"Type":"EnhancedCpc"}`) {
		t.Fatalf("campaign body %s does not carry the EnhancedCpc scheme", campaignBody)
	}
	if !strings.Contains(adGroupBody, `"CpcBid":{"Amount":1.5}`) {
		t.Fatalf("ad group body %s does not carry CpcBid 1.5", adGroupBody)
	}
	if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingManual || camp.MaxCPCBid == nil || *camp.MaxCPCBid != 1.5 {
		t.Fatalf("row = %v / %v, want manual / 1.5", camp.BiddingStrategy, camp.MaxCPCBid)
	}

	lever, calls := msBudgetDispatcher(t, msBidStrategy("EnhancedCpc"), http.StatusOK, `{"PartialErrors":[]}`)
	if err := writeMSBid(lever, camp, 2.25); err != nil {
		t.Fatalf("the bid lever refused a manual-created campaign: %v", err)
	}
	writes := msWrites(calls())
	if len(writes) != 1 || !strings.Contains(writes[0].Body, `"Id":654,"CpcBid":{"Amount":2.25}`) {
		t.Fatalf("lever writes = %+v, want one PUT re-bidding ad group 654", writes)
	}
}

// TestMicrosoft_AutomaticCreatedCampaignIsRefusedByTheBidLever is the other side: the default
// sends MaxClicks with no ad group bid, records automatic and no bid, and the lever refuses it.
func TestMicrosoft_AutomaticCreatedCampaignIsRefusedByTheBidLever(t *testing.T) {
	r := msBiddingCreate(t, `{"microsoftConfig":{"budget":50}}`)
	camp, err, campaignBody, adGroupBody := r.camp, r.err, r.campaignBody, r.adGroupBody
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(campaignBody, `"BiddingScheme":{"Type":"MaxClicks"}`) || strings.Contains(adGroupBody, "CpcBid") {
		t.Fatalf("automatic create: campaign %s / ad group %s, want MaxClicks and no CpcBid", campaignBody, adGroupBody)
	}
	if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingAutomatic || camp.MaxCPCBid != nil {
		t.Fatalf("row = %v / %v, want automatic / nil", camp.BiddingStrategy, camp.MaxCPCBid)
	}
	lever, calls := msBudgetDispatcher(t, msBidStrategy("MaxClicks"), http.StatusOK, `{"PartialErrors":[]}`)
	if err := writeMSBid(lever, camp, 2.25); !errors.Is(err, domain.ErrBidUnwritable) {
		t.Fatalf("WriteBid = %v, want ErrBidUnwritable", err)
	}
	if w := msWrites(calls()); len(w) != 0 {
		t.Fatalf("the lever wrote %d times to an automatic campaign", len(w))
	}
}

// TestMicrosoft_LegacyCpcBidIsRecordedManual: a cpcBid without `bidding` keeps its pre-LFXV2-2665
// meaning (an EnhancedCpc campaign bidding cpcBid) and is recorded as manual.
func TestMicrosoft_LegacyCpcBidIsRecordedManual(t *testing.T) {
	r := msBiddingCreate(t, `{"microsoftConfig":{"budget":50,"cpcBid":0.8}}`)
	camp, err, campaignBody, adGroupBody := r.camp, r.err, r.campaignBody, r.adGroupBody
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(campaignBody, `"Type":"EnhancedCpc"`) || !strings.Contains(adGroupBody, `"CpcBid":{"Amount":0.8}`) {
		t.Fatalf("legacy cpcBid: campaign %s / ad group %s", campaignBody, adGroupBody)
	}
	if camp.BiddingStrategy == nil || *camp.BiddingStrategy != model.BiddingManual || camp.MaxCPCBid == nil || *camp.MaxCPCBid != 0.8 {
		t.Fatalf("row = %v / %v, want manual / 0.8", camp.BiddingStrategy, camp.MaxCPCBid)
	}
}

// TestMicrosoft_InvalidBiddingFailsBeforeAnyUpstreamCall: Dispatch re-applies the preflight's
// rules for any path that skipped it, before a single request — and releases the claim.
func TestMicrosoft_InvalidBiddingFailsBeforeAnyUpstreamCall(t *testing.T) {
	r := msBiddingCreate(t, `{"microsoftConfig":{"budget":50,"bidding":{"strategy":"automatic","bidAmount":1}}}`)
	camp, err, requests := r.camp, r.err, r.requests
	if err == nil || camp != nil {
		t.Fatalf("Dispatch = %+v, %v; want a pre-create refusal", camp, err)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("err = %v, want NoUpstreamCreate (claim released)", err)
	}
	if requests != 0 {
		t.Fatalf("%d upstream requests for a refused config, want 0", requests)
	}
}
