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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is reached by TYPE ASSERTION, so a drifted signature would silently turn every
// bid write back into "unsupported". These make that a compile error.
var (
	_ service.BidWriter = (*MicrosoftDispatcher)(nil)
	_ service.BidWriter = (*RedditDispatcher)(nil)
)

// TestBidWriter_OnlyMicrosoftAndRedditImplementIt pins the other half: Google Ads, LinkedIn, Meta,
// X and HubSpot are NOT BidWriters today, so the orchestrator answers ErrBidUnsupported (400) for
// them. A platform gaining the capability is a deliberate decision about its bid model, and this
// test is where that decision is made visible.
func TestBidWriter_OnlyMicrosoftAndRedditImplementIt(t *testing.T) {
	for name, d := range map[string]any{
		"google-ads":   (*GoogleAdsDispatcher)(nil),
		"linkedin-ads": (*LinkedInDispatcher)(nil),
		"meta-ads":     (*MetaDispatcher)(nil),
		"twitter-ads":  (*TwitterDispatcher)(nil),
		"hubspot":      (*HubSpotDispatcher)(nil),
	} {
		if _, ok := d.(service.BidWriter); ok {
			t.Errorf("%s implements BidWriter; update the docs, the design description and this test together", name)
		}
	}
}

func msBidCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321",
		Result: json.RawMessage(`{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987"}`),
	}
}

func msBidStrategy(schemeType string) string {
	return `{"Campaigns":[{"Id":321,"BudgetType":"DailyBudgetStandard","BiddingScheme":{"Type":"` + schemeType + `"}}],"PartialErrors":[]}`
}

func writeMSBid(d *MicrosoftDispatcher, c *model.Campaign, amount float64) error {
	return d.WriteBid(context.Background(), "proj", model.ProviderMicrosoftAds, c, model.BidChange{Amount: amount, Type: model.BidTypeCPC})
}

func TestMicrosoft_WriteBid_EnhancedCpcWritesTheAdGroupCpcBidOnly(t *testing.T) {
	for _, scheme := range []string{"EnhancedCpc", "EnhancedCpcBiddingScheme", "ManualCpc", "ManualCpcBiddingScheme"} {
		t.Run(scheme, func(t *testing.T) {
			d, calls := msBudgetDispatcher(t, msBidStrategy(scheme), http.StatusOK, `{"PartialErrors":[]}`)
			if err := writeMSBid(d, msBidCampaign(), 2.35); err != nil {
				t.Fatalf("WriteBid: %v", err)
			}
			reqs := calls()
			if len(reqs) != 2 || !strings.HasSuffix(reqs[0].Path, "/Campaigns/QueryByIds") {
				t.Fatalf("want the QueryByIds read then one write, got %+v", reqs)
			}
			writes := msWrites(reqs)
			if len(writes) != 1 || !strings.HasSuffix(writes[0].Path, "/CampaignManagement/v13/AdGroups") {
				t.Fatalf("want exactly one PUT .../AdGroups, got %+v", writes)
			}
			// Id and CpcBid ONLY on the ad group: no BiddingScheme (never a strategy change), no
			// Status, no Name — an UpdateAdGroups field left unset is documented as unchanged.
			want := `{"CampaignId":321,"AdGroups":[{"Id":654,"CpcBid":{"Amount":2.35}}],"UpdateAudienceAdsBidAdjustment":false,"ReturnInheritedBidStrategyTypes":false}`
			if writes[0].Body != want {
				t.Errorf("PUT body = %s\nwant       %s", writes[0].Body, want)
			}
		})
	}
}

// TestMicrosoft_WriteBid_AutomatedStrategiesRefusedWithZeroWrites is the strategy table: every
// strategy under which Microsoft ignores the ad group bid is refused, and the platform is not
// written.
func TestMicrosoft_WriteBid_AutomatedStrategiesRefusedWithZeroWrites(t *testing.T) {
	cases := []struct{ name, readJSON string }{
		{"MaxClicks", msBidStrategy("MaxClicks")},
		{"MaxConversions", msBidStrategy("MaxConversionsBiddingScheme")},
		{"TargetCpa", msBidStrategy("TargetCpa")},
		{"TargetRoas", msBidStrategy("TargetRoas")},
		{"MaxConversionValue", msBidStrategy("MaxConversionValue")},
		{"TargetImpressionShare", msBidStrategy("TargetImpressionShare")},
		{"CostPerSale", msBidStrategy("CostPerSale")},
		// Microsoft omits the scheme for MaxConversionValue / TargetImpressionShare by default,
		// so absence is refused, never read as manual.
		{"scheme not reported", `{"Campaigns":[{"Id":321}],"PartialErrors":[]}`},
		{"scheme null", `{"Campaigns":[{"Id":321,"BiddingScheme":null}],"PartialErrors":[]}`},
		{"scheme without a type", `{"Campaigns":[{"Id":321,"BiddingScheme":{}}],"PartialErrors":[]}`},
		{"scheme not an object", `{"Campaigns":[{"Id":321,"BiddingScheme":"EnhancedCpc"}],"PartialErrors":[]}`},
		{"portfolio strategy, even EnhancedCpc", `{"Campaigns":[{"Id":321,"BidStrategyId":"8888","BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`},
		{"unreadable portfolio id", `{"Campaigns":[{"Id":321,"BidStrategyId":-4,"BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := msBudgetDispatcher(t, tc.readJSON, http.StatusOK, `{"PartialErrors":[]}`)
			err := writeMSBid(d, msBidCampaign(), 2)
			if !errors.Is(err, domain.ErrBidUnwritable) {
				t.Fatalf("want ErrBidUnwritable, got %T: %v", err, err)
			}
			assertNoMicrosoftWrite(t, calls())
		})
	}
}

// A portfolio id of 0 is Microsoft's documented "own strategy" value and must not be refused.
func TestMicrosoft_WriteBid_ZeroPortfolioIDIsTheCampaignsOwnStrategy(t *testing.T) {
	d, calls := msBudgetDispatcher(t, `{"Campaigns":[{"Id":321,"BidStrategyId":0,"BiddingScheme":{"Type":"EnhancedCpc"}}],"PartialErrors":[]}`, http.StatusOK, `{"PartialErrors":[]}`)
	if err := writeMSBid(d, msBidCampaign(), 1); err != nil {
		t.Fatalf("WriteBid: %v", err)
	}
	if n := len(msWrites(calls())); n != 1 {
		t.Errorf("want one write, got %d", n)
	}
}

// Refusals that are facts about the row or the request reach no endpoint at all.
func TestMicrosoft_WriteBid_RowAndRequestRefusalsBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Campaign)
		amount float64
		want   error
	}{
		{"no recorded ad group", func(c *model.Campaign) {
			c.Result = json.RawMessage(`{"accountId":"1234567","campaignId":"321"}`)
		}, 2, domain.ErrBidUnwritable},
		{"malformed ad group id", func(c *model.Campaign) {
			c.Result = json.RawMessage(`{"accountId":"1234567","adGroupId":"abc"}`)
		}, 2, domain.ErrBidUnwritable},
		{"malformed campaign id", func(c *model.Campaign) { c.PlatformCampaignID = "x1" }, 2, domain.ErrBidUnwritable},
		{"below the create path's floor", func(*model.Campaign) {}, 0.001, domain.ErrBidAmountRejected},
		{"above the create path's ceiling", func(*model.Campaign) {}, 1000.01, domain.ErrBidAmountRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := msBudgetDispatcher(t, msBidStrategy("EnhancedCpc"), http.StatusOK, `{"PartialErrors":[]}`)
			c := msBidCampaign()
			tc.mutate(c)
			err := writeMSBid(d, c, tc.amount)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %T: %v", tc.want, err, err)
			}
			if tc.want == domain.ErrBidAmountRejected {
				var r interface{ BidAmountReason() string }
				if !errors.As(err, &r) || !strings.Contains(r.BidAmountReason(), "Microsoft Advertising max CPC bid") {
					t.Errorf("an amount refusal must carry a client-safe reason, got %v", err)
				}
			}
			if reqs := calls(); len(reqs) != 0 {
				t.Errorf("refusal must reach no endpoint, saw %+v", reqs)
			}
		})
	}
}

// Provenance: an unrecorded account and a foreign account are both refused before any token or
// API request.
func TestMicrosoft_WriteBid_ProvenanceRefusedBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name, result string
		want         []error
	}{
		{"foreign account", `{"accountId":"7654321","campaignId":"321","adGroupId":"654"}`, []error{domain.ErrCampaignAccountMismatch}},
		{"no provenance recorded", `{"campaignId":"321","adGroupId":"654"}`, []error{domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				reached []string
			)
			hit := func(label string) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					reached = append(reached, label)
					mu.Unlock()
					_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
				}
			}
			tokenSrv := httptest.NewServer(hit("token"))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(hit("api"))
			defer apiSrv.Close()
			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
				microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321", Result: json.RawMessage(tc.result)}
			err := writeMSBid(d, camp, 2)
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Errorf("want %v in the chain, got %T: %v", want, err, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reached) != 0 {
				t.Errorf("provenance refusal must precede every request, saw %v", reached)
			}
		})
	}
}

func TestMicrosoft_WriteBid_CampaignAbsentIs404Sentinel(t *testing.T) {
	d, calls := msBudgetDispatcher(t, `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`, http.StatusOK, `{"PartialErrors":[]}`)
	if err := writeMSBid(d, msBidCampaign(), 2); !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("want ErrPlatformCampaignAbsent, got %v", err)
	}
	assertNoMicrosoftWrite(t, calls())
}

// TestMicrosoft_WriteBid_WriteOutcomes pins the classification of the PUT, exactly as the budget
// write's table does: ambiguity is UNCONFIRMED, a definite refusal keeps its identity, and the
// amount refusals carry a client-safe reason.
func TestMicrosoft_WriteBid_WriteOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
		want        error
		write       http.HandlerFunc
		opts        []microsoft.Option
	}{
		{name: "5xx is unconfirmed", status: http.StatusBadGateway, unconfirmed: true},
		{name: "3xx is unconfirmed", status: http.StatusTemporaryRedirect, unconfirmed: true},
		{name: "transport timeout is unconfirmed", unconfirmed: true,
			write: func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			},
			opts: []microsoft.Option{microsoft.WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond})}},
		{name: "unanswered 200 is unconfirmed", status: http.StatusOK, body: `{}`, unconfirmed: true},
		{name: "definite 4xx", status: http.StatusBadRequest, body: `{"Errors":[{"Code":1001,"ErrorCode":"SomethingElse"}]}`},
		{name: "PartialError on the single op", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1234,"ErrorCode":"Other","Index":0}]}`},
		{name: "below floor", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1515,"ErrorCode":"CampaignServiceBidAmountsLessThanFloorPrice","Index":0}]}`, want: domain.ErrBidAmountRejected},
		{name: "above ceiling", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1516,"ErrorCode":"CampaignServiceBidAmountsGreaterThanCeilingPrice","Index":0}]}`, want: domain.ErrBidAmountRejected},
		{name: "invalid bid as a 400 fault", status: http.StatusBadRequest, body: `{"Errors":[{"Code":1538,"ErrorCode":"CampaignServiceInvalidBidAmount"}]}`, want: domain.ErrBidAmountRejected},
		{name: "ad group takes no CPC bid", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1229,"ErrorCode":"CampaignServiceCannotSetSearchBidOnAdGroup","Index":0}]}`, want: domain.ErrBidUnwritable},
		{name: "recorded ad group no longer valid", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1201,"ErrorCode":"CampaignServiceInvalidAdGroupId","Index":0}]}`, want: domain.ErrBidUnwritable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write := tc.write
			if write == nil {
				write = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}
			}
			d, calls := msBudgetDispatcherWith(t, msBidStrategy("EnhancedCpc"), write, tc.opts...)
			err := writeMSBid(d, msBidCampaign(), 2)
			if err == nil {
				t.Fatal("want an error")
			}
			var u interface{ Unconfirmed() bool }
			if got := errors.As(err, &u) && u.Unconfirmed(); got != tc.unconfirmed {
				t.Errorf("Unconfirmed = %v, want %v (%v)", got, tc.unconfirmed, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("want %v in the chain, got %v", tc.want, err)
			}
			if tc.want == nil && (errors.Is(err, domain.ErrBidUnwritable) || errors.Is(err, domain.ErrBidAmountRejected)) {
				t.Errorf("a generic failure must not be classified as a specific refusal: %v", err)
			}
			if tc.want == domain.ErrBidAmountRejected {
				var r interface{ BidAmountReason() string }
				if !errors.As(err, &r) || !strings.Contains(r.BidAmountReason(), "Microsoft Advertising refused a max CPC bid of 2") {
					t.Errorf("an amount refusal must carry a client-safe reason, got %v", err)
				}
			}
			if n := len(msWrites(calls())); n != 1 {
				t.Errorf("want exactly one write attempt, got %d", n)
			}
		})
	}
}

// A refusal that answers a PUT RETRIED after a 429 is NOT a definite refusal: the 429'd attempt
// may have applied. It must surface as UNCONFIRMED, never as the 400 amount refusal.
func TestMicrosoft_WriteBid_RefusalAfterARetried429IsUnconfirmed(t *testing.T) {
	var (
		mu    sync.Mutex
		count int
	)
	write := func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[{"Code":1515,"ErrorCode":"CampaignServiceBidAmountsLessThanFloorPrice","Index":0}]}`)
	}
	d, calls := msBudgetDispatcherWith(t, msBidStrategy("EnhancedCpc"), write)
	err := writeMSBid(d, msBidCampaign(), 2)
	var u interface{ Unconfirmed() bool }
	if !errors.As(err, &u) || !u.Unconfirmed() {
		t.Fatalf("want an UNCONFIRMED outcome, got %v", err)
	}
	if errors.Is(err, domain.ErrBidAmountRejected) {
		t.Errorf("a refusal after a retried 429 must not be reported as a definite amount refusal: %v", err)
	}
	if n := len(msWrites(calls())); n != 2 {
		t.Errorf("want the 429 retried once (2 writes), got %d", n)
	}
}
