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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

const (
	redditBidAdGroupPath = "/api/v3/ad_accounts/t2_acct/ad_groups/t5_ag"
	// redditManualCPCAdGroup is the shape the bid write accepts: an ad group of this campaign,
	// MANUAL_BIDDING, CPC, a legible bid_value.
	redditManualCPCAdGroup = `{"data":{"id":"t5_ag","campaign_id":"t3_c","bid_strategy":"MANUAL_BIDDING","bid_type":"CPC","bid_value":1500000}}`
)

// redditManualCBOCampaign is the campaign the bid write accepts: CBO on (as the create path
// sets it) with the campaign's own MANUAL_BIDDING strategy.
const redditManualCBOCampaign = `{"data":{"id":"t3_c","ad_account_id":"t2_acct","is_campaign_budget_optimization":true,"bid_strategy":"MANUAL_BIDDING"}}`

// newRedditBidStub is newRedditBudgetStub with the two GETs answered separately: the CAMPAIGN
// read (its bid strategy) with campaignBody, the AD GROUP read with adGroupStatus/adGroupBody.
func newRedditBidStub(t *testing.T, campaignBody string, adGroupStatus int, adGroupBody string, patchStatus int, patchBody string) *redditBudgetStub {
	t.Helper()
	s := &redditBudgetStub{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			w.WriteHeader(patchStatus)
			_, _ = io.WriteString(w, patchBody)
		case r.URL.Path == redditBudgetCampaignPath:
			_, _ = io.WriteString(w, campaignBody)
		default:
			w.WriteHeader(adGroupStatus)
			_, _ = io.WriteString(w, adGroupBody)
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.tokens.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	s.d = NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	return s
}

func redditAdGroup(strategy, bidType string) string {
	return `{"data":{"id":"t5_ag","campaign_id":"t3_c","bid_strategy":"` + strategy + `","bid_type":"` + bidType + `","bid_value":1500000}}`
}

func writeRedditBid(s *redditBudgetStub, c *model.Campaign, amount float64) error {
	return s.d.WriteBid(context.Background(), "proj", model.ProviderRedditAds, c, model.BidChange{Amount: amount, Type: model.BidTypeCPC})
}

func TestReddit_WriteBid_ManualCPCPatchesBidValueOnTheAdGroupOnly(t *testing.T) {
	s := newRedditBidStub(t, redditManualCBOCampaign, http.StatusOK, redditManualCPCAdGroup, http.StatusOK,
		`{"data":{"id":"t5_ag","bid_value":2350000}}`)
	if err := writeRedditBid(s, redditBudgetCampaign(), 2.35); err != nil {
		t.Fatalf("WriteBid: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 3 || reqs[0].Path != redditBudgetCampaignPath || reqs[1].Method != http.MethodGet || reqs[1].Path != redditBidAdGroupPath {
		t.Fatalf("want the campaign GET, the ad group GET, then one PATCH, got %+v", reqs)
	}
	p := s.patches()
	if len(p) != 1 || p[0].Path != redditBidAdGroupPath {
		t.Fatalf("want exactly one PATCH of the ad group, got %+v", p)
	}
	// bid_value ONLY: bid_strategy and bid_type are never sent, so the strategy cannot switch.
	if p[0].Body != `{"data":{"bid_value":2350000}}` {
		t.Errorf("PATCH body = %s", p[0].Body)
	}
}

// Every Reddit campaign this service creates is BIDLESS, so this is the ORDINARY refusal, and it
// must leave the platform unwritten.
func TestReddit_WriteBid_AutomatedOrUnaddressableRefusedWithZeroWrites(t *testing.T) {
	cases := []struct{ name, getBody string }{
		{"BIDLESS (what the create path sends)", redditAdGroup("BIDLESS", "CPC")},
		{"MAXIMIZE_VOLUME", redditAdGroup("MAXIMIZE_VOLUME", "CPC")},
		{"TARGET_CPX", redditAdGroup("TARGET_CPX", "CPC")},
		{"strategy not reported", `{"data":{"id":"t5_ag","campaign_id":"t3_c","bid_type":"CPC"}}`},
		{"CPM ad group", redditAdGroup("MANUAL_BIDDING", "CPM")},
		{"bid type not reported", `{"data":{"id":"t5_ag","campaign_id":"t3_c","bid_strategy":"MANUAL_BIDDING"}}`},
		{"ad group of another campaign", `{"data":{"id":"t5_ag","campaign_id":"t3_other","bid_strategy":"MANUAL_BIDDING","bid_type":"CPC"}}`},
		// An unreported owner is refused like a different one — never assumed to be this campaign.
		{"campaign_id not reported", `{"data":{"id":"t5_ag","bid_strategy":"MANUAL_BIDDING","bid_type":"CPC","bid_value":1500000}}`},
		{"unreadable bid_value", `{"data":{"id":"t5_ag","campaign_id":"t3_c","bid_strategy":"MANUAL_BIDDING","bid_type":"CPC","bid_value":"1.5"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBidStub(t, redditManualCBOCampaign, http.StatusOK, tc.getBody, http.StatusOK, `{"data":{}}`)
			err := writeRedditBid(s, redditBudgetCampaign(), 2)
			if !errors.Is(err, domain.ErrBidUnwritable) {
				t.Fatalf("want ErrBidUnwritable, got %T: %v", err, err)
			}
			s.assertNoPatch(t)
		})
	}
}

// A deleted ad group is not a deleted campaign: it is an unaddressable bid (409), not the 404
// that would tell the caller the whole campaign is gone.
func TestReddit_WriteBid_AbsentAdGroupIsUnwritableNotAbsentCampaign(t *testing.T) {
	s := newRedditBidStub(t, redditManualCBOCampaign, http.StatusNotFound, `{}`, http.StatusOK, `{"data":{}}`)
	err := writeRedditBid(s, redditBudgetCampaign(), 2)
	if !errors.Is(err, domain.ErrBidUnwritable) || errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("want ErrBidUnwritable and not ErrPlatformCampaignAbsent, got %v", err)
	}
	s.assertNoPatch(t)
}

func TestReddit_WriteBid_RowAndRequestRefusalsBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Campaign)
		amount float64
		want   []error
	}{
		{"no provenance recorded", func(c *model.Campaign) { c.Result = json.RawMessage(`{"adGroupId":"t5_ag"}`) }, 2,
			[]error{domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch}},
		{"foreign account", func(c *model.Campaign) { c.Result = json.RawMessage(`{"accountId":"t2_other","adGroupId":"t5_ag"}`) }, 2,
			[]error{domain.ErrCampaignAccountMismatch}},
		{"no recorded ad group", func(c *model.Campaign) { c.Result = json.RawMessage(`{"accountId":"t2_acct"}`) }, 2,
			[]error{domain.ErrBidUnwritable}},
		{"ad group id that cannot address a path", func(c *model.Campaign) {
			c.Result = json.RawMessage(`{"accountId":"t2_acct","adGroupId":"t5/../x"}`)
		}, 2, []error{domain.ErrBidUnwritable, reddit.ErrInvalidAdGroupID}},
		{"rounds to zero micros", func(*model.Campaign) {}, 0.0000001, []error{domain.ErrBidAmountRejected}},
		{"over the ceiling", func(*model.Campaign) {}, 1_000_001, []error{domain.ErrBidAmountRejected}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBidStub(t, redditManualCBOCampaign, http.StatusOK, redditManualCPCAdGroup, http.StatusOK, `{"data":{}}`)
			c := redditBudgetCampaign()
			tc.mutate(c)
			err := writeRedditBid(s, c, tc.amount)
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Errorf("want %v in the chain, got %T: %v", want, err, err)
				}
			}
			if errors.Is(err, domain.ErrBidAmountRejected) {
				var r interface{ BidAmountReason() string }
				if !errors.As(err, &r) || r.BidAmountReason() == "" {
					t.Errorf("an amount refusal must carry a client-safe reason: %v", err)
				}
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("refusal must reach no Ads API endpoint, got %d request(s)", n)
			}
		})
	}
}

// The mutate's ambiguity contract, as on the budget write: a definite 4xx is a refusal (and a
// 400 naming bid_value is the amount refusal); a 5xx or a 2xx echoing another ad group or bid MAY
// have applied and is UNCONFIRMED.
func TestReddit_WriteBid_MutateOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
		want        error
	}{
		{"definite 400 naming bid_value is an amount refusal", http.StatusBadRequest, `{"error":{"fields":[{"field":"bid_value","message":"too low"}]}}`, false, domain.ErrBidAmountRejected},
		{"definite 400 about something else", http.StatusBadRequest, `{"error":"bad request"}`, false, nil},
		// Mentions bid_value, but not as a structured field error: a strategy race or an echoed
		// payload must not be reported to the caller as an amount problem.
		{"definite 400 mentioning bid_value outside fields[]", http.StatusBadRequest, `{"error":{"message":"bid_value cannot be set while bid_strategy is BIDLESS"}}`, false, nil},
		{"definite 403", http.StatusForbidden, `{}`, false, nil},
		{"5xx is ambiguous", http.StatusBadGateway, `{}`, true, nil},
		{"2xx echoing another ad group", http.StatusOK, `{"data":{"id":"t5_other","bid_value":2000000}}`, true, nil},
		{"2xx echoing another bid", http.StatusOK, `{"data":{"id":"t5_ag","bid_value":1}}`, true, nil},
		{"2xx whose data is not an object", http.StatusOK, `{"data":[1]}`, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBidStub(t, redditManualCBOCampaign, http.StatusOK, redditManualCPCAdGroup, tc.status, tc.body)
			err := writeRedditBid(s, redditBudgetCampaign(), 2)
			if err == nil {
				t.Fatal("expected an error")
			}
			var u interface{ Unconfirmed() bool }
			if got := errors.As(err, &u) && u.Unconfirmed(); got != tc.unconfirmed {
				t.Errorf("Unconfirmed() = %v, want %v: %v", got, tc.unconfirmed, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("want %v, got %v", tc.want, err)
			}
			if tc.want == nil && (errors.Is(err, domain.ErrBidUnwritable) || errors.Is(err, domain.ErrBidAmountRejected)) {
				t.Errorf("a mutate outcome must not be classified as a pre-mutate refusal: %v", err)
			}
			if tc.want == domain.ErrBidAmountRejected {
				var r interface{ BidAmountReason() string }
				if !errors.As(err, &r) || strings.Contains(r.BidAmountReason(), "too low") {
					t.Errorf("the reason must be this service's sentence, never Reddit's body: %v", err)
				}
			}
			if p := s.patches(); len(p) != 1 {
				t.Errorf("want exactly one PATCH, got %d", len(p))
			}
		})
	}
}

func TestReddit_WriteBid_UnusableConnectionRefusedBeforeAnyCall(t *testing.T) {
	conn := activeRedditConn(goodRedditCreds)
	conn.Status = model.StatusInactive
	d := NewRedditDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		reddit.WithBaseURL("http://127.0.0.1:0/api/v3"), reddit.WithTokenURL("http://127.0.0.1:0/token"))
	err := d.WriteBid(context.Background(), "proj", model.ProviderRedditAds, redditBudgetCampaign(),
		model.BidChange{Amount: 2, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("want ErrConnectionNotUsable, got %v", err)
	}
}

// The CAMPAIGN's strategy governs its ad groups under CBO (which the create path turns on), so a
// campaign whose own strategy is not manual is refused even when the ad group reads as manual —
// and nothing past the campaign read is requested.
func TestReddit_WriteBid_CampaignStrategyMustAllowAManualAdGroupBid(t *testing.T) {
	cases := []struct {
		name, campaignBody string
		want               error
	}{
		{"CBO on, BIDLESS campaign (what the create path sends)", `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"bid_strategy":"BIDLESS"}}`, domain.ErrBidUnwritable},
		{"CBO on, campaign strategy not reported", `{"data":{"id":"t3_c","is_campaign_budget_optimization":true}}`, domain.ErrBidUnwritable},
		{"CBO flag not reported", `{"data":{"id":"t3_c","bid_strategy":"MANUAL_BIDDING"}}`, domain.ErrBidUnwritable},
		{"CBO off, automated campaign strategy", `{"data":{"id":"t3_c","is_campaign_budget_optimization":false,"bid_strategy":"MAXIMIZE_VOLUME"}}`, domain.ErrBidUnwritable},
		{"campaign under another account", `{"data":{"id":"t3_c","ad_account_id":"t2_other","is_campaign_budget_optimization":true,"bid_strategy":"MANUAL_BIDDING"}}`, domain.ErrCampaignAccountMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBidStub(t, tc.campaignBody, http.StatusOK, redditManualCPCAdGroup, http.StatusOK, `{"data":{}}`)
			err := writeRedditBid(s, redditBudgetCampaign(), 2)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %T: %v", tc.want, err, err)
			}
			s.assertNoPatch(t)
			if n := len(s.requests()); n != 1 {
				t.Errorf("want only the campaign read, got %d request(s)", n)
			}
		})
	}
}

// With CBO OFF and no campaign-level strategy reported, the ad group governs its own bid.
func TestReddit_WriteBid_CBOOffDefersToTheAdGroup(t *testing.T) {
	s := newRedditBidStub(t, `{"data":{"id":"t3_c","is_campaign_budget_optimization":false}}`, http.StatusOK, redditManualCPCAdGroup, http.StatusOK, `{"data":{}}`)
	if err := writeRedditBid(s, redditBudgetCampaign(), 2); err != nil {
		t.Fatalf("WriteBid: %v", err)
	}
	if n := len(s.patches()); n != 1 {
		t.Errorf("want one PATCH, got %d", n)
	}
}

func TestReddit_WriteBid_CampaignAbsentIs404Sentinel(t *testing.T) {
	s := newRedditBudgetStub(t, http.StatusNotFound, `{}`, http.StatusOK, `{"data":{}}`)
	if err := writeRedditBid(s, redditBudgetCampaign(), 2); !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("want ErrPlatformCampaignAbsent, got %v", err)
	}
	s.assertNoPatch(t)
}
