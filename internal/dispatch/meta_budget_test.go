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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is optional and is reached by TYPE ASSERTION in the orchestrator, so a
// signature drift would silently downgrade this dispatcher to "budget writes unsupported"
// rather than failing the build. This assertion makes that a compile error.
var _ service.BudgetWriter = (*MetaDispatcher)(nil)

// metaBudgetDispatcher wires a MetaDispatcher against a fake Graph API. The ad-set read is
// answered with adSetJSON, the account preflight with the given currency, and any POST gets
// writeStatus. Every request is recorded with its body, because what a budget write set — and
// on which node — is the whole subject.
func metaBudgetDispatcher(t *testing.T, adSetJSON, currency string, writeStatus int) (*MetaDispatcher, func() []budgetRequest) {
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
			if writeStatus < 400 {
				_, _ = io.WriteString(w, `{"success":true}`)
			} else {
				_, _ = io.WriteString(w, `{"error":{"message":"nope","code":100}}`)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/act_") {
			_, _ = io.WriteString(w, `{"name":"LF","account_status":1,"currency":"`+currency+`"}`)
			return
		}
		_, _ = io.WriteString(w, adSetJSON)
	}))
	t.Cleanup(srv.Close)

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL),
	)
	return d, func() []budgetRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]budgetRequest, len(seen))
		copy(out, seen)
		return out
	}
}

// metaBudgetCampaign is a campaign row recording provenance matching activeMetaConn (act_777)
// and the ad set the budget lives on, so every test exercises its own subject. CampaignResult
// is marshalled UNTAGGED, so the persisted keys are the Go field names.
func metaBudgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderMetaAds, PlatformCampaignID: "555",
		Result: []byte(`{"AccountID":"act_777","AdSetID":"777"}`),
	}
}

func metaWrites(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func assertNoMetaWrite(t *testing.T, reqs []budgetRequest) {
	t.Helper()
	if w := metaWrites(reqs); len(w) != 0 {
		t.Fatalf("refusal still issued %d write(s): %+v — the caller is told nothing changed while the platform was written", len(w), w)
	}
}

const metaAdSetHead = `{"id":"777","status":"PAUSED","campaign_id":"555"`

func TestMeta_WriteBudget_Daily(t *testing.T) {
	d, calls := metaBudgetDispatcher(t, metaAdSetHead+`,"daily_budget":"10000"}`, "USD", http.StatusOK)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(),
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	writes := metaWrites(calls())
	if len(writes) != 1 {
		t.Fatalf("want exactly one write, got %d: %+v", len(writes), writes)
	}
	// The AD SET node, not the campaign: Meta's budget lives on the ad set.
	if writes[0].Path != "/777" {
		t.Errorf("wrote to %q, want the ad set node /777", writes[0].Path)
	}
	// 250 USD encoded through the account's own currency scale.
	if !strings.Contains(writes[0].Body, `"daily_budget":"25000"`) {
		t.Errorf("write did not set daily_budget to 25000 minor units: %s", writes[0].Body)
	}
	if strings.Contains(writes[0].Body, "lifetime_budget") {
		t.Errorf("a daily write must not touch lifetime_budget: %s", writes[0].Body)
	}
}

func TestMeta_WriteBudget_Lifetime(t *testing.T) {
	d, calls := metaBudgetDispatcher(t, metaAdSetHead+`,"lifetime_budget":"100000"}`, "USD", http.StatusOK)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(),
		model.BudgetChange{Amount: 2000, Type: model.BudgetLifetime})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	writes := metaWrites(calls())
	if len(writes) != 1 || !strings.Contains(writes[0].Body, `"lifetime_budget":"200000"`) {
		t.Fatalf("want one lifetime_budget write of 200000 minor units, got %+v", writes)
	}
}

// TestMeta_WriteBudget_ZeroDecimalCurrency: the account's currency decides the scale, and
// assuming 100 on a JPY account would encode the budget 100x too high. The read half of the
// same account preflight the create path runs is what prevents it.
func TestMeta_WriteBudget_ZeroDecimalCurrency(t *testing.T) {
	d, calls := metaBudgetDispatcher(t, metaAdSetHead+`,"daily_budget":"3000"}`, "JPY", http.StatusOK)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(),
		model.BudgetChange{Amount: 5000, Type: model.BudgetDaily})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}
	writes := metaWrites(calls())
	if len(writes) != 1 || !strings.Contains(writes[0].Body, `"daily_budget":"5000"`) {
		t.Fatalf("a zero-decimal currency must be written unscaled, got %+v", writes)
	}
}

// TestMeta_WriteBudget_Guards is the refusal table. Every case must return the named sentinel
// AND leave the platform untouched.
func TestMeta_WriteBudget_Guards(t *testing.T) {
	cases := []struct {
		name      string
		adSetJSON string
		currency  string
		change    model.BudgetChange
		wantErr   error
		wantText  string
	}{
		{
			name:      "an unreadable current amount is refused, not read as absent",
			adSetJSON: metaAdSetHead + `,"daily_budget":"lots"}`,
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "could not read",
		},
		{
			// Meta's form of Google's shared budget: the campaign holds the amount
			// and distributes it across every ad set beneath it.
			name:      "a campaign-level (CBO) budget is refused",
			adSetJSON: metaAdSetHead + `,"campaign":{"id":"555","daily_budget":"100000"}}`,
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "Campaign Budget Optimization",
		},
		{
			// CBO is checked BEFORE pacing: an ad set under a CBO campaign has no
			// budget fields of its own, so a pacing answer there would explain the
			// wrong thing.
			name:      "a lifetime CBO budget is refused as CBO, not as a pacing mismatch",
			adSetJSON: metaAdSetHead + `,"campaign":{"id":"555","lifetime_budget":"100000"}}`,
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "Campaign Budget Optimization",
		},
		{
			name:      "BOTH budgets present is ambiguous and refused",
			adSetJSON: metaAdSetHead + `,"daily_budget":"10000","lifetime_budget":"100000"}`,
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "BOTH a daily and a lifetime budget",
		},
		{
			name:      "NEITHER budget present gives nothing to write",
			adSetJSON: metaAdSetHead + `}`,
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "no budget at all",
		},
		{
			name:      "a pacing model that does not match upstream is refused",
			adSetJSON: metaAdSetHead + `,"daily_budget":"10000"}`,
			change:    model.BudgetChange{Amount: 2000, Type: model.BudgetLifetime},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "never its pacing model",
		},
		{
			name:      "the reverse mismatch is refused too",
			adSetJSON: metaAdSetHead + `,"lifetime_budget":"100000"}`,
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantErr:   domain.ErrBudgetUnwritable,
			wantText:  "never its pacing model",
		},
		{
			// An unknown currency has no scale, and guessing one is how a budget
			// gets encoded 100x wrong.
			name:      "an unknown account currency is refused",
			adSetJSON: metaAdSetHead + `,"daily_budget":"10000"}`,
			currency:  "XYZ",
			change:    model.BudgetChange{Amount: 250, Type: model.BudgetDaily},
			wantText:  "unsupported or missing currency code",
		},
		{
			name:      "an amount below one minor unit is refused",
			adSetJSON: metaAdSetHead + `,"daily_budget":"10000"}`,
			change:    model.BudgetChange{Amount: 0.004, Type: model.BudgetDaily},
			wantText:  "budget too small",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			currency := tc.currency
			if currency == "" {
				currency = "USD"
			}
			d, calls := metaBudgetDispatcher(t, tc.adSetJSON, currency, http.StatusOK)

			err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(), tc.change)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error does not wrap %v: %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
			assertNoMetaWrite(t, calls())
		})
	}
}

// TestMeta_WriteBudget_NoAdSetIsUnprovisioned. A Meta budget lives on the ad set, so a row
// that records none has nothing addressable to write — a deterministic 409, not a 503.
func TestMeta_WriteBudget_NoAdSetIsUnprovisioned(t *testing.T) {
	d, calls := metaBudgetDispatcher(t, metaAdSetHead+`,"daily_budget":"10000"}`, "USD", http.StatusOK)
	campaign := metaBudgetCampaign()
	campaign.Result = []byte(`{"AccountID":"act_777"}`)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, campaign,
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err == nil || !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("want ErrCampaignNotProvisioned, got %v", err)
	}
	assertNoMetaWrite(t, calls())
}

// TestMeta_WriteBudget_AbsentProvenanceIsRefused is the guard deliberately STRICTER than
// verifyMetaAccountMatch, which treats BOTH unknowns as "proceed" so the toggle and metrics
// paths keep working on account-less connections. A budget write can accept neither: the
// account is also what supplies the CURRENCY the amount is encoded in.
func TestMeta_WriteBudget_AbsentProvenanceIsRefused(t *testing.T) {
	for _, result := range [][]byte{nil, []byte(`{"AdSetID":"777"}`), []byte(`{not json`)} {
		d, calls := metaBudgetDispatcher(t, metaAdSetHead+`,"daily_budget":"10000"}`, "USD", http.StatusOK)
		campaign := metaBudgetCampaign()
		campaign.Result = result

		err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, campaign,
			model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
		if err == nil {
			t.Fatalf("result %q: want a refusal", result)
		}
		if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Errorf("result %q: want both provenance sentinels, got %v", result, err)
		}
		if reqs := calls(); len(reqs) != 0 {
			t.Errorf("result %q: an absent-provenance refusal contacted the platform: %+v", result, reqs)
		}
	}
}

// TestMeta_WriteBudget_NoAccountSelectedIsRefused is the half no other Meta path has. Toggle
// and metrics deliberately tolerate an account-less connection because they address the
// campaign node by id. A budget write cannot: with no account there is no currency, and with
// no currency there is no scale that could be assumed.
func TestMeta_WriteBudget_NoAccountSelectedIsRefused(t *testing.T) {
	conn := activeMetaConn(goodMetaCreds)
	conn.AccountID = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("an account-less connection must be refused before any call: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, meta.WithBaseURL(srv.URL))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(),
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err == nil || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("want a refusal wrapping ErrCampaignAccountMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Errorf("the refusal should say why the account is needed: %v", err)
	}
}

func TestMeta_WriteBudget_MismatchedProvenanceIsRefused(t *testing.T) {
	d, calls := metaBudgetDispatcher(t, metaAdSetHead+`,"daily_budget":"10000"}`, "USD", http.StatusOK)
	campaign := metaBudgetCampaign()
	campaign.Result = []byte(`{"AccountID":"act_999","AdSetID":"777"}`)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, campaign,
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err == nil || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("want ErrCampaignAccountMismatch, got %v", err)
	}
	assertNoMetaWrite(t, calls())
}

// TestMeta_WriteBudget_AFailedReadIsDefiniteNotUnconfirmed. Both pre-write calls — the ad-set
// read and the account preflight — mutate nothing, so neither may be reported as a write whose
// outcome is unknown.
func TestMeta_WriteBudget_AFailedReadIsDefiniteNotUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("a failed read must not be followed by a write")
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(),
		model.BudgetChange{Amount: 250, Type: model.BudgetDaily})
	if err == nil {
		t.Fatal("want an error")
	}
	var u interface{ Unconfirmed() bool }
	if errors.As(err, &u) && u.Unconfirmed() {
		t.Errorf("a failed READ must not be reported as an unconfirmed write: %v", err)
	}
}

// TestMeta_WriteBudget_ClassifiesTheMutate is the split the service's answer depends on.
func TestMeta_WriteBudget_ClassifiesTheMutate(t *testing.T) {
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
			d, _ := metaBudgetDispatcher(t, metaAdSetHead+`,"daily_budget":"10000"}`, "USD", tc.status)

			err := d.WriteBudget(context.Background(), "proj", model.ProviderMetaAds, metaBudgetCampaign(),
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
