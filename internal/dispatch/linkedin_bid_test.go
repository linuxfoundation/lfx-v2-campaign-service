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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// ─── bid lever ───

func writeLIBid(d *LinkedInDispatcher, row *model.Campaign, amount float64) error {
	return d.WriteBid(context.Background(), "proj", model.ProviderLinkedInAds, row, model.BidChange{Amount: amount, Type: model.BidTypeCPC})
}

func liPosts(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func TestLinkedIn_WriteBid_ManualCPCWritesUnitCostOnly(t *testing.T) {
	for _, opt := range []string{"NONE", "ENHANCED_CONVERSION"} {
		t.Run(opt, func(t *testing.T) {
			d, api := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200,
				body: liCampaignJSON(replace(`"optimizationTargetType":"NONE"`, `"optimizationTargetType":"`+opt+`"`))}})
			if err := writeLIBid(d, liRow(), 6.5); err != nil {
				t.Fatalf("WriteBid: %v", err)
			}
			reqs := api.requests()
			posts := liPosts(reqs)
			if len(reqs) != 2 || reqs[0].Method != http.MethodGet || len(posts) != 1 {
				t.Fatalf("requests = %+v, want one read then one write", reqs)
			}
			body := posts[0].Body
			if !strings.Contains(body, `"unitCost":{"amount":"6.50","currencyCode":"USD"}`) {
				t.Errorf("write body = %s", body)
			}
			for _, never := range []string{"optimizationTargetType", "costType", "dailyBudget", "status"} {
				if strings.Contains(body, never) {
					t.Errorf("the bid write sent %s — it must write the amount only: %s", never, body)
				}
			}
		})
	}
}

// The refusal table. Every case must return its sentinel AND leave the platform unwritten.
func TestLinkedIn_WriteBid_GuardsRefuseWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"automated MAX_CLICK", liCampaignJSON(replace(`"NONE"`, `"MAX_CLICK"`)), domain.ErrBidUnwritable},
		{"automated MAX_CONVERSION", liCampaignJSON(replace(`"NONE"`, `"MAX_CONVERSION"`)), domain.ErrBidUnwritable},
		{"target cost", liCampaignJSON(replace(`"NONE"`, `"TARGET_COST_PER_CLICK"`)), domain.ErrBidUnwritable},
		{"cost cap on CPM", liCampaignJSON(replace(`"NONE"`, `"CAP_COST_AND_MAXIMIZE_CLICKS"`), replace(`"CPC"`, `"CPM"`)), domain.ErrBidUnwritable},
		{"unreported strategy", liCampaignJSON(replace(`"optimizationTargetType":"NONE",`, ``)), domain.ErrBidUnwritable},
		{"manual CPM", liCampaignJSON(replace(`"costType":"CPC"`, `"costType":"CPM"`)), domain.ErrBidUnwritable},
		{"unreported cost type", liCampaignJSON(replace(`"costType":"CPC",`, ``)), domain.ErrBidUnwritable},
		{"illegible unitCost", liCampaignJSON(replace(`"5.25"`, `"five"`)), domain.ErrBidUnwritable},
		{"no unitCost", liCampaignJSON(replace(`"unitCost":{"amount":"5.25","currencyCode":"USD"},`, ``)), domain.ErrBidUnwritable},
		{"non-USD bid", liCampaignJSON(replace(`"5.25","currencyCode":"USD"`, `"5.25","currencyCode":"EUR"`)), domain.ErrBidUnwritable},
		{"unreported currency", liCampaignJSON(replace(`"5.25","currencyCode":"USD"`, `"5.25"`)), domain.ErrBidUnwritable},
		{"unreported account", liCampaignJSON(replace(`"account":"urn:li:sponsoredAccount:123456789",`, ``)), domain.ErrBidUnwritable},
		{"another account", liCampaignJSON(replace("sponsoredAccount:123456789", "sponsoredAccount:999")), domain.ErrCampaignAccountMismatch},
		{"removed upstream", liCampaignJSON(replace(`"status":"ACTIVE"`, `"status":"REMOVED"`)), domain.ErrBidUnwritable},
		{"unreported status", liCampaignJSON(replace(`"status":"ACTIVE",`, ``)), domain.ErrBidUnwritable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, api := liLeverDispatcher(t, map[string]settingsRoute{liCampaignPath: {status: 200, body: tc.body}})
			err := writeLIBid(d, liRow(), 6.5)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if p := liPosts(api.requests()); len(p) != 0 {
				t.Fatalf("a refusal still wrote: %+v", p)
			}
		})
	}
}

func TestLinkedIn_WriteBid_RowFactsRefuseBeforeAnyRequest(t *testing.T) {
	adopted := liRow()
	adopted.Result = []byte(`{"accountId":"123456789","adopted":true}`)
	unknown := liRow()
	unknown.Result = []byte(`{}`)
	badID := liRow()
	badID.PlatformCampaignID = "77/7"
	pastInt64 := liRow()
	pastInt64.PlatformCampaignID = "9223372036854775808"
	padded := liRow()
	padded.PlatformCampaignID = " 777"
	trailing := liRow()
	trailing.PlatformCampaignID = "777 "
	cases := []struct {
		name   string
		row    *model.Campaign
		amount float64
		check  func(error) bool
	}{
		{"adopted row", adopted, 6.5, func(e error) bool {
			return errors.Is(e, domain.ErrBidAdoptedCampaign) && errors.Is(e, domain.ErrBidUnwritable) && strings.Contains(e.Error(), "ADOPTED")
		}},
		{"unknown provenance", unknown, 6.5, func(e error) bool {
			return errors.Is(e, domain.ErrCampaignProvenanceUnknown) && errors.Is(e, domain.ErrCampaignAccountMismatch)
		}},
		{"malformed campaign id", badID, 6.5, func(e error) bool { return errors.Is(e, domain.ErrBidUnwritable) }},
		{"campaign id past int64", pastInt64, 6.5, func(e error) bool { return errors.Is(e, domain.ErrBidUnwritable) }},
		// VERBATIM: a padded stored id is not campaign 777 and must never read or mutate it.
		{"leading space in stored id", padded, 6.5, func(e error) bool { return errors.Is(e, domain.ErrBidUnwritable) }},
		{"trailing space in stored id", trailing, 6.5, func(e error) bool { return errors.Is(e, domain.ErrBidUnwritable) }},
		{"sub-cent amount", liRow(), 2.345, func(e error) bool { return errors.Is(e, domain.ErrBidAmountRejected) }},
		{"zero amount", liRow(), 0, func(e error) bool { return errors.Is(e, domain.ErrBidAmountRejected) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, api := liLeverDispatcher(t, nil)
			err := writeLIBid(d, tc.row, tc.amount)
			if !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
			if n := len(api.requests()); n != 0 {
				t.Fatalf("%d request(s) sent for a refusal decided by the row or request alone", n)
			}
		})
	}
	// A non-CPC bid type is refused too.
	d, api := liLeverDispatcher(t, nil)
	err := d.WriteBid(context.Background(), "proj", model.ProviderLinkedInAds, liRow(), model.BidChange{Amount: 5, Type: "cpm"})
	if !errors.Is(err, domain.ErrBidUnwritable) || len(api.requests()) != 0 {
		t.Fatalf("cpm bid: err = %v, requests = %d", err, len(api.requests()))
	}
}

// WRITE-SAFETY CLASSIFICATION. Every outcome of the one write maps to exactly one answer: nil
// (confirmed), UNCONFIRMED (may have applied — 503 "verify"), REJECTED amount (400, only on
// LinkedIn's parsed, identity-checked envelope) or a DEFINITE failure (nothing applied).
func TestLinkedIn_WriteBid_WriteOutcomeClassification(t *testing.T) {
	envelope := `{"serviceErrorCode":10007,"status":400,"code":"FIELD_VALUE_TOO_LOW","errorDetailType":"com.linkedin.common.error.BadRequest",` +
		`"errorDetails":{"inputErrors":[{"input":{"value":{"decimalValue":"6.50"},"inputPath":{"fieldPath":"/Campaign/unitCost/amount"}},"minValue":"7.00","code":"FIELD_VALUE_TOO_LOW","description":"x"}]}}`
	cases := []struct {
		name            string
		status          int
		body            string
		wantNil         bool
		wantUnconfirmed bool
		wantRejected    bool
	}{
		{"204 confirmed", 204, ``, true, false, false},
		{"5xx unconfirmed", 503, `{}`, false, true, false},
		{"429 unconfirmed", 429, `{}`, false, true, false},
		{"3xx unconfirmed", 302, `{}`, false, true, false},
		{"401 on the write unconfirmed", 401, `{"status":401}`, false, true, false},
		{"structured amount refusal", 400, envelope, false, false, true},
		{"envelope blaming another field is definite", 400, strings.ReplaceAll(envelope, "unitCost", "dailyBudget"), false, false, false},
		{"unstructured 400 is definite", 400, `{"status":400,"message":"bad bid"}`, false, false, false},
		{"403 is definite", 403, `{"status":403}`, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLIWriteServer(t, liCampaignJSON(), tc.status, tc.body)
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.url))
			err := writeLIBid(d, liRow(), 6.5)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var u interface{ Unconfirmed() bool }
			if got := errors.As(err, &u) && u.Unconfirmed(); got != tc.wantUnconfirmed {
				t.Errorf("unconfirmed = %v, want %v (%v)", got, tc.wantUnconfirmed, err)
			}
			if got := errors.Is(err, domain.ErrBidAmountRejected); got != tc.wantRejected {
				t.Errorf("rejected = %v, want %v (%v)", got, tc.wantRejected, err)
			}
			if tc.wantRejected {
				var r interface{ BidAmountReason() string }
				if !errors.As(err, &r) || r.BidAmountReason() == "" || strings.Contains(r.BidAmountReason(), "minValue") {
					t.Errorf("rejection carries no client-safe reason: %v", err)
				}
			}
			if n := srv.posts(); n != 1 {
				t.Errorf("the write was sent %d times, want exactly once", n)
			}
		})
	}
}

// liWriteServer answers the campaign GET with a fixed body and every POST with (status, body),
// counting POSTs under a mutex.
type liWriteServer struct {
	url string
	mu  *sync.Mutex
	n   *int
}

func (s liWriteServer) posts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.n
}

func newLIWriteServer(t *testing.T, read string, status int, body string) liWriteServer {
	t.Helper()
	out := liWriteServer{mu: &sync.Mutex{}, n: new(int)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			out.mu.Lock()
			*out.n++
			out.mu.Unlock()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, read)
	}))
	t.Cleanup(srv.Close)
	out.url = srv.URL
	return out
}

// ─── orchestrator wiring ───

func TestLinkedIn_LeversAreReachableThroughTheOrchestrator(t *testing.T) {
	d, _ := liLeverDispatcher(t, nil)
	o := service.NewOrchestrator(nil, nil, map[model.Provider]service.PlatformDispatcher{model.ProviderLinkedInAds: d})
	row := liRow()
	row.Result = []byte(`{}`)
	if err := o.WriteCampaignBid(context.Background(), "proj", model.ProviderLinkedInAds, row, model.BidChange{Amount: 5, Type: model.BidTypeCPC}); errors.Is(err, service.ErrBidUnsupported) {
		t.Fatalf("LinkedIn is still bid-unsupported: %v", err)
	}
}
