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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

const (
	xBidLineItemPath = "/12/accounts/acc1/line_items/li1"
	// xManualCPCLineItem is the shape the bid write accepts: a line item of this campaign, MAX,
	// charged per link click, with a legible bid.
	xManualCPCLineItem = `{"data":{"id":"li1","campaign_id":"c1","bid_strategy":"MAX","pay_by":"LINK_CLICK","bid_amount_local_micro":1500000}}`
)

func xLineItem(strategy, payBy string) string {
	return `{"data":{"id":"li1","campaign_id":"c1","bid_strategy":"` + strategy + `","pay_by":"` + payBy + `","bid_amount_local_micro":1500000}}`
}

func xBidCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderTwitterAds, PlatformCampaignID: "c1",
		Result: json.RawMessage(`{"AccountID":"acc1","CampaignID":"c1","LineItemID":"li1"}`),
	}
}

type xBidRequest struct {
	Method string
	Path   string
	Query  string
}

// xBidStub answers the line-item GET with getStatus/getBody and every PUT with putStatus/putBody,
// recording each request with its query string — X carries update parameters there.
type xBidStub struct {
	d    *TwitterDispatcher
	mu   sync.Mutex
	seen []xBidRequest
}

func newXBidStub(t *testing.T, getStatus int, getBody string, putStatus int, putBody string) *xBidStub {
	t.Helper()
	return newXBidStubConn(t, activeTwitterConn(goodTwitterCreds), getStatus, getBody, putStatus, putBody)
}

func newXBidStubConn(t *testing.T, conn *model.Connection, getStatus int, getBody string, putStatus int, putBody string) *xBidStub {
	t.Helper()
	s := &xBidStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, xBidRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			w.WriteHeader(putStatus)
			_, _ = io.WriteString(w, putBody)
			return
		}
		w.WriteHeader(getStatus)
		_, _ = io.WriteString(w, getBody)
	}))
	t.Cleanup(srv.Close)
	s.d = NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return s
}

func (s *xBidStub) requests() []xBidRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]xBidRequest(nil), s.seen...)
}

func (s *xBidStub) puts() []xBidRequest {
	var out []xBidRequest
	for _, r := range s.requests() {
		if r.Method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

func (s *xBidStub) assertNoPut(t *testing.T) {
	t.Helper()
	if p := s.puts(); len(p) != 0 {
		t.Fatalf("refusal still issued %d PUT(s): %+v", len(p), p)
	}
}

func writeXBid(s *xBidStub, c *model.Campaign, amount float64) error {
	return s.d.WriteBid(context.Background(), "proj", model.ProviderTwitterAds, c, model.BidChange{Amount: amount, Type: model.BidTypeCPC})
}

func TestTwitter_WriteBid_ManualCPCPutsBidAmountOnTheLineItemOnly(t *testing.T) {
	s := newXBidStub(t, http.StatusOK, xManualCPCLineItem, http.StatusOK,
		`{"data":{"id":"li1","bid_amount_local_micro":2350000}}`)
	if err := writeXBid(s, xBidCampaign(), 2.35); err != nil {
		t.Fatalf("WriteBid: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Path != xBidLineItemPath {
		t.Fatalf("want the line item GET then one PUT, got %+v", reqs)
	}
	if !strings.Contains(reqs[0].Query, "with_deleted=true") {
		t.Errorf("the read must include deleted line items so a deleted one is named: %q", reqs[0].Query)
	}
	p := s.puts()
	if len(p) != 1 || p[0].Path != xBidLineItemPath {
		t.Fatalf("want exactly one PUT of the line item, got %+v", p)
	}
	// bid_amount_local_micro ONLY: bid_strategy and pay_by are never sent.
	if p[0].Query != "bid_amount_local_micro=2350000" {
		t.Errorf("PUT query = %q", p[0].Query)
	}
}

// Every X campaign this service creates is AUTO, so the first case is the ORDINARY refusal, and
// every case must leave the platform unwritten.
func TestTwitter_WriteBid_AutomatedOrUnaddressableRefusedWithZeroWrites(t *testing.T) {
	cases := []struct {
		name      string
		getStatus int
		getBody   string
	}{
		{"AUTO (what the create path sends)", http.StatusOK, xLineItem("AUTO", "LINK_CLICK")},
		{"TARGET", http.StatusOK, xLineItem("TARGET", "LINK_CLICK")},
		{"strategy not reported", http.StatusOK, `{"data":{"id":"li1","campaign_id":"c1","pay_by":"LINK_CLICK"}}`},
		{"charged per impression", http.StatusOK, xLineItem("MAX", "IMPRESSION")},
		{"charged per app click", http.StatusOK, xLineItem("MAX", "APP_CLICK")},
		{"charge unit not reported", http.StatusOK, `{"data":{"id":"li1","campaign_id":"c1","bid_strategy":"MAX"}}`},
		{"line item of another campaign", http.StatusOK, `{"data":{"id":"li1","campaign_id":"c9","bid_strategy":"MAX","pay_by":"LINK_CLICK"}}`},
		{"owner not reported", http.StatusOK, `{"data":{"id":"li1","bid_strategy":"MAX","pay_by":"LINK_CLICK"}}`},
		{"unreadable bid", http.StatusOK, `{"data":{"id":"li1","campaign_id":"c1","bid_strategy":"MAX","pay_by":"LINK_CLICK","bid_amount_local_micro":"1.5"}}`},
		{"deleted line item", http.StatusOK, `{"data":{"id":"li1","campaign_id":"c1","bid_strategy":"MAX","pay_by":"LINK_CLICK","deleted":true}}`},
		{"absent line item", http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newXBidStub(t, tc.getStatus, tc.getBody, http.StatusOK, `{"data":{}}`)
			err := writeXBid(s, xBidCampaign(), 2)
			if !errors.Is(err, domain.ErrBidUnwritable) {
				t.Fatalf("want ErrBidUnwritable, got %T: %v", err, err)
			}
			if errors.Is(err, domain.ErrPlatformCampaignAbsent) {
				t.Errorf("a missing line item is not a missing campaign: %v", err)
			}
			s.assertNoPut(t)
		})
	}
}

func TestTwitter_WriteBid_RowAndRequestRefusalsBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Campaign)
		amount float64
		want   []error
	}{
		{"no provenance recorded", func(c *model.Campaign) { c.Result = json.RawMessage(`{"LineItemID":"li1"}`) }, 2,
			[]error{domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch}},
		{"foreign account", func(c *model.Campaign) { c.Result = json.RawMessage(`{"AccountID":"acc9","LineItemID":"li1"}`) }, 2,
			[]error{domain.ErrCampaignAccountMismatch}},
		{"no recorded line item", func(c *model.Campaign) { c.Result = json.RawMessage(`{"AccountID":"acc1"}`) }, 2,
			[]error{domain.ErrBidUnwritable}},
		{"line item id that cannot address a path", func(c *model.Campaign) {
			c.Result = json.RawMessage(`{"AccountID":"acc1","LineItemID":"li/../x"}`)
		}, 2, []error{domain.ErrBidUnwritable, twitter.ErrInvalidLineItemID}},
		{"rounds to zero micros", func(*model.Campaign) {}, 0.0000001, []error{domain.ErrBidAmountRejected}},
		{"over the ceiling", func(*model.Campaign) {}, 1_000_001, []error{domain.ErrBidAmountRejected}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newXBidStub(t, http.StatusOK, xManualCPCLineItem, http.StatusOK, `{"data":{}}`)
			c := xBidCampaign()
			tc.mutate(c)
			err := writeXBid(s, c, tc.amount)
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

func TestTwitter_WriteBid_NonCPCUnitRefusedBeforeAnyCall(t *testing.T) {
	s := newXBidStub(t, http.StatusOK, xManualCPCLineItem, http.StatusOK, `{"data":{}}`)
	err := s.d.WriteBid(context.Background(), "proj", model.ProviderTwitterAds, xBidCampaign(),
		model.BidChange{Amount: 2, Type: model.BidType("cpm")})
	if !errors.Is(err, domain.ErrBidUnwritable) {
		t.Fatalf("want ErrBidUnwritable, got %v", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("refusal must reach no Ads API endpoint, got %d request(s)", n)
	}
}

// The mutate's ambiguity contract. A 429 is UNCONFIRMED and is NOT retried in-call, so a later
// definite refusal can never answer for an attempt that may have applied.
func TestTwitter_WriteBid_WriteOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
		want        error
	}{
		{"INVALID_PARAMETER on bid_amount_local_micro is an amount refusal", http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER","parameter":"bid_amount_local_micro","message":"too low"}]}`, false, domain.ErrBidAmountRejected},
		{"a bid-looking code with no parameter is not an amount refusal", http.StatusBadRequest, `{"errors":[{"code":"INVALID_BID_AMOUNT","message":"too low"}]}`, false, nil},
		{"a bid-unit code is not an amount refusal", http.StatusBadRequest, `{"errors":[{"code":"INVALID_BID_TYPE"}]}`, false, nil},
		{"FORBIDDEN is not an amount refusal", http.StatusBadRequest, `{"errors":[{"code":"FORBIDDEN","parameter":"bid_amount_local_micro"}]}`, false, nil},
		{"definite 400 about something else", http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`, false, nil},
		{"definite 403", http.StatusForbidden, `{}`, false, nil},
		{"5xx is ambiguous", http.StatusBadGateway, `{}`, true, nil},
		{"429 is ambiguous and not retried", http.StatusTooManyRequests, `{}`, true, nil},
		{"2xx echoing another line item", http.StatusOK, `{"data":{"id":"li9","bid_amount_local_micro":2000000}}`, true, nil},
		{"2xx echoing another bid", http.StatusOK, `{"data":{"id":"li1","bid_amount_local_micro":1}}`, true, nil},
		{"2xx whose data is not an object", http.StatusOK, `{"data":[1]}`, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newXBidStub(t, http.StatusOK, xManualCPCLineItem, tc.status, tc.body)
			err := writeXBid(s, xBidCampaign(), 2)
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
					t.Errorf("the reason must be this service's sentence, never X's text: %v", err)
				}
			}
			if p := s.puts(); len(p) != 1 {
				t.Errorf("want exactly one PUT (no in-call retry), got %d", len(p))
			}
		})
	}
}

func TestTwitter_WriteBid_UnusableConnectionRefusedBeforeAnyCall(t *testing.T) {
	conn := activeTwitterConn(goodTwitterCreds)
	conn.Status = model.StatusInactive
	d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		twitter.WithBaseURL("http://127.0.0.1:0"), twitter.WithWriteDelay(0))
	err := d.WriteBid(context.Background(), "proj", model.ProviderTwitterAds, xBidCampaign(),
		model.BidChange{Amount: 2, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("want ErrConnectionNotUsable, got %v", err)
	}
}

// A stored account id that cannot address an X path is a defect of the CONNECTION: an unusable
// connection (409), refused before any request — never a generic upstream failure (503). The row
// records the same id so the account-match guard cannot be what refuses it.
func TestTwitter_WriteBid_MalformedStoredAccountIDIsUnusableConnection(t *testing.T) {
	for _, bad := range []string{"acc1/../acc2", "acc1?x=1", "acc 1"} {
		t.Run(bad, func(t *testing.T) {
			conn := activeTwitterConn(goodTwitterCreds)
			conn.AccountID = bad
			s := newXBidStubConn(t, conn, http.StatusOK, xManualCPCLineItem, http.StatusOK, `{"data":{}}`)
			c := xBidCampaign()
			blob, _ := json.Marshal(map[string]string{"AccountID": bad, "LineItemID": "li1"})
			c.Result = blob
			err := writeXBid(s, c, 2)
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("want ErrConnectionNotUsable, got %T: %v", err, err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("refusal must reach no Ads API endpoint, got %d request(s)", n)
			}
		})
	}
}
