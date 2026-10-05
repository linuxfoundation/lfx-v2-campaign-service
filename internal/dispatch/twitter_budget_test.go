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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is optional and reached by TYPE ASSERTION in the orchestrator, so a signature
// drift would silently downgrade X to "budget writes unsupported". This makes it a compile error.
var _ service.BudgetWriter = (*TwitterDispatcher)(nil)

const (
	twitterBudgetCampaignPath = "/12/accounts/acc1/campaigns/cmp1"
	// twitterDailyCampaign is the shape CreateCampaign is INFERRED to produce (unverified live):
	// campaign budget optimization, a daily amount on the campaign, no total.
	twitterDailyCampaign = `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":null,"currency":"USD"}}`
	// twitterTotalOnlyCampaign contradicts X's CAMPAIGN contract (daily required) and is refused.
	twitterTotalOnlyCampaign = `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":null,"total_budget_amount_local_micro":500000000}}`
)

type twitterBudgetCall struct {
	Method, Path, RawQuery string
}

// twitterBudgetStub wires a TwitterDispatcher against a fake X Ads API. The campaign GET is
// answered with getStatus/getBody; the n-th PUT with puts[n] (the last repeats). The handlers
// never call t.Fatal.
type twitterBudgetStub struct {
	d     *TwitterDispatcher
	mu    sync.Mutex
	seen  []twitterBudgetCall
	nPuts int
}

type twitterReply struct {
	status int
	body   string
}

func newTwitterBudgetStub(t *testing.T, getStatus int, getBody string, puts ...twitterReply) *twitterBudgetStub {
	t.Helper()
	if len(puts) == 0 {
		puts = []twitterReply{{http.StatusOK, `{}`}}
	}
	s := &twitterBudgetStub{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, twitterBudgetCall{r.Method, r.URL.Path, r.URL.RawQuery})
		n := s.nPuts
		if r.Method == http.MethodPut {
			s.nPuts++
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			rep := puts[min(n, len(puts)-1)]
			if rep.status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "1")
			}
			w.WriteHeader(rep.status)
			_, _ = io.WriteString(w, rep.body)
			return
		}
		w.WriteHeader(getStatus)
		_, _ = io.WriteString(w, getBody)
	}))
	t.Cleanup(api.Close)
	s.d = NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0))
	return s
}

func (s *twitterBudgetStub) requests() []twitterBudgetCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]twitterBudgetCall(nil), s.seen...)
}

func (s *twitterBudgetStub) puts() []twitterBudgetCall {
	var out []twitterBudgetCall
	for _, r := range s.requests() {
		if r.Method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

func (s *twitterBudgetStub) assertNoPut(t *testing.T) {
	t.Helper()
	if p := s.puts(); len(p) != 0 {
		t.Fatalf("refusal still issued %d PUT(s): %+v — the caller is told nothing changed while the platform was written", len(p), p)
	}
}

// twitterBudgetCampaign is a row recording provenance matching activeTwitterConn (acc1).
func twitterBudgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderTwitterAds, PlatformCampaignID: "cmp1",
		Result: json.RawMessage(`{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc1"}`),
	}
}

func writeTwitterBudget(s *twitterBudgetStub, c *model.Campaign, amount float64, typ model.BudgetType) error {
	return s.d.WriteBudget(context.Background(), "proj", model.ProviderTwitterAds, c, model.BudgetChange{Amount: amount, Type: typ})
}

func TestTwitter_WriteBudget_DailyPutsTheDailyAmountOnTheCampaign(t *testing.T) {
	s := newTwitterBudgetStub(t, http.StatusOK, twitterDailyCampaign,
		twitterReply{http.StatusOK, `{"data":{"id":"cmp1","daily_budget_amount_local_micro":75000000}}`})

	if err := writeTwitterBudget(s, twitterBudgetCampaign(), 75, model.BudgetDaily); err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Path != twitterBudgetCampaignPath {
		t.Fatalf("want a GET of %s then one PUT, got %+v", twitterBudgetCampaignPath, reqs)
	}
	p := reqs[1]
	// The CAMPAIGN, not a line item: the create path puts the budget on the campaign.
	if p.Method != http.MethodPut || p.Path != twitterBudgetCampaignPath {
		t.Errorf("wrote %s %s, want PUT %s", p.Method, p.Path, twitterBudgetCampaignPath)
	}
	// Exactly the daily amount in micros — no budget_optimization, no status, no total.
	if p.RawQuery != "daily_budget_amount_local_micro=75000000" {
		t.Errorf("PUT query = %q, want exactly daily_budget_amount_local_micro=75000000", p.RawQuery)
	}
}

func TestTwitter_WriteBudget_RoundsToTheNearestMicro(t *testing.T) {
	s := newTwitterBudgetStub(t, http.StatusOK, twitterDailyCampaign)
	if err := writeTwitterBudget(s, twitterBudgetCampaign(), 99.9999996, model.BudgetDaily); err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	if p := s.puts(); len(p) != 1 || p[0].RawQuery != "daily_budget_amount_local_micro=100000000" {
		t.Fatalf("99.9999996 must be sent as 100000000 micro-units, got %+v", p)
	}
}

// X budgets are written as a daily amount only: a lifetime request is refused (409) before any
// X Ads endpoint is reached — whatever the campaign holds — and says why.
func TestTwitter_WriteBudget_LifetimeRefusedBeforeAnyCall(t *testing.T) {
	for _, get := range []string{twitterDailyCampaign, twitterTotalOnlyCampaign} {
		s := newTwitterBudgetStub(t, http.StatusOK, get)
		err := writeTwitterBudget(s, twitterBudgetCampaign(), 50, model.BudgetLifetime)
		if !errors.Is(err, domain.ErrBudgetUnwritable) {
			t.Fatalf("want ErrBudgetUnwritable, got %v", err)
		}
		if !strings.Contains(err.Error(), "only as a daily amount") {
			t.Errorf("the refusal must say X budgets are daily-only here: %v", err)
		}
		if n := len(s.requests()); n != 0 {
			t.Errorf("a lifetime refusal must reach no endpoint, got %d request(s)", n)
		}
	}
}

// A budget shape other than the daily-only CAMPAIGN one — line-item optimization, an unreported or
// unknown model, a total cap alone (which contradicts X's CAMPAIGN contract), both caps, neither
// cap, or an unreadable amount — is refused rather than guessed.
func TestTwitter_WriteBudget_UnsupportedBudgetModelsRefusedWithoutMutate(t *testing.T) {
	for _, tc := range []struct{ name, get string }{
		{"line-item budget optimization", `{"data":{"id":"cmp1","budget_optimization":"LINE_ITEM","daily_budget_amount_local_micro":null}}`},
		{"budget_optimization not reported", `{"data":{"id":"cmp1","daily_budget_amount_local_micro":50000000}}`},
		{"unknown budget_optimization", `{"data":{"id":"cmp1","budget_optimization":"ACCOUNT","daily_budget_amount_local_micro":50000000}}`},
		{"total cap only", twitterTotalOnlyCampaign},
		{"both caps", `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":500000000}}`},
		{"neither cap", `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN"}}`},
		{"fractional daily", `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":1.5}}`},
		{"non-numeric total", `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":1,"total_budget_amount_local_micro":"lots"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwitterBudgetStub(t, http.StatusOK, tc.get)
			err := writeTwitterBudget(s, twitterBudgetCampaign(), 50, model.BudgetDaily)
			if !errors.Is(err, domain.ErrBudgetUnwritable) {
				t.Fatalf("want ErrBudgetUnwritable, got %v", err)
			}
			s.assertNoPut(t)
		})
	}
}

// Provenance is refused BEFORE any call.
func TestTwitter_WriteBudget_ProvenanceAndAccountMismatchRefusedBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  string
		want    error
		unknown bool
	}{
		{"no creating account recorded", `{"CampaignID":"cmp1","LineItemID":"li1"}`, domain.ErrCampaignProvenanceUnknown, true},
		{"no result blob at all", ``, domain.ErrCampaignProvenanceUnknown, true},
		{"created under another account", `{"CampaignID":"cmp1","AccountID":"acc_other"}`, domain.ErrCampaignAccountMismatch, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwitterBudgetStub(t, http.StatusOK, twitterDailyCampaign)
			c := twitterBudgetCampaign()
			c.Result = json.RawMessage(tc.result)
			err := writeTwitterBudget(s, c, 50, model.BudgetDaily)
			if !errors.Is(err, tc.want) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Fatalf("want %v (and ErrCampaignAccountMismatch), got %v", tc.want, err)
			}
			if errors.Is(err, domain.ErrCampaignProvenanceUnknown) != tc.unknown {
				t.Errorf("ErrCampaignProvenanceUnknown presence = %v, want %v: %v", !tc.unknown, tc.unknown, err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("a provenance refusal must reach no X Ads endpoint, got %d request(s)", n)
			}
		})
	}
}

func TestTwitter_WriteBudget_CampaignAbsentIs404(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"404", http.StatusNotFound, `{}`},
		{"deleted", http.StatusOK, `{"data":{"id":"cmp1","deleted":true,"budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwitterBudgetStub(t, tc.status, tc.body)
			err := writeTwitterBudget(s, twitterBudgetCampaign(), 50, model.BudgetDaily)
			if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
				t.Fatalf("want ErrPlatformCampaignAbsent, got %v", err)
			}
			s.assertNoPut(t)
		})
	}
}

// A failure of the READ is definite — nothing was built to be ambiguous about — and so is a read
// that answers about another campaign.
func TestTwitter_WriteBudget_ReadFailuresAreDefinite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx read", http.StatusInternalServerError, `{}`},
		{"read of another campaign", http.StatusOK, `{"data":{"id":"cmp2","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwitterBudgetStub(t, tc.status, tc.body)
			err := writeTwitterBudget(s, twitterBudgetCampaign(), 50, model.BudgetDaily)
			if err == nil {
				t.Fatal("expected an error")
			}
			var u interface{ Unconfirmed() bool }
			if errors.As(err, &u) && u.Unconfirmed() {
				t.Errorf("a read-side failure built no mutate and must not be Unconfirmed: %v", err)
			}
			s.assertNoPut(t)
		})
	}
}

// Amount bounds: the create path's own, refused with a client-safe reason before any call.
func TestTwitter_WriteBudget_AmountBoundsRejectedBeforeAnyCall(t *testing.T) {
	for _, amount := range []float64{2_000_000_000, 0.0000001, 0, -5} {
		s := newTwitterBudgetStub(t, http.StatusOK, twitterDailyCampaign)
		err := writeTwitterBudget(s, twitterBudgetCampaign(), amount, model.BudgetDaily)
		if !errors.Is(err, domain.ErrBudgetAmountRejected) {
			t.Fatalf("%v: want ErrBudgetAmountRejected, got %v", amount, err)
		}
		var r interface{ BudgetAmountReason() string }
		if !errors.As(err, &r) || r.BudgetAmountReason() == "" {
			t.Errorf("%v: the refusal must carry the adapter's client-safe reason: %T %v", amount, err, err)
		}
		if n := len(s.requests()); n != 0 {
			t.Errorf("%v: an amount refusal must reach no endpoint, got %d request(s)", amount, n)
		}
	}
}

// The ambiguity contract on the mutate: a definite 4xx is a refusal; anything that may have
// applied — 5xx, 3xx, a 429 followed by a refusal, a 2xx echoing another campaign or amount — is
// UNCONFIRMED and never a pre-mutate sentinel.
func TestTwitter_WriteBudget_MutateOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replies     []twitterReply
		unconfirmed bool
		wantPuts    int
	}{
		{"definite 400 rejection", []twitterReply{{http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`}}, false, 1},
		{"definite 404 rejection", []twitterReply{{http.StatusNotFound, `{}`}}, false, 1},
		{"5xx is ambiguous", []twitterReply{{http.StatusBadGateway, `{}`}}, true, 1},
		{"3xx is ambiguous", []twitterReply{{http.StatusFound, ``}}, true, 1},
		{"429 then 400 is ambiguous", []twitterReply{{http.StatusTooManyRequests, ``}, {http.StatusBadRequest, `{}`}}, true, 2},
		{"2xx echoing another campaign", []twitterReply{{http.StatusOK, `{"data":{"id":"cmp2","daily_budget_amount_local_micro":50000000}}`}}, true, 1},
		{"2xx echoing another amount", []twitterReply{{http.StatusOK, `{"data":{"id":"cmp1","daily_budget_amount_local_micro":1}}`}}, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTwitterBudgetStub(t, http.StatusOK, twitterDailyCampaign, tc.replies...)
			err := writeTwitterBudget(s, twitterBudgetCampaign(), 50, model.BudgetDaily)
			if err == nil {
				t.Fatal("expected an error")
			}
			var u interface{ Unconfirmed() bool }
			if got := errors.As(err, &u) && u.Unconfirmed(); got != tc.unconfirmed {
				t.Errorf("Unconfirmed() = %v, want %v: %v", got, tc.unconfirmed, err)
			}
			if errors.Is(err, domain.ErrBudgetUnwritable) || errors.Is(err, domain.ErrBudgetAmountRejected) || errors.Is(err, domain.ErrBudgetShared) {
				t.Errorf("a mutate outcome must not be classified as a pre-mutate refusal: %v", err)
			}
			if p := s.puts(); len(p) != tc.wantPuts {
				t.Errorf("want %d PUT(s), got %d", tc.wantPuts, len(p))
			}
		})
	}
}

// A timeout on the PUT, after it was sent, is UNCONFIRMED.
func TestTwitter_WriteBudget_PutTimeoutIsUnconfirmed(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			<-release
			return
		}
		_, _ = io.WriteString(w, twitterDailyCampaign)
	}))
	t.Cleanup(api.Close)
	t.Cleanup(func() { close(release) })
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
		twitter.WithHTTPClient(&http.Client{Timeout: 100 * time.Millisecond}))
	err := d.WriteBudget(context.Background(), "proj", model.ProviderTwitterAds, twitterBudgetCampaign(),
		model.BudgetChange{Amount: 50, Type: model.BudgetDaily})
	var u interface{ Unconfirmed() bool }
	if err == nil || !errors.As(err, &u) || !u.Unconfirmed() {
		t.Fatalf("a timed-out PUT must be unconfirmed, got %v", err)
	}
}

func TestTwitter_WriteBudget_UnusableConnectionRefusedBeforeAnyCall(t *testing.T) {
	conn := activeTwitterConn(goodTwitterCreds)
	conn.Status = model.StatusInactive
	d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, twitter.WithBaseURL("http://127.0.0.1:0"))
	err := d.WriteBudget(context.Background(), "proj", model.ProviderTwitterAds, twitterBudgetCampaign(),
		model.BudgetChange{Amount: 50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("want ErrConnectionNotUsable, got %v", err)
	}
}

// The two ids the client refuses before any request have different owners and remedies.
func TestTwitter_WriteBudget_InvalidIDsClassifiedByTheirOwner(t *testing.T) {
	t.Run("connection account id is a connection defect", func(t *testing.T) {
		conn := activeTwitterConn(goodTwitterCreds)
		conn.AccountID = "acc-bad"
		c := twitterBudgetCampaign()
		c.Result = json.RawMessage(`{"AccountID":"acc-bad"}`)
		d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, twitter.WithBaseURL("http://127.0.0.1:0"))
		err := d.WriteBudget(context.Background(), "proj", model.ProviderTwitterAds, c,
			model.BudgetChange{Amount: 50, Type: model.BudgetDaily})
		if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrProviderConfigInvalid) || !errors.Is(err, twitter.ErrInvalidAccountID) {
			t.Fatalf("want ErrConnectionNotUsable + ErrProviderConfigInvalid + ErrInvalidAccountID, got %v", err)
		}
		if errors.Is(err, domain.ErrBudgetUnwritable) {
			t.Errorf("a connection defect must not be reported as an unaddressable budget: %v", err)
		}
	})
	t.Run("row campaign id is a row defect", func(t *testing.T) {
		s := newTwitterBudgetStub(t, http.StatusOK, twitterDailyCampaign)
		c := twitterBudgetCampaign()
		c.PlatformCampaignID = "cmp/../x"
		err := writeTwitterBudget(s, c, 50, model.BudgetDaily)
		if !errors.Is(err, domain.ErrBudgetUnwritable) || !errors.Is(err, twitter.ErrInvalidCampaignID) {
			t.Fatalf("want ErrBudgetUnwritable wrapping ErrInvalidCampaignID, got %v", err)
		}
		if n := len(s.requests()); n != 0 {
			t.Errorf("an invalid id must reach no endpoint, got %d request(s)", n)
		}
	})
}
