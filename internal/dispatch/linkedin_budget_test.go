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

// The capability is optional and is reached by TYPE ASSERTION in the orchestrator, so nothing
// in the build fails if this method's signature drifts away from the interface — the
// dispatcher would simply stop being a BudgetWriter and every LinkedIn budget write would
// start answering "unsupported". This assertion turns that silent downgrade into a compile
// error, exactly as the Google Ads slice's does.
var _ service.BudgetWriter = (*LinkedInDispatcher)(nil)

// liBudgetDispatcher wires a LinkedInDispatcher against a fake LinkedIn API that answers the
// campaign read with readJSON and accepts any PARTIAL_UPDATE, recording every request with its
// method and body. Recording the BODY is the point: a harness that only counted calls would
// pass a mutate that wrote the wrong amount into the wrong field.
func liBudgetDispatcher(t *testing.T, readJSON string, writeStatus int) (*LinkedInDispatcher, func() []budgetRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []budgetRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()
		if r.Method == http.MethodPost {
			w.WriteHeader(writeStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, readJSON)
	}))
	t.Cleanup(srv.Close)

	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL),
	)
	return d, func() []budgetRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]budgetRequest, len(seen))
		copy(out, seen)
		return out
	}
}

// liBudgetCampaign is a campaign row recording provenance matching activeLinkedInConn, so
// every test below exercises its own subject rather than the provenance guard.
func liBudgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderLinkedInAds, PlatformCampaignID: "777",
		Result: []byte(`{"accountId":"123456789"}`),
	}
}

// liWrites returns just the mutating requests out of a recording.
func liWrites(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

// assertNoLinkedInWrite is the assertion every refusal test shares. A guard that returns the
// right sentinel AFTER the platform was written is not a guard: the caller is told the request
// was refused while the money moved.
func assertNoLinkedInWrite(t *testing.T, reqs []budgetRequest) {
	t.Helper()
	if w := liWrites(reqs); len(w) != 0 {
		t.Fatalf("refusal still issued %d write(s): %+v — the caller is told nothing changed while the platform was written", len(w), w)
	}
}

func liDaily(amount string) string {
	return `{"id":777,"status":"ACTIVE","dailyBudget":{"amount":"` + amount + `","currencyCode":"USD"}}`
}

func TestLinkedIn_WriteBudget_Daily(t *testing.T) {
	d, calls := liBudgetDispatcher(t, liDaily("100.00"), http.StatusNoContent)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	writes := liWrites(calls())
	if len(writes) != 1 {
		t.Fatalf("want exactly one write, got %d: %+v", len(writes), writes)
	}
	if !strings.Contains(writes[0].Body, `"dailyBudget"`) || !strings.Contains(writes[0].Body, `"250.00"`) {
		t.Errorf("write did not set dailyBudget to 250.00: %s", writes[0].Body)
	}
	if strings.Contains(writes[0].Body, `"totalBudget"`) {
		t.Errorf("a daily write must not touch totalBudget: %s", writes[0].Body)
	}
}

func TestLinkedIn_WriteBudget_Lifetime(t *testing.T) {
	d, calls := liBudgetDispatcher(t,
		`{"id":777,"totalBudget":{"amount":"1000.00","currencyCode":"USD"}}`, http.StatusNoContent)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
		model.BudgetChange{Amount: 2000, Type: model.BudgetLifetime})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	writes := liWrites(calls())
	if len(writes) != 1 || !strings.Contains(writes[0].Body, `"totalBudget"`) || !strings.Contains(writes[0].Body, `"2000.00"`) {
		t.Fatalf("want one totalBudget write of 2000.00, got %+v", writes)
	}
}

// TestLinkedIn_WriteBudget_Guards is the refusal table. Every case must return the named
// sentinel AND leave the platform untouched — the second half is what makes a 409 honest.
func TestLinkedIn_WriteBudget_Guards(t *testing.T) {
	cases := []struct {
		name     string
		readJSON string
		change   model.BudgetChange
		wantErr  error
		wantText string
	}{
		{
			name:     "an unreadable current amount is refused, not read as absent",
			readJSON: `{"id":777,"dailyBudget":{"amount":"lots","currencyCode":"USD"}}`,
			change:   model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "could not read",
		},
		{
			// The minimums enforced here are USD-specific and this client only ever
			// SENDS USD, so writing over a EUR campaign would silently redenominate it.
			name:     "a non-USD campaign is refused",
			readJSON: `{"id":777,"dailyBudget":{"amount":"100.00","currencyCode":"EUR"}}`,
			change:   model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "denominated in EUR",
		},
		{
			name: "BOTH budgets present is ambiguous and refused",
			readJSON: `{"id":777,"dailyBudget":{"amount":"100.00","currencyCode":"USD"},
				"totalBudget":{"amount":"1000.00","currencyCode":"USD"}}`,
			change:   model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "BOTH a daily and a total budget",
		},
		{
			// The currency guard sits BELOW the pacing guard precisely so this case is
			// reachable and refusable: a campaign that HAS a budget whose currencyCode
			// LinkedIn did not report. Writing here would send "USD" over an amount
			// denominated in something the platform declined to name.
			name:     "a budget whose currency LinkedIn did not report is refused",
			readJSON: `{"id":777,"dailyBudget":{"amount":"100.00"}}`,
			change:   model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "a currency LinkedIn did not report",
		},
		{
			name:     "NEITHER budget present gives nothing to write",
			readJSON: `{"id":777,"status":"ACTIVE"}`,
			change:   model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "no budget at all",
		},
		{
			name:     "a pacing model that does not match upstream is refused",
			readJSON: liDaily("100.00"),
			change:   model.BudgetChange{Amount: 2000, Type: model.BudgetLifetime},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "never its pacing model",
		},
		{
			name:     "the reverse mismatch is refused too",
			readJSON: `{"id":777,"totalBudget":{"amount":"1000.00","currencyCode":"USD"}}`,
			change:   model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetUnwritable,
			wantText: "never its pacing model",
		},
		{
			// The shared validator's rules reach the write path: an amount this
			// service would refuse to CREATE with cannot be reached by editing.
			//
			// wantErr is the load-bearing half here. LinkedIn's minimums have no
			// equivalent in the service layer, so an UNCLASSIFIED refusal falls to
			// that layer's default arm and is answered 503 "the campaign was not
			// modified" — with a retry invitation, for a request that can never
			// succeed. Asserting the text alone passes while the status is wrong.
			name:     "an amount below LinkedIn's daily minimum is refused",
			readJSON: liDaily("100.00"),
			change:   model.BudgetChange{Amount: 5, Type: model.BudgetDaily},
			wantErr:  domain.ErrBudgetAmountRejected,
			wantText: "below LinkedIn's minimum",
		},
		{
			name:     "an amount below LinkedIn's lifetime minimum is refused",
			readJSON: `{"id":777,"totalBudget":{"amount":"1000.00","currencyCode":"USD"}}`,
			change:   model.BudgetChange{Amount: 50, Type: model.BudgetLifetime},
			wantErr:  domain.ErrBudgetAmountRejected,
			wantText: "below LinkedIn's minimum",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := liBudgetDispatcher(t, tc.readJSON, http.StatusNoContent)

			err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(), tc.change)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error does not wrap %v: %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
			assertNoLinkedInWrite(t, calls())
		})
	}
}

// TestLinkedIn_WriteBudget_AbsentProvenanceIsRefused is the guard that is deliberately
// STRICTER than verifyLinkedInAccountMatch. That helper returns nil for a row recording no
// creating account — tolerated by ToggleStatus and ReadMetrics, forbidden here, because
// BudgetWriter requires the account-identity invariant enforced at least as strictly as the
// read path enforces it and the cost of being wrong is moving another account's money.
func TestLinkedIn_WriteBudget_AbsentProvenanceIsRefused(t *testing.T) {
	for _, result := range [][]byte{nil, []byte(`{}`), []byte(`{"accountId":""}`), []byte(`{not json`)} {
		d, calls := liBudgetDispatcher(t, liDaily("100.00"), http.StatusNoContent)
		campaign := liBudgetCampaign()
		campaign.Result = result

		err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, campaign,
			model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
		if err == nil {
			t.Fatalf("result %q: want a refusal", result)
		}
		if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Errorf("result %q: want both provenance sentinels, got %v", result, err)
		}
		// Not one call of any kind: the refusal is decided from the persisted row.
		if reqs := calls(); len(reqs) != 0 {
			t.Errorf("result %q: an absent-provenance refusal contacted the platform: %+v", result, reqs)
		}
	}
}

// TestLinkedIn_WriteBudget_MismatchedProvenanceIsRefused covers the other half: a row created
// under a DIFFERENT account, answered in the one wording every adapter uses.
func TestLinkedIn_WriteBudget_MismatchedProvenanceIsRefused(t *testing.T) {
	d, calls := liBudgetDispatcher(t, liDaily("100.00"), http.StatusNoContent)
	campaign := liBudgetCampaign()
	campaign.Result = []byte(`{"accountId":"999999999"}`)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, campaign,
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err == nil || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("want ErrCampaignAccountMismatch, got %v", err)
	}
	assertNoLinkedInWrite(t, calls())
}

// TestLinkedIn_WriteBudget_AFailedReadIsDefiniteNotUnconfirmed. The read mutates nothing, so
// classifying its failure as unconfirmed would send an operator to verify a write that never
// existed — the mis-scoping the Google Ads slice corrected.
func TestLinkedIn_WriteBudget_AFailedReadIsDefiniteNotUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("a failed read must not be followed by a write")
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL),
	)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err == nil {
		t.Fatal("want an error")
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		t.Errorf("a failed READ must not be reported as an unconfirmed write: %v", err)
	}
}

// TestLinkedIn_WriteBudget_ClassifiesTheMutate is the split the service's 503-vs-"verify"
// answer depends on. A 5xx on a money-moving write may have been applied; a definite 4xx was
// not, and reporting it as unconfirmed would send an operator after nothing.
func TestLinkedIn_WriteBudget_ClassifiesTheMutate(t *testing.T) {
	cases := []struct {
		name            string
		status          int
		wantUnconfirmed bool
	}{
		{name: "server error is unconfirmed", status: http.StatusBadGateway, wantUnconfirmed: true},
		{name: "bad request is definite", status: http.StatusBadRequest, wantUnconfirmed: false},
		{name: "forbidden is definite", status: http.StatusForbidden, wantUnconfirmed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := liBudgetDispatcher(t, liDaily("100.00"), tc.status)

			err := d.WriteBudget(context.Background(), "proj", model.ProviderLinkedInAds, liBudgetCampaign(),
				model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
			if err == nil {
				t.Fatal("want an error")
			}
			var u interface{ Unconfirmed() bool }
			got := errors.As(err, &u) && u.Unconfirmed()
			if got != tc.wantUnconfirmed {
				t.Errorf("Unconfirmed = %v, want %v (err: %v)", got, tc.wantUnconfirmed, err)
			}
		})
	}
}
