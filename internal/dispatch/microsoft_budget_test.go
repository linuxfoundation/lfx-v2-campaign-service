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
// Microsoft budget write back into "unsupported". This makes that a compile error, as each
// sibling slice does.
var _ service.BudgetWriter = (*MicrosoftDispatcher)(nil)

// msBudgetDispatcher wires a MicrosoftDispatcher against a fake Microsoft API that answers the
// QueryByIds read with readJSON and the UpdateCampaigns PUT with (writeStatus, writeJSON),
// recording every API request WITH its body: a harness that only counted calls would pass a
// mutate that wrote the wrong amount or flipped the budget type.
//
// Every failure the handler could notice is reported from the test goroutine, never with
// t.Fatal inside the handler.
func msBudgetDispatcher(t *testing.T, readJSON string, writeStatus int, writeJSON string) (*MicrosoftDispatcher, func() []budgetRequest) {
	t.Helper()
	return msBudgetDispatcherWith(t, readJSON, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(writeStatus)
		_, _ = io.WriteString(w, writeJSON)
	})
}

// msBudgetDispatcherWith is msBudgetDispatcher with the PUT answered by write, and opts passed to
// the Microsoft client — for the outcomes a fixed status and body cannot express, such as a PUT
// that never answers before the client's deadline.
func msBudgetDispatcherWith(t *testing.T, readJSON string, write http.HandlerFunc, opts ...microsoft.Option) (*MicrosoftDispatcher, func() []budgetRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []budgetRequest
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			write(w, r)
			return
		}
		_, _ = io.WriteString(w, readJSON)
	}))
	t.Cleanup(apiSrv.Close)

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		append([]microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL)}, opts...)...,
	)
	return d, func() []budgetRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]budgetRequest(nil), seen...)
	}
}

// msBudgetCampaign records provenance matching activeMicrosoftConn (account 1234567), so every
// test below exercises its own subject rather than the provenance guard.
func msBudgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "321",
		Result: json.RawMessage(`{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987"}`),
	}
}

func msOwnBudget(budgetType string) string {
	return `{"Campaigns":[{"Id":321,"BudgetId":null,"BudgetType":"` + budgetType + `","DailyBudget":50}],"PartialErrors":[]}`
}

func msWrites(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if r.Method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

func assertNoMicrosoftWrite(t *testing.T, reqs []budgetRequest) {
	t.Helper()
	if w := msWrites(reqs); len(w) != 0 {
		t.Fatalf("refusal still issued %d write(s): %+v — the caller is told nothing changed while the platform was written", len(w), w)
	}
}

func TestMicrosoft_WriteBudget_DailyWritesAmountAndKeepsBudgetType(t *testing.T) {
	d, calls := msBudgetDispatcher(t, msOwnBudget(microsoft.BudgetTypeDailyStandard), http.StatusOK, `{"PartialErrors":[]}`)
	err := d.WriteBudget(context.Background(), "proj", model.ProviderMicrosoftAds, msBudgetCampaign(),
		model.BudgetChange{Amount: 75.25, Type: model.BudgetDaily})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	reqs := calls()
	if len(reqs) != 2 || !strings.HasSuffix(reqs[0].Path, "/Campaigns/QueryByIds") {
		t.Fatalf("want the QueryByIds read then one write, got %+v", reqs)
	}
	writes := msWrites(reqs)
	if len(writes) != 1 || !strings.HasSuffix(writes[0].Path, "/CampaignManagement/v13/Campaigns") {
		t.Fatalf("want exactly one PUT .../Campaigns, got %+v", writes)
	}
	want := `{"AccountId":1234567,"Campaigns":[{"Id":321,"BudgetType":"DailyBudgetStandard","DailyBudget":75.25}]}`
	if writes[0].Body != want {
		t.Errorf("PUT body = %s\nwant       %s", writes[0].Body, want)
	}
}

// TestMicrosoft_WriteBudget_AcceleratedSearchCampaignRefused pins GUARD 3's contradiction case:
// Microsoft documents DailyBudgetAccelerated as Audience-only, and the read asks for Search
// campaigns, so a Search campaign reporting it is refused — and the platform is not written —
// rather than having the contradictory type echoed back on the PUT.
func TestMicrosoft_WriteBudget_AcceleratedSearchCampaignRefused(t *testing.T) {
	d, calls := msBudgetDispatcher(t, msOwnBudget(microsoft.BudgetTypeDailyAccelerated), http.StatusOK, `{"PartialErrors":[]}`)
	err := d.WriteBudget(context.Background(), "proj", model.ProviderMicrosoftAds, msBudgetCampaign(),
		model.BudgetChange{Amount: 75.25, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("err = %v, want ErrBudgetUnwritable", err)
	}
	if !strings.Contains(err.Error(), "only to Audience campaigns") {
		t.Errorf("err = %v, want it to say Accelerated is Audience-only", err)
	}
	assertNoMicrosoftWrite(t, calls())
}

// TestMicrosoft_WriteBudget_Guards is the refusal table: each case must return its sentinel AND
// leave the platform unwritten — the second half is what makes a 409 honest.
func TestMicrosoft_WriteBudget_Guards(t *testing.T) {
	cases := []struct {
		name     string
		readJSON string
		change   model.BudgetChange
		want     error
		noCalls  bool
	}{
		{
			name:    "lifetime request is refused before any call",
			change:  model.BudgetChange{Amount: 500, Type: model.BudgetLifetime},
			want:    domain.ErrBudgetUnwritable,
			noCalls: true,
		},
		{
			name:     "shared budget",
			readJSON: `{"Campaigns":[{"Id":321,"BudgetId":"8888","BudgetType":"DailyBudgetStandard","DailyBudget":500}],"PartialErrors":[]}`,
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrBudgetShared,
		},
		{
			name:     "unreadable BudgetId is not assumed private",
			readJSON: `{"Campaigns":[{"Id":321,"BudgetId":-1,"BudgetType":"DailyBudgetStandard"}],"PartialErrors":[]}`,
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrBudgetUnwritable,
		},
		{
			name:     "experiment campaign",
			readJSON: `{"Campaigns":[{"Id":321,"ExperimentId":77,"BudgetType":"DailyBudgetStandard"}],"PartialErrors":[]}`,
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrBudgetUnwritable,
		},
		{
			name:     "unreported budget type",
			readJSON: `{"Campaigns":[{"Id":321,"DailyBudget":50}],"PartialErrors":[]}`,
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrBudgetUnwritable,
		},
		{
			name:     "lifetime upstream",
			readJSON: msOwnBudget(microsoft.BudgetTypeLifetimeStandard),
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrBudgetUnwritable,
		},
		{
			name:     "unknown budget type",
			readJSON: msOwnBudget("MonthlyBudgetSpendUntilDepleted"),
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrBudgetUnwritable,
		},
		{
			name:     "campaign absent upstream",
			readJSON: `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`,
			change:   model.BudgetChange{Amount: 75, Type: model.BudgetDaily},
			want:     domain.ErrPlatformCampaignAbsent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := msBudgetDispatcher(t, tc.readJSON, http.StatusOK, `{"PartialErrors":[]}`)
			err := d.WriteBudget(context.Background(), "proj", model.ProviderMicrosoftAds, msBudgetCampaign(), tc.change)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %T: %v", tc.want, err, err)
			}
			reqs := calls()
			assertNoMicrosoftWrite(t, reqs)
			if tc.noCalls && len(reqs) != 0 {
				t.Errorf("refusal must not reach Microsoft at all, saw %+v", reqs)
			}
		})
	}
}

// Provenance is enforced more strictly than on ToggleStatus: a row with NO recorded account is
// refused (ToggleStatus proceeds), and a row from ANOTHER account is refused — both before any
// token or API request.
func TestMicrosoft_WriteBudget_ProvenanceRefusedBeforeAnyCall(t *testing.T) {
	cases := []struct {
		name, result string
		want         []error
	}{
		{"foreign account", `{"accountId":"7654321","campaignId":"321"}`, []error{domain.ErrCampaignAccountMismatch}},
		{"no provenance recorded", `{"campaignId":"321"}`, []error{domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch}},
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
			err := d.WriteBudget(context.Background(), "proj", model.ProviderMicrosoftAds, camp,
				model.BudgetChange{Amount: 75, Type: model.BudgetDaily})
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Errorf("want %v in the chain, got %T: %v", want, err, err)
				}
			}
			mu.Lock()
			got := append([]string(nil), reached...)
			mu.Unlock()
			if len(got) != 0 {
				t.Errorf("provenance refusal must precede every request, saw %v", got)
			}
		})
	}
}

func TestMicrosoft_WriteBudget_MalformedCampaignIDRefusedBeforeAnyCall(t *testing.T) {
	d, calls := msBudgetDispatcher(t, msOwnBudget(microsoft.BudgetTypeDailyStandard), http.StatusOK, `{"PartialErrors":[]}`)
	camp := msBudgetCampaign()
	camp.PlatformCampaignID = "abc"
	err := d.WriteBudget(context.Background(), "proj", model.ProviderMicrosoftAds, camp,
		model.BudgetChange{Amount: 75, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("want ErrBudgetUnwritable, got %T: %v", err, err)
	}
	if reqs := calls(); len(reqs) != 0 {
		t.Errorf("a malformed id must not reach Microsoft, saw %+v", reqs)
	}
}

// TestMicrosoft_WriteBudget_WriteOutcomes pins the classification of the PUT: ambiguity (5xx,
// 3xx, a transport timeout, unanswered body) is UNCONFIRMED; a definite 4xx or PartialError is a definite failure; the
// shared-budget and amount refusals keep their own identities.
func TestMicrosoft_WriteBudget_WriteOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
		want        error
		// write, when set, answers the PUT in place of (status, body); opts reach the client.
		write http.HandlerFunc
		opts  []microsoft.Option
	}{
		{name: "5xx is unconfirmed", status: http.StatusBadGateway, body: ``, unconfirmed: true},
		// Redirects are never followed (noFollow), so a 3xx is the PUT's last answer and says
		// nothing about whether the update applied.
		{name: "3xx is unconfirmed", status: http.StatusTemporaryRedirect, body: ``, unconfirmed: true},
		// The PUT was SENT and the answer never arrived: the update may have applied.
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
		{name: "shared budget backstop", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1159,"ErrorCode":"CampaignServiceCannotUpdateSharedBudget","Index":0}]}`, want: domain.ErrBudgetShared},
		{name: "amount refused", status: http.StatusOK, body: `{"PartialErrors":[{"Code":1106,"ErrorCode":"CampaignServiceInvalidDailyBudget","Index":0}]}`, want: domain.ErrBudgetAmountRejected},
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
			d, calls := msBudgetDispatcherWith(t, msOwnBudget(microsoft.BudgetTypeDailyStandard), write, tc.opts...)
			err := d.WriteBudget(context.Background(), "proj", model.ProviderMicrosoftAds, msBudgetCampaign(),
				model.BudgetChange{Amount: 75, Type: model.BudgetDaily})
			if err == nil {
				t.Fatal("want an error")
			}
			var u interface{ Unconfirmed() bool }
			gotUnconfirmed := errors.As(err, &u) && u.Unconfirmed()
			if gotUnconfirmed != tc.unconfirmed {
				t.Errorf("Unconfirmed = %v, want %v (%v)", gotUnconfirmed, tc.unconfirmed, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("want %v in the chain, got %v", tc.want, err)
			}
			if tc.want == nil && (errors.Is(err, domain.ErrBudgetShared) || errors.Is(err, domain.ErrBudgetAmountRejected)) {
				t.Errorf("a generic failure must not be classified as a specific refusal: %v", err)
			}
			if tc.want == domain.ErrBudgetAmountRejected {
				var r interface{ BudgetAmountReason() string }
				if !errors.As(err, &r) || !strings.Contains(r.BudgetAmountReason(), "Microsoft Advertising") {
					t.Errorf("an amount refusal must carry a client-safe reason, got %v", err)
				}
			}
			if n := len(msWrites(calls())); n != 1 {
				t.Errorf("want exactly one write attempt, got %d", n)
			}
		})
	}
}
