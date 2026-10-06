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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaLinkClickBidCap is the one ad-set shape the bid write accepts: an ad set of this campaign,
// LOWEST_COST_WITH_BID_CAP, billed and optimized on LINK_CLICKS, with a legible bid_amount.
const metaLinkClickBidCap = `{"id":"777","campaign_id":"555","bid_strategy":"LOWEST_COST_WITH_BID_CAP","billing_event":"LINK_CLICKS","optimization_goal":"LINK_CLICKS","bid_amount":150}`

func metaAdSet(strategy, billing, goal string) string {
	return `{"id":"777","campaign_id":"555","bid_strategy":"` + strategy + `","billing_event":"` + billing + `","optimization_goal":"` + goal + `","bid_amount":150}`
}

// metaBidStub answers the ad-set read with adSetJSON, the account preflight with currency, and
// every POST with postStatus/postBody. Every request is recorded.
type metaBidStub struct {
	d    *MetaDispatcher
	mu   sync.Mutex
	seen []budgetRequest
}

func newMetaBidStub(t *testing.T, conn *model.Connection, adSetJSON, currency string, postStatus int, postBody string) *metaBidStub {
	t.Helper()
	s := &metaBidStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(postStatus)
			_, _ = io.WriteString(w, postBody)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/act_") {
			_, _ = io.WriteString(w, `{"name":"LF","account_status":1,"currency":"`+currency+`"}`)
			return
		}
		_, _ = io.WriteString(w, adSetJSON)
	}))
	t.Cleanup(srv.Close)
	s.d = NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	return s
}

func (s *metaBidStub) requests() []budgetRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]budgetRequest(nil), s.seen...)
}

func writeMetaBid(s *metaBidStub, c *model.Campaign, amount float64) error {
	return s.d.WriteBid(context.Background(), "proj", model.ProviderMetaAds, c, model.BidChange{Amount: amount, Type: model.BidTypeCPC})
}

func TestMeta_WriteBid_LinkClickBidCapPostsBidAmountOnTheAdSetOnly(t *testing.T) {
	for _, tc := range []struct {
		currency string
		amount   float64
		wantBody string
	}{
		{"USD", 2.35, `{"bid_amount":235}`},
		// A zero-decimal currency: the basic unit IS the minor unit.
		{"JPY", 120, `{"bid_amount":120}`},
	} {
		t.Run(tc.currency, func(t *testing.T) {
			s := newMetaBidStub(t, activeMetaConn(goodMetaCreds), metaLinkClickBidCap, tc.currency, http.StatusOK, `{"success":true}`)
			if err := writeMetaBid(s, metaBudgetCampaign(), tc.amount); err != nil {
				t.Fatalf("WriteBid: %v", err)
			}
			w := metaWrites(s.requests())
			if len(w) != 1 || w[0].Path != "/777" {
				t.Fatalf("want exactly one POST to the ad set node /777, got %+v", w)
			}
			// bid_amount ONLY: bid_strategy, billing_event and optimization_goal are never sent.
			if w[0].Body != tc.wantBody {
				t.Errorf("POST body = %s, want %s", w[0].Body, tc.wantBody)
			}
		})
	}
}

// Every Meta campaign this service creates is LOWEST_COST_WITHOUT_CAP billed on IMPRESSIONS, so
// the first case is the ORDINARY refusal, and every case must leave the platform unwritten.
func TestMeta_WriteBid_NonCPCOrUnaddressableRefusedWithZeroWrites(t *testing.T) {
	cases := []struct{ name, adSet string }{
		{"what the create path sends", metaAdSet("LOWEST_COST_WITHOUT_CAP", "IMPRESSIONS", "LINK_CLICKS")},
		{"automatic bidding billed per link click", metaAdSet("LOWEST_COST_WITHOUT_CAP", "LINK_CLICKS", "LINK_CLICKS")},
		{"COST_CAP is a target average", metaAdSet("COST_CAP", "LINK_CLICKS", "LINK_CLICKS")},
		{"MIN_ROAS", metaAdSet("LOWEST_COST_WITH_MIN_ROAS", "IMPRESSIONS", "OFFSITE_CONVERSIONS")},
		{"strategy not reported", `{"id":"777","campaign_id":"555","billing_event":"LINK_CLICKS","optimization_goal":"LINK_CLICKS"}`},
		{"bid cap billed on impressions is a CPM", metaAdSet("LOWEST_COST_WITH_BID_CAP", "IMPRESSIONS", "LINK_CLICKS")},
		{"bid cap billed on any click", metaAdSet("LOWEST_COST_WITH_BID_CAP", "CLICKS", "LINK_CLICKS")},
		{"bid cap optimized for reach", metaAdSet("LOWEST_COST_WITH_BID_CAP", "LINK_CLICKS", "REACH")},
		{"billing not reported", `{"id":"777","campaign_id":"555","bid_strategy":"LOWEST_COST_WITH_BID_CAP","optimization_goal":"LINK_CLICKS"}`},
		{"ad set of another campaign", `{"id":"777","campaign_id":"999","bid_strategy":"LOWEST_COST_WITH_BID_CAP","billing_event":"LINK_CLICKS","optimization_goal":"LINK_CLICKS"}`},
		{"owner not reported", `{"id":"777","bid_strategy":"LOWEST_COST_WITH_BID_CAP","billing_event":"LINK_CLICKS","optimization_goal":"LINK_CLICKS"}`},
		{"unreadable bid_amount", `{"id":"777","campaign_id":"555","bid_strategy":"LOWEST_COST_WITH_BID_CAP","billing_event":"LINK_CLICKS","optimization_goal":"LINK_CLICKS","bid_amount":1.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetaBidStub(t, activeMetaConn(goodMetaCreds), tc.adSet, "USD", http.StatusOK, `{"success":true}`)
			err := writeMetaBid(s, metaBudgetCampaign(), 2)
			if !errors.Is(err, domain.ErrBidUnwritable) {
				t.Fatalf("want ErrBidUnwritable, got %T: %v", err, err)
			}
			assertNoMetaWrite(t, s.requests())
		})
	}
}

func TestMeta_WriteBid_RowAndRequestRefusalsBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Campaign)
		typ    model.BidType
		want   []error
	}{
		{"no provenance recorded", func(c *model.Campaign) { c.Result = []byte(`{"AdSetID":"777"}`) }, model.BidTypeCPC,
			[]error{domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch}},
		{"foreign account", func(c *model.Campaign) { c.Result = []byte(`{"AccountID":"act_999","AdSetID":"777"}`) }, model.BidTypeCPC,
			[]error{domain.ErrCampaignAccountMismatch}},
		{"no recorded ad set", func(c *model.Campaign) { c.Result = []byte(`{"AccountID":"act_777"}`) }, model.BidTypeCPC,
			[]error{domain.ErrBidUnwritable}},
		{"ad set id that cannot address a path", func(c *model.Campaign) { c.Result = []byte(`{"AccountID":"act_777","AdSetID":"7/../x"}`) }, model.BidTypeCPC,
			[]error{domain.ErrBidUnwritable, meta.ErrInvalidAdSetID}},
		{"a unit other than cpc", func(*model.Campaign) {}, model.BidType("cpm"), []error{domain.ErrBidUnwritable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetaBidStub(t, activeMetaConn(goodMetaCreds), metaLinkClickBidCap, "USD", http.StatusOK, `{"success":true}`)
			c := metaBudgetCampaign()
			tc.mutate(c)
			err := s.d.WriteBid(context.Background(), "proj", model.ProviderMetaAds, c, model.BidChange{Amount: 2, Type: tc.typ})
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Errorf("want %v in the chain, got %T: %v", want, err, err)
				}
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("refusal must reach no Graph endpoint, got %d request(s)", n)
			}
		})
	}
}

// The amount is encoded against the account's own currency, so its refusals follow the read —
// but still precede any write.
func TestMeta_WriteBid_AmountAndCurrencyRefusalsWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		currency string
		amount   float64
		want     error
	}{
		{"under one cent", "USD", 0.004, domain.ErrBidAmountRejected},
		{"under one yen", "JPY", 0.4, domain.ErrBidAmountRejected},
		{"over the ceiling", "USD", 1_000_001, domain.ErrBidAmountRejected},
		{"currency with no known scale", "XXX", 2, domain.ErrBidUnwritable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetaBidStub(t, activeMetaConn(goodMetaCreds), metaLinkClickBidCap, tc.currency, http.StatusOK, `{"success":true}`)
			err := writeMetaBid(s, metaBudgetCampaign(), tc.amount)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.want == domain.ErrBidAmountRejected {
				var r interface{ BidAmountReason() string }
				if !errors.As(err, &r) || r.BidAmountReason() == "" {
					t.Errorf("an amount refusal must carry a client-safe reason: %v", err)
				}
			}
			assertNoMetaWrite(t, s.requests())
		})
	}
}

// The mutate's ambiguity contract. A throttle — Meta's 429 or its HTTP-400 rate-limit code — is
// UNCONFIRMED and is NOT retried in-call, so a later definite refusal can never answer for an
// attempt that may have applied.
func TestMeta_WriteBid_WriteOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
		want        error
	}{
		{"definite invalid-parameter naming the bid is an amount refusal", http.StatusBadRequest, `{"error":{"message":"Bid amount too low for this ad set","code":100}}`, false, domain.ErrBidAmountRejected},
		{"definite invalid-parameter about something else", http.StatusBadRequest, `{"error":{"message":"Invalid targeting","code":100}}`, false, nil},
		{"definite 403", http.StatusForbidden, `{"error":{"message":"denied","code":200}}`, false, nil},
		{"5xx is ambiguous", http.StatusBadGateway, `{}`, true, nil},
		{"429 is ambiguous and not retried", http.StatusTooManyRequests, `{}`, true, nil},
		{"HTTP-400 rate limit is ambiguous and not retried", http.StatusBadRequest, `{"error":{"message":"User request limit reached","code":17}}`, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMetaBidStub(t, activeMetaConn(goodMetaCreds), metaLinkClickBidCap, "USD", tc.status, tc.body)
			err := writeMetaBid(s, metaBudgetCampaign(), 2)
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
					t.Errorf("the reason must be this service's sentence, never Meta's text: %v", err)
				}
			}
			if w := metaWrites(s.requests()); len(w) != 1 {
				t.Errorf("want exactly one POST (no in-call retry), got %d", len(w))
			}
		})
	}
}

func TestMeta_WriteBid_NoAccountSelectedRefusedBeforeAnyCall(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = ""
	s := newMetaBidStub(t, conn, metaLinkClickBidCap, "USD", http.StatusOK, `{"success":true}`)
	err := writeMetaBid(s, metaBudgetCampaign(), 2)
	if !errors.Is(err, domain.ErrAccountNotSelected) {
		t.Fatalf("want ErrAccountNotSelected, got %v", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("refusal must reach no Graph endpoint, got %d request(s)", n)
	}
}
