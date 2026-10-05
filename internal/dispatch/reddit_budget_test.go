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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is optional and reached by TYPE ASSERTION in the orchestrator, so a signature
// drift would silently downgrade Reddit to "budget writes unsupported". This makes it a compile
// error instead.
var _ service.BudgetWriter = (*RedditDispatcher)(nil)

const (
	redditBudgetCampaignPath = "/api/v3/ad_accounts/t2_acct/campaigns/t3_c"
	// redditLifetimeCampaign is the shape CreateCampaign produces: CBO on, LIFETIME_SPEND, the
	// amount in micro-units on the campaign.
	redditLifetimeCampaign = `{"data":{"id":"t3_c","ad_account_id":"t2_acct","is_campaign_budget_optimization":true,"goal_type":"LIFETIME_SPEND","goal_value":100000000}}`
)

// redditBudgetStub wires a RedditDispatcher against a fake Ads API. The GET of the campaign is
// answered with getStatus/getBody, every PATCH with patchStatus/patchBody. Every request is
// recorded with its body, because what was written — and where — is the whole subject. The
// handlers never call t.Fatal (it must not run off the test goroutine).
type redditBudgetStub struct {
	d      *RedditDispatcher
	mu     sync.Mutex
	seen   []budgetRequest
	tokens atomic.Int32
}

func newRedditBudgetStub(t *testing.T, getStatus int, getBody string, patchStatus int, patchBody string) *redditBudgetStub {
	t.Helper()
	s := &redditBudgetStub{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			w.WriteHeader(patchStatus)
			_, _ = io.WriteString(w, patchBody)
			return
		}
		w.WriteHeader(getStatus)
		_, _ = io.WriteString(w, getBody)
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

func (s *redditBudgetStub) requests() []budgetRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]budgetRequest(nil), s.seen...)
}

func (s *redditBudgetStub) patches() []budgetRequest {
	var out []budgetRequest
	for _, r := range s.requests() {
		if r.Method == http.MethodPatch {
			out = append(out, r)
		}
	}
	return out
}

func (s *redditBudgetStub) assertNoPatch(t *testing.T) {
	t.Helper()
	if p := s.patches(); len(p) != 0 {
		t.Fatalf("refusal still issued %d PATCH(es): %+v — the caller is told nothing changed while the platform was written", len(p), p)
	}
}

// redditBudgetCampaign is a row recording provenance matching activeRedditConn (t2_acct).
func redditBudgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderRedditAds, PlatformCampaignID: "t3_c",
		Result: json.RawMessage(`{"accountId":"t2_acct","adGroupId":"t5_ag","adId":"t6_ad"}`),
	}
}

func writeRedditBudget(s *redditBudgetStub, c *model.Campaign, amount float64, typ model.BudgetType) error {
	return s.d.WriteBudget(context.Background(), "proj", model.ProviderRedditAds, c, model.BudgetChange{Amount: amount, Type: typ})
}

func TestReddit_WriteBudget_LifetimePatchesGoalValueOnTheCampaign(t *testing.T) {
	s := newRedditBudgetStub(t, http.StatusOK, redditLifetimeCampaign, http.StatusOK,
		`{"data":{"id":"t3_c","goal_type":"LIFETIME_SPEND","goal_value":250000000}}`)

	if err := writeRedditBudget(s, redditBudgetCampaign(), 250, model.BudgetLifetime); err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Path != redditBudgetCampaignPath {
		t.Fatalf("want a GET of %s then one PATCH, got %+v", redditBudgetCampaignPath, reqs)
	}
	p := reqs[1]
	// The CAMPAIGN, not an ad group: the create path puts the budget on the campaign (CBO on).
	if p.Method != http.MethodPatch || p.Path != redditBudgetCampaignPath {
		t.Errorf("wrote %s %s, want PATCH %s", p.Method, p.Path, redditBudgetCampaignPath)
	}
	// Exactly goal_value, in micro-units — no goal_type (pacing unchanged), no schedule.
	if p.Body != `{"data":{"goal_value":250000000}}` {
		t.Errorf("PATCH body = %s, want exactly {\"data\":{\"goal_value\":250000000}}", p.Body)
	}
}

func TestReddit_WriteBudget_RoundsToTheNearestMicro(t *testing.T) {
	s := newRedditBudgetStub(t, http.StatusOK, redditLifetimeCampaign, http.StatusOK, `{"data":{"id":"t3_c"}}`)

	if err := writeRedditBudget(s, redditBudgetCampaign(), 99.9999996, model.BudgetLifetime); err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	p := s.patches()
	if len(p) != 1 || p[0].Body != `{"data":{"goal_value":100000000}}` {
		t.Fatalf("99.9999996 must be sent as 100000000 micro-units (rounded, not truncated), got %+v", p)
	}
}

func TestReddit_WriteBudget_DailyCampaignTakesADailyAmount(t *testing.T) {
	daily := `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"goal_type":"DAILY_SPEND","goal_value":5000000}}`
	s := newRedditBudgetStub(t, http.StatusOK, daily, http.StatusOK, `{"data":{"id":"t3_c","goal_value":20000000}}`)

	if err := writeRedditBudget(s, redditBudgetCampaign(), 20, model.BudgetDaily); err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	if p := s.patches(); len(p) != 1 || p[0].Body != `{"data":{"goal_value":20000000}}` {
		t.Fatalf("want one goal_value write of 20000000, got %+v", p)
	}
}

// The requested pacing must already be the campaign's: a mismatch is refused (409), never
// translated by rewriting goal_type, and before any PATCH.
func TestReddit_WriteBudget_PacingMismatchRefusedWithoutMutate(t *testing.T) {
	daily := `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"goal_type":"DAILY_SPEND","goal_value":5000000}}`
	for _, tc := range []struct {
		name, get string
		typ       model.BudgetType
	}{
		{"daily requested on a lifetime campaign", redditLifetimeCampaign, model.BudgetDaily},
		{"lifetime requested on a daily campaign", daily, model.BudgetLifetime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBudgetStub(t, http.StatusOK, tc.get, http.StatusOK, `{"data":{}}`)
			err := writeRedditBudget(s, redditBudgetCampaign(), 50, tc.typ)
			if !errors.Is(err, domain.ErrBudgetUnwritable) {
				t.Fatalf("want ErrBudgetUnwritable, got %v", err)
			}
			if !strings.Contains(err.Error(), "never its pacing model") {
				t.Errorf("the refusal must say the pacing is not changed here: %v", err)
			}
			s.assertNoPatch(t)
		})
	}
}

// A budget that is not on the campaign is refused rather than allocated across ad groups, and a
// shape this service cannot read is refused rather than guessed.
func TestReddit_WriteBudget_UnaddressableBudgetShapesRefusedWithoutMutate(t *testing.T) {
	for _, tc := range []struct{ name, get string }{
		{"CBO off: budget lives on the ad groups", `{"data":{"id":"t3_c","is_campaign_budget_optimization":false,"goal_type":"LIFETIME_SPEND","goal_value":1}}`},
		{"CBO not reported", `{"data":{"id":"t3_c","goal_type":"LIFETIME_SPEND","goal_value":1}}`},
		{"goal_type not reported", `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"goal_value":1}}`},
		{"unmapped goal_type", `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"goal_type":"WEEKLY_SPEND","goal_value":1}}`},
		{"fractional goal_value", `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"goal_type":"LIFETIME_SPEND","goal_value":1.5}}`},
		{"non-numeric goal_value", `{"data":{"id":"t3_c","is_campaign_budget_optimization":true,"goal_type":"LIFETIME_SPEND","goal_value":"lots"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBudgetStub(t, http.StatusOK, tc.get, http.StatusOK, `{"data":{}}`)
			err := writeRedditBudget(s, redditBudgetCampaign(), 50, model.BudgetLifetime)
			if !errors.Is(err, domain.ErrBudgetUnwritable) {
				t.Fatalf("want ErrBudgetUnwritable, got %v", err)
			}
			s.assertNoPatch(t)
		})
	}
}

// Provenance is refused BEFORE any call — not even a token is fetched.
func TestReddit_WriteBudget_ProvenanceRefusedBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  string
		want    error
		unknown bool
	}{
		{"no creating account recorded", `{"adGroupId":"t5_ag"}`, domain.ErrCampaignProvenanceUnknown, true},
		{"created under another account", `{"accountId":"t2_other"}`, domain.ErrCampaignAccountMismatch, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBudgetStub(t, http.StatusOK, redditLifetimeCampaign, http.StatusOK, `{"data":{}}`)
			c := redditBudgetCampaign()
			c.Result = json.RawMessage(tc.result)
			err := writeRedditBudget(s, c, 50, model.BudgetLifetime)
			if !errors.Is(err, tc.want) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Fatalf("want %v (and ErrCampaignAccountMismatch), got %v", tc.want, err)
			}
			if errors.Is(err, domain.ErrCampaignProvenanceUnknown) != tc.unknown {
				t.Errorf("ErrCampaignProvenanceUnknown presence = %v, want %v: %v", !tc.unknown, tc.unknown, err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("a provenance refusal must reach no Ads API endpoint, got %d request(s)", n)
			}
			if n := s.tokens.Load(); n != 0 {
				t.Errorf("a provenance refusal must fetch no token, got %d", n)
			}
		})
	}
}

// When Reddit's answer names the account the campaign sits under, it is checked.
func TestReddit_WriteBudget_ReportedForeignAccountRefusedWithoutMutate(t *testing.T) {
	foreign := `{"data":{"id":"t3_c","ad_account_id":"t2_other","is_campaign_budget_optimization":true,"goal_type":"LIFETIME_SPEND","goal_value":1}}`
	s := newRedditBudgetStub(t, http.StatusOK, foreign, http.StatusOK, `{"data":{}}`)
	err := writeRedditBudget(s, redditBudgetCampaign(), 50, model.BudgetLifetime)
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("want ErrCampaignAccountMismatch, got %v", err)
	}
	s.assertNoPatch(t)
}

// A read answering about a different campaign is not a budget this caller may act on.
func TestReddit_WriteBudget_ReadEchoingAnotherCampaignRefusedWithoutMutate(t *testing.T) {
	other := `{"data":{"id":"t3_other","is_campaign_budget_optimization":true,"goal_type":"LIFETIME_SPEND","goal_value":1}}`
	s := newRedditBudgetStub(t, http.StatusOK, other, http.StatusOK, `{"data":{}}`)
	err := writeRedditBudget(s, redditBudgetCampaign(), 50, model.BudgetLifetime)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		t.Errorf("a read-side failure built no mutate and must not be Unconfirmed: %v", err)
	}
	s.assertNoPatch(t)
}

func TestReddit_WriteBudget_CampaignAbsentIs404(t *testing.T) {
	s := newRedditBudgetStub(t, http.StatusNotFound, `{"error":"not found"}`, http.StatusOK, `{"data":{}}`)
	err := writeRedditBudget(s, redditBudgetCampaign(), 50, model.BudgetLifetime)
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("want ErrPlatformCampaignAbsent, got %v", err)
	}
	s.assertNoPatch(t)
}

// A 5xx on the READ is definite: nothing was built to be ambiguous about.
func TestReddit_WriteBudget_ReadFailureIsDefinite(t *testing.T) {
	s := newRedditBudgetStub(t, http.StatusInternalServerError, `{}`, http.StatusOK, `{"data":{}}`)
	err := writeRedditBudget(s, redditBudgetCampaign(), 50, model.BudgetLifetime)
	if err == nil {
		t.Fatal("expected an error")
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		t.Errorf("a failed read must not be reported as an unconfirmed write: %v", err)
	}
	s.assertNoPatch(t)
}

// An amount Reddit's micro-unit encoding cannot carry is a 400 with the adapter's own reason,
// refused before any call.
func TestReddit_WriteBudget_AmountRejectedBeforeAnyCall(t *testing.T) {
	s := newRedditBudgetStub(t, http.StatusOK, redditLifetimeCampaign, http.StatusOK, `{"data":{}}`)
	err := writeRedditBudget(s, redditBudgetCampaign(), 2_000_000_000, model.BudgetLifetime)
	if !errors.Is(err, domain.ErrBudgetAmountRejected) {
		t.Fatalf("want ErrBudgetAmountRejected, got %v", err)
	}
	var r interface{ BudgetAmountReason() string }
	if !errors.As(err, &r) || r.BudgetAmountReason() == "" {
		t.Errorf("the refusal must carry the adapter's client-safe reason: %T %v", err, err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("an amount refusal must reach no endpoint, got %d request(s)", n)
	}
}

// The ambiguity contract on the mutate: a definite 4xx is a refusal; a 5xx, or a 2xx whose echo
// names another campaign or amount, MAY have applied and is UNCONFIRMED.
func TestReddit_WriteBudget_MutateOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
	}{
		{"definite 400 rejection", http.StatusBadRequest, `{"error":"bad goal_value"}`, false},
		{"definite 403 rejection", http.StatusForbidden, `{}`, false},
		{"5xx is ambiguous", http.StatusBadGateway, `{}`, true},
		{"2xx echoing another campaign", http.StatusOK, `{"data":{"id":"t3_other","goal_value":50000000}}`, true},
		{"2xx echoing another amount", http.StatusOK, `{"data":{"id":"t3_c","goal_value":1}}`, true},
		{"2xx whose data is not an object", http.StatusOK, `{"data":[1,2]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditBudgetStub(t, http.StatusOK, redditLifetimeCampaign, tc.status, tc.body)
			err := writeRedditBudget(s, redditBudgetCampaign(), 50, model.BudgetLifetime)
			if err == nil {
				t.Fatal("expected an error")
			}
			var u interface{ Unconfirmed() bool }
			got := errors.As(err, &u) && u.Unconfirmed()
			if got != tc.unconfirmed {
				t.Errorf("Unconfirmed() = %v, want %v: %v", got, tc.unconfirmed, err)
			}
			if errors.Is(err, domain.ErrBudgetUnwritable) || errors.Is(err, domain.ErrBudgetAmountRejected) {
				t.Errorf("a mutate outcome must not be classified as a pre-mutate refusal: %v", err)
			}
			if p := s.patches(); len(p) != 1 {
				t.Errorf("want exactly one PATCH (a 5xx PATCH is not retried), got %d", len(p))
			}
		})
	}
}

// An inactive connection is refused by the shared resolution with the connection sentinel, before
// any Ads API call — the same answer the toggle gives.
func TestReddit_WriteBudget_UnusableConnectionRefusedBeforeAnyCall(t *testing.T) {
	conn := activeRedditConn(goodRedditCreds)
	conn.Status = model.StatusInactive
	d := NewRedditDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		reddit.WithBaseURL("http://127.0.0.1:0/api/v3"), reddit.WithTokenURL("http://127.0.0.1:0/token"))
	err := d.WriteBudget(context.Background(), "proj", model.ProviderRedditAds, redditBudgetCampaign(),
		model.BudgetChange{Amount: 50, Type: model.BudgetLifetime})
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("want ErrConnectionNotUsable, got %v", err)
	}
}
