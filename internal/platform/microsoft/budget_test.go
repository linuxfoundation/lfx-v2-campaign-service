// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// budgetRecorder records the method, path and body of every API request, so a test can assert
// what was SENT rather than only how many calls were made.
type budgetRecorder struct {
	mu   sync.Mutex
	reqs []recordedReq
}

type recordedReq struct{ method, path, body string }

func (b *budgetRecorder) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, recordedReq{r.Method, r.URL.Path, string(body)})
}

func (b *budgetRecorder) all() []recordedReq {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedReq(nil), b.reqs...)
}

func TestGetCampaignBudget_SendsQueryByIdsAndReadsShape(t *testing.T) {
	rec := &budgetRecorder{}
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"BudgetType":"DailyBudgetAccelerated","DailyBudget":50,"BudgetId":null}],"PartialErrors":[]}`)
	})
	got, err := c.GetCampaignBudget(context.Background(), "321")
	if err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	if got.CampaignID != "321" || got.IsShared() || got.BudgetIDUnreadable || got.BudgetType != BudgetTypeDailyAccelerated || got.ExperimentID != "" {
		t.Errorf("unexpected shape: %+v", got)
	}
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("want one request, got %+v", reqs)
	}
	if reqs[0].method != http.MethodPost || !strings.HasSuffix(reqs[0].path, "/CampaignManagement/v13/Campaigns/QueryByIds") {
		t.Errorf("want POST .../Campaigns/QueryByIds, got %s %s", reqs[0].method, reqs[0].path)
	}
	if want := `{"AccountId":1234567,"CampaignIds":[321],"CampaignType":"Search"}`; reqs[0].body != want {
		t.Errorf("read body = %s, want %s", reqs[0].body, want)
	}
}

func TestGetCampaignBudget_Shapes(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantNil    bool
		wantErr    bool
		wantShared string
		unreadable bool
		experiment string
		budgetType string
	}{
		{name: "own budget, BudgetId zero", body: `{"Campaigns":[{"Id":"321","BudgetId":"0","BudgetType":"DailyBudgetStandard"}],"PartialErrors":null}`, budgetType: BudgetTypeDailyStandard},
		{name: "shared budget", body: `{"Campaigns":[{"Id":321,"BudgetId":"8888","BudgetType":"DailyBudgetStandard"}],"PartialErrors":[]}`, wantShared: "8888", budgetType: BudgetTypeDailyStandard},
		{name: "unreadable BudgetId", body: `{"Campaigns":[{"Id":321,"BudgetId":-5,"BudgetType":"DailyBudgetStandard"}],"PartialErrors":[]}`, unreadable: true, budgetType: BudgetTypeDailyStandard},
		{name: "experiment campaign", body: `{"Campaigns":[{"Id":321,"ExperimentId":"77","BudgetType":"DailyBudgetStandard"}],"PartialErrors":[]}`, experiment: "77", budgetType: BudgetTypeDailyStandard},
		{name: "unreported budget type", body: `{"Campaigns":[{"Id":321}],"PartialErrors":[]}`},
		{name: "invalid campaign id partial error is absent", body: `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`, wantNil: true},
		{name: "other partial error is a failure", body: `{"Campaigns":[null],"PartialErrors":[{"Code":516,"ErrorCode":"EntityIdFilterMismatch","Index":0}]}`, wantErr: true},
		{name: "omitted Campaigns", body: `{"PartialErrors":[]}`, wantErr: true},
		{name: "unexplained null slot", body: `{"Campaigns":[null],"PartialErrors":[]}`, wantErr: true},
		{name: "a different campaign echoed", body: `{"Campaigns":[{"Id":999,"BudgetType":"DailyBudgetStandard"}],"PartialErrors":[]}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := c.GetCampaignBudget(context.Background(), "321")
			switch {
			case tc.wantErr:
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantNil:
				if got != nil {
					t.Fatalf("want (nil, nil) for an absent campaign, got %+v", got)
				}
				return
			case got == nil:
				t.Fatal("want a budget, got nil")
			}
			if got.SharedBudgetID != tc.wantShared || got.BudgetIDUnreadable != tc.unreadable ||
				got.ExperimentID != tc.experiment || got.BudgetType != tc.budgetType {
				t.Errorf("got %+v", got)
			}
		})
	}
}

// An unknown id answered as a FAULT rather than a 200 PartialError means the same thing.
func TestGetCampaignBudget_InvalidCampaignIDFaultIsAbsent(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"Errors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId"}]}`)
	})
	got, err := c.GetCampaignBudget(context.Background(), "321")
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got %+v, %v", got, err)
	}
}

func TestUpdateCampaignDailyBudget_SendsAmountAndPreservesBudgetType(t *testing.T) {
	rec := &budgetRecorder{}
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	if err := c.UpdateCampaignDailyBudget(context.Background(), "321", 125.5, BudgetTypeDailyAccelerated); err != nil {
		t.Fatalf("UpdateCampaignDailyBudget: %v", err)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].method != http.MethodPut || !strings.HasSuffix(reqs[0].path, "/CampaignManagement/v13/Campaigns") {
		t.Fatalf("want one PUT .../Campaigns, got %+v", reqs)
	}
	// The amount is sent as the decimal it is — not micros, not rounded — and the budget type
	// is the one passed in, so the write changes the amount and nothing else.
	if want := `{"AccountId":1234567,"Campaigns":[{"Id":321,"BudgetType":"DailyBudgetAccelerated","DailyBudget":125.5}]}`; reqs[0].body != want {
		t.Errorf("PUT body = %s, want %s", reqs[0].body, want)
	}
}

func TestUpdateCampaignDailyBudget_RejectsBadInputBeforeAnyCall(t *testing.T) {
	var calls int32
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	cases := []struct {
		name, id, budgetType string
		amount               float64
		amountErr            bool
	}{
		{name: "non-numeric id", id: "abc", budgetType: BudgetTypeDailyStandard, amount: 10},
		{name: "lifetime type", id: "321", budgetType: BudgetTypeLifetimeStandard, amount: 10},
		{name: "empty type", id: "321", budgetType: "", amount: 10},
		{name: "zero", id: "321", budgetType: BudgetTypeDailyStandard, amount: 0, amountErr: true},
		{name: "NaN", id: "321", budgetType: BudgetTypeDailyStandard, amount: math.NaN(), amountErr: true},
		{name: "over max", id: "321", budgetType: BudgetTypeDailyStandard, amount: maxBudget * 2, amountErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.UpdateCampaignDailyBudget(context.Background(), tc.id, tc.amount, tc.budgetType)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if got := errors.Is(err, ErrBudgetAmountInvalid); got != tc.amountErr {
				t.Errorf("errors.Is(ErrBudgetAmountInvalid) = %v, want %v (%v)", got, tc.amountErr, err)
			}
		})
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("input refusals must not reach Microsoft, saw %d call(s)", n)
	}
}

func TestUpdateCampaignDailyBudget_Classification(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
		shared      bool
		amount      bool
	}{
		{name: "5xx is unconfirmed", status: http.StatusInternalServerError, body: `{}`, unconfirmed: true},
		{name: "omitted PartialErrors is unconfirmed", status: http.StatusOK, body: `{}`, unconfirmed: true},
		{name: "plain 4xx is a definite failure", status: http.StatusBadRequest, body: `{"Errors":[{"Code":105,"ErrorCode":"InvalidCredentials"}]}`},
		{name: "unrelated PartialError is a definite failure", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1234,"ErrorCode":"Other","Index":0}]}`},
		{name: "shared budget PartialError", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1159,"ErrorCode":"CampaignServiceCannotUpdateSharedBudget","Index":0}]}`, shared: true},
		{name: "shared budget 4xx BatchError", status: http.StatusBadRequest, body: `{"BatchErrors":[{"Code":1159,"Index":0}]}`, shared: true},
		{name: "shared budget 4xx BatchError by symbolic name", status: http.StatusBadRequest, body: `{"BatchErrors":[{"ErrorCode":"CampaignServiceCannotUpdateSharedBudget","Index":0}]}`, shared: true},
		{name: "invalid daily budget PartialError", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1106,"ErrorCode":"CampaignServiceInvalidDailyBudget","Index":0}]}`, amount: true},
		{name: "below spend PartialError", status: http.StatusOK, body: `{"PartialErrors":[{"ErrorCode":"CampaignServiceCampaignBudgetAmountIsLessThanSpendAmount","Index":0}]}`, amount: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := c.UpdateCampaignDailyBudget(context.Background(), "321", 10, BudgetTypeDailyStandard)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v (%v)", got, tc.unconfirmed, err)
			}
			if got := errors.Is(err, ErrSharedBudget); got != tc.shared {
				t.Errorf("errors.Is(ErrSharedBudget) = %v, want %v (%v)", got, tc.shared, err)
			}
			_, gotAmount := BudgetAmountReason(err)
			if gotAmount != tc.amount {
				t.Errorf("BudgetAmountReason ok = %v, want %v (%v)", gotAmount, tc.amount, err)
			}
		})
	}
}

// The client-facing reason must render a large amount as a plain decimal. Seven-figure daily
// budgets are ordinary in JPY, KRW, IDR and VND, and %g would have rendered 1500000 as
// "1.5e+06" in a sentence handed back to the caller.
func TestUpdateCampaignDailyBudget_AmountReasonIsPlainDecimal(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		amount float64
		want   string
	}{
		{name: "invalid daily budget, whole amount", code: "CampaignServiceInvalidDailyBudget", amount: 1500000, want: "refused a daily budget of 1500000 as not valid"},
		{name: "below spend, whole amount", code: "CampaignServiceCampaignBudgetAmountIsLessThanSpendAmount", amount: 25000000, want: "refused a daily budget of 25000000 because it is less than"},
		{name: "invalid daily budget, fractional amount", code: "CampaignServiceInvalidDailyBudget", amount: 1234567.89, want: "refused a daily budget of 1234567.89 as not valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"PartialErrors":[{"ErrorCode":"`+tc.code+`","Index":0}]}`)
			})
			err := c.UpdateCampaignDailyBudget(context.Background(), "321", tc.amount, BudgetTypeDailyStandard)
			reason, ok := BudgetAmountReason(err)
			if !ok {
				t.Fatalf("want an amount refusal, got %v", err)
			}
			if !strings.Contains(reason, tc.want) || strings.Contains(reason, "e+") {
				t.Errorf("reason = %q, want it to contain %q in plain decimal notation", reason, tc.want)
			}
		})
	}
}

// ValidateDailyBudget's over-maximum refusal is handed back to the caller too, so it must name
// the amount exactly as given: %.2f rounded it (1000000000.005 read as "1000000000.01"), which
// misstates an amount this client deliberately never rounds, and %g would have used exponent
// form for a whole amount. Both figures use the same plain-decimal formatter as the mutate's
// refusals.
func TestValidateDailyBudget_OverMaxReasonIsPlainDecimal(t *testing.T) {
	cases := []struct {
		name   string
		amount float64
		want   string
	}{
		{name: "fractional amount is not rounded", amount: 1000000000.005, want: "the Microsoft Advertising daily budget 1000000000.005 exceeds the maximum 1000000000"},
		{name: "whole amount has no exponent", amount: 1.5e9, want: "the Microsoft Advertising daily budget 1500000000 exceeds the maximum 1000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := BudgetAmountReason(ValidateDailyBudget(tc.amount))
			if !ok {
				t.Fatalf("want an amount refusal for %v", tc.amount)
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

// The extracted putUpdate must leave the status toggle's error text exactly as it was, since
// an operator reads that text to decide what to verify by hand.
func TestPutStatus_RejectionTextUnchanged(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"PartialErrors":[{"Code":1234,"Index":0}]}`)
	})
	err := c.putStatus(context.Background(), "Campaigns", struct{}{}, "campaign")
	if err == nil || err.Error() != "microsoft-ads rejected the campaign status update: 1234" {
		t.Fatalf("status rejection text changed: %v", err)
	}
}
