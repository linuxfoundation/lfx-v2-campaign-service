// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type budgetCall struct {
	Method, Path, RawQuery string
}

// budgetServer answers the campaign GET with getStatus/getBody and the n-th PUT with
// putReplies[n] (the last reply repeats). Every request is recorded.
type budgetServer struct {
	mu    sync.Mutex
	calls []budgetCall
	puts  int
}

type budgetReply struct {
	status int
	body   string
}

func newBudgetServer(t *testing.T, getStatus int, getBody string, putReplies ...budgetReply) (*budgetServer, *Client) {
	t.Helper()
	s := &budgetServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.calls = append(s.calls, budgetCall{r.Method, r.URL.Path, r.URL.RawQuery})
		n := s.puts
		if r.Method == http.MethodPut {
			s.puts++
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			rep := putReplies[min(n, len(putReplies)-1)]
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
	t.Cleanup(srv.Close)
	return s, newToggleTestClient(t, srv.URL)
}

func (s *budgetServer) recorded() []budgetCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]budgetCall(nil), s.calls...)
}

const xCampaignBody = `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":null,"currency":"USD","deleted":false}}`

func TestGetCampaignBudget_ReadsTheCampaignAtItsAccountScopedPath(t *testing.T) {
	s, c := newBudgetServer(t, http.StatusOK, xCampaignBody, budgetReply{http.StatusOK, `{}`})
	got, err := c.GetCampaignBudget(context.Background(), "cmp1")
	if err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	if got.BudgetOptimization != BudgetOptimizationCampaign || got.DailyMicros == nil || *got.DailyMicros != 50000000 || got.TotalMicros != nil {
		t.Fatalf("unexpected budget %+v", got)
	}
	calls := s.recorded()
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/12/accounts/acc1/campaigns/cmp1" {
		t.Fatalf("want one GET /12/accounts/acc1/campaigns/cmp1, got %+v", calls)
	}
}

func TestGetCampaignBudget_AbsentShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"404", http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND"}]}`},
		{"deleted", http.StatusOK, `{"data":{"id":"cmp1","deleted":true,"budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newBudgetServer(t, tc.status, tc.body, budgetReply{http.StatusOK, `{}`})
			got, err := c.GetCampaignBudget(context.Background(), "cmp1")
			if err != nil || got != nil {
				t.Fatalf("want (nil, nil), got (%+v, %v)", got, err)
			}
		})
	}
}

func TestGetCampaignBudget_UnparseableAmountsFlagged(t *testing.T) {
	for _, v := range []string{`1.5`, `"lots"`, `-1`, `"5"`} {
		_, c := newBudgetServer(t, http.StatusOK, `{"data":{"id":"cmp1","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":`+v+`}}`, budgetReply{http.StatusOK, `{}`})
		got, err := c.GetCampaignBudget(context.Background(), "cmp1")
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if !got.DailyUnparseable || got.DailyMicros != nil {
			t.Errorf("%s: want DailyUnparseable with no value, got %+v", v, got)
		}
	}
}

func TestGetCampaignBudget_ReadOfAnotherCampaignIsAnError(t *testing.T) {
	_, c := newBudgetServer(t, http.StatusOK, `{"data":{"id":"cmp2","budget_optimization":"CAMPAIGN"}}`, budgetReply{http.StatusOK, `{}`})
	if _, err := c.GetCampaignBudget(context.Background(), "cmp1"); err == nil {
		t.Fatal("a read answering about another campaign must be an error")
	}
}

func TestCampaignBudget_InvalidIDsRefusedBeforeAnyRequest(t *testing.T) {
	s, c := newBudgetServer(t, http.StatusOK, xCampaignBody, budgetReply{http.StatusOK, `{}`})
	if _, err := c.GetCampaignBudget(context.Background(), "cmp1/../x"); !errors.Is(err, ErrInvalidCampaignID) {
		t.Errorf("want ErrInvalidCampaignID, got %v", err)
	}
	if err := c.UpdateCampaignBudget(context.Background(), "cmp1?x=1", BudgetFieldDaily, 1); !errors.Is(err, ErrInvalidCampaignID) {
		t.Errorf("want ErrInvalidCampaignID, got %v", err)
	}
	c.account.AccountID = "acc/1"
	if _, err := c.GetCampaignBudget(context.Background(), "cmp1"); !errors.Is(err, ErrInvalidAccountID) {
		t.Errorf("want ErrInvalidAccountID, got %v", err)
	}
	if n := len(s.recorded()); n != 0 {
		t.Errorf("an invalid id must reach no endpoint, got %d request(s)", n)
	}
}

// The PUT carries exactly the one field, in the query string (X v12 write contract), and nothing
// that would change the budget model, status or the other cap.
func TestUpdateCampaignBudget_PutsExactlyTheOneField(t *testing.T) {
	for _, field := range []BudgetField{BudgetFieldDaily, BudgetFieldTotal} {
		t.Run(string(field), func(t *testing.T) {
			s, c := newBudgetServer(t, http.StatusOK, xCampaignBody,
				budgetReply{http.StatusOK, `{"data":{"id":"cmp1","` + string(field) + `":75000000}}`})
			if err := c.UpdateCampaignBudget(context.Background(), "cmp1", field, 75000000); err != nil {
				t.Fatalf("UpdateCampaignBudget: %v", err)
			}
			calls := s.recorded()
			if len(calls) != 1 || calls[0].Method != http.MethodPut || calls[0].Path != "/12/accounts/acc1/campaigns/cmp1" {
				t.Fatalf("want one PUT /12/accounts/acc1/campaigns/cmp1, got %+v", calls)
			}
			if calls[0].RawQuery != string(field)+"=75000000" {
				t.Errorf("query = %q, want exactly %s=75000000", calls[0].RawQuery, field)
			}
		})
	}
}

func TestUpdateCampaignBudget_RefusesAnUnknownFieldAndANonPositiveAmount(t *testing.T) {
	s, c := newBudgetServer(t, http.StatusOK, xCampaignBody, budgetReply{http.StatusOK, `{}`})
	if err := c.UpdateCampaignBudget(context.Background(), "cmp1", BudgetField("entity_status"), 1); err == nil {
		t.Error("an unknown field must be refused")
	}
	err := c.UpdateCampaignBudget(context.Background(), "cmp1", BudgetFieldDaily, 0)
	if !errors.Is(err, ErrBudgetAmountInvalid) {
		t.Errorf("want ErrBudgetAmountInvalid, got %v", err)
	}
	if n := len(s.recorded()); n != 0 {
		t.Errorf("a refused write must reach no endpoint, got %d request(s)", n)
	}
}

// The outcome table. Definite 4xx refusals are not Unconfirmed; everything that may have applied
// is — including a refusal that followed a retried 429 (the Microsoft PR #255 lesson).
func TestUpdateCampaignBudget_OutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replies     []budgetReply
		wantErr     bool
		unconfirmed bool
		wantPuts    int
	}{
		{"2xx confirmed", []budgetReply{{http.StatusOK, `{"data":{"id":"cmp1","daily_budget_amount_local_micro":75000000}}`}}, false, false, 1},
		{"2xx without echo confirmed", []budgetReply{{http.StatusOK, `{}`}}, false, false, 1},
		{"400 definite", []budgetReply{{http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`}}, true, false, 1},
		{"404 definite", []budgetReply{{http.StatusNotFound, `{}`}}, true, false, 1},
		{"5xx unconfirmed", []budgetReply{{http.StatusBadGateway, `{}`}}, true, true, 1},
		{"3xx unconfirmed", []budgetReply{{http.StatusFound, ``}}, true, true, 1},
		{"429 then 400 unconfirmed", []budgetReply{{http.StatusTooManyRequests, ``}, {http.StatusBadRequest, `{}`}}, true, true, 2},
		{"429 then 2xx confirmed", []budgetReply{{http.StatusTooManyRequests, ``}, {http.StatusOK, `{}`}}, false, false, 2},
		{"2xx echoing another campaign", []budgetReply{{http.StatusOK, `{"data":{"id":"cmp2"}}`}}, true, true, 1},
		{"2xx echoing another amount", []budgetReply{{http.StatusOK, `{"data":{"id":"cmp1","daily_budget_amount_local_micro":1}}`}}, true, true, 1},
		{"2xx whose data is not an object", []budgetReply{{http.StatusOK, `{"data":[1]}`}}, true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newBudgetServer(t, http.StatusOK, xCampaignBody, tc.replies...)
			err := c.UpdateCampaignBudget(context.Background(), "cmp1", BudgetFieldDaily, 75000000)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v: %v", got, tc.unconfirmed, err)
			}
			if got := len(s.recorded()); got != tc.wantPuts {
				t.Errorf("PUTs = %d, want %d", got, tc.wantPuts)
			}
		})
	}
}

func TestUpdateCampaignBudget_ExhaustedThrottleIsUnconfirmed(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps through retryMax Retry-After waits")
	}
	_, c := newBudgetServer(t, http.StatusOK, xCampaignBody, budgetReply{http.StatusTooManyRequests, ``})
	err := c.UpdateCampaignBudget(context.Background(), "cmp1", BudgetFieldDaily, 75000000)
	if err == nil || !IsOutcomeUnconfirmed(err) {
		t.Fatalf("an exhausted 429 on a PUT must be unconfirmed, got %v", err)
	}
}

// A timeout after the request was sent is a transportError: UNCONFIRMED.
func TestUpdateCampaignBudget_TimeoutIsUnconfirmed(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := newToggleTestClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.UpdateCampaignBudget(ctx, "cmp1", BudgetFieldDaily, 75000000)
	if err == nil || !IsOutcomeUnconfirmed(err) {
		t.Fatalf("a timed-out PUT must be unconfirmed, got %v", err)
	}
}

func TestBudgetMicros_SharesTheCreatePathBounds(t *testing.T) {
	if v, err := BudgetMicros(99.9999996); err != nil || v != 100000000 {
		t.Errorf("BudgetMicros(99.9999996) = %d, %v; want 100000000 (rounded)", v, err)
	}
	for _, bad := range []float64{0, -1, maxBudgetUsd * 2, 0.0000001} {
		_, err := BudgetMicros(bad)
		if !errors.Is(err, ErrBudgetAmountInvalid) {
			t.Errorf("BudgetMicros(%v): want ErrBudgetAmountInvalid, got %v", bad, err)
			continue
		}
		reason, ok := BudgetAmountReason(err)
		if !ok || reason == "" || strings.Contains(reason, "e+") {
			t.Errorf("BudgetMicros(%v): reason %q must be a plain client-safe sentence", bad, reason)
		}
	}
}
