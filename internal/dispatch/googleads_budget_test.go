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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is optional and is reached by type assertion in the orchestrator, so nothing
// in the build fails if this method's signature drifts away from the interface — the
// dispatcher would simply stop being a BudgetWriter and every budget write would start
// answering "unsupported". This assertion turns that silent downgrade into a compile error,
// and it lives with the tests for the same reason the AccountLister one does: it moves with
// the code.
var _ service.BudgetWriter = (*GoogleAdsDispatcher)(nil)

// budgetRequest is one call the dispatcher made to the fake Google Ads API.
type budgetRequest struct {
	Method string
	Path   string
	Body   string
}

// budgetDispatcher wires a GoogleAdsDispatcher against a fake Google Ads API that answers the
// settings query with searchJSON and accepts any mutate, recording every request with its
// body. Recording the BODY is what separates this harness from settingsDispatcher's: the
// whole point of a budget write is which field it set and under which updateMask, and a
// harness that only recorded paths would pass a mutate that wrote the wrong amount field.
func budgetDispatcher(t *testing.T, searchJSON string) (*GoogleAdsDispatcher, func() []budgetRequest) {
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
		if strings.Contains(r.URL.Path, "campaignBudgets:mutate") {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/555"}]}`)
			return
		}
		_, _ = io.WriteString(w, searchJSON)
	}))
	t.Cleanup(apiSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	return d, func() []budgetRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]budgetRequest, len(seen))
		copy(out, seen)
		return out
	}
}

// budgetCampaign is a campaign row recording the provenance the fake connection resolves to,
// so every test below exercises its own subject rather than the provenance guard.
func budgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result: settingsProvenance(),
	}
}

// budgetSearchJSON renders a settings-query answer whose campaignBudget object is exactly the
// fields given, so each guard test can withhold ONE fact and nothing else.
func budgetSearchJSON(budgetObj string) string {
	return `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":` + budgetObj + `}]}`
}

// mutateCalls returns just the campaignBudgets:mutate requests out of a recording.
func mutateCalls(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if strings.Contains(r.Path, "campaignBudgets:mutate") {
			out = append(out, r)
		}
	}
	return out
}

// assertNoMutate is the assertion every refusal test shares. A guard that returns the right
// sentinel AFTER the platform was already written is not a guard: the caller is told the
// request was refused while the money moved, which is the one outcome this capability is
// built to make impossible.
func assertNoMutate(t *testing.T, reqs []budgetRequest) {
	t.Helper()
	if m := mutateCalls(reqs); len(m) != 0 {
		t.Fatalf("refusal still issued %d campaignBudgets:mutate call(s): %+v — the caller is told nothing changed while the platform was written", len(m), m)
	}
}

// TestGoogleAds_WriteBudget_Daily is the happy path, and it asserts the WIRE, not just the
// error being nil: exactly one mutate, naming the budget resource the READ reported, carrying
// amountMicros (never totalAmountMicros) under the amount_micros mask.
func TestGoogleAds_WriteBudget_Daily(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":false}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}

	calls := mutateCalls(requests())
	if len(calls) != 1 {
		t.Fatalf("got %d campaignBudgets:mutate calls, want exactly 1: %+v", len(calls), calls)
	}
	var req struct {
		Operations []struct {
			UpdateMask string `json:"updateMask"`
			Update     struct {
				ResourceName      string `json:"resourceName"`
				AmountMicros      *int64 `json:"amountMicros"`
				TotalAmountMicros *int64 `json:"totalAmountMicros"`
			} `json:"update"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(calls[0].Body), &req); err != nil {
		t.Fatalf("mutate body is not the expected shape: %v\nbody: %s", err, calls[0].Body)
	}
	if len(req.Operations) != 1 {
		t.Fatalf("got %d operations, want 1: %s", len(req.Operations), calls[0].Body)
	}
	op := req.Operations[0]
	if op.UpdateMask != "amount_micros" {
		t.Errorf("updateMask = %q, want %q: a DAILY budget's amount lives in amount_micros", op.UpdateMask, "amount_micros")
	}
	if want := "customers/1234567890/campaignBudgets/555"; op.Update.ResourceName != want {
		t.Errorf("resourceName = %q, want %q: the write must address the budget the READ reported attached, not the campaign", op.Update.ResourceName, want)
	}
	if op.Update.AmountMicros == nil || *op.Update.AmountMicros != 25_500_000 {
		t.Errorf("amountMicros = %v, want 25500000", op.Update.AmountMicros)
	}
	if op.Update.TotalAmountMicros != nil {
		t.Errorf("totalAmountMicros = %v, want absent: the two amount fields are mutually exclusive upstream, and a row carrying both is one GetCampaignSettings refuses to read back at all", *op.Update.TotalAmountMicros)
	}
}

// TestGoogleAds_WriteBudget_Lifetime is the other arm of the field selection. It is a separate
// test rather than a table row because getting it wrong is not a smaller version of the same
// bug: writing amount_micros onto a CUSTOM_PERIOD budget leaves a self-contradictory row.
func TestGoogleAds_WriteBudget_Lifetime(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","totalAmountMicros":"90000000","period":"CUSTOM_PERIOD","explicitlyShared":false}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 120, Type: model.BudgetLifetime})
	if err != nil {
		t.Fatalf("WriteBudget: %v", err)
	}

	calls := mutateCalls(requests())
	if len(calls) != 1 {
		t.Fatalf("got %d campaignBudgets:mutate calls, want exactly 1", len(calls))
	}
	if !strings.Contains(calls[0].Body, `"updateMask":"total_amount_micros"`) {
		t.Errorf("mutate did not carry the total_amount_micros mask: %s", calls[0].Body)
	}
	if strings.Contains(calls[0].Body, `"amountMicros"`) {
		t.Errorf("mutate carried amountMicros on a CUSTOM_PERIOD budget, which would set a daily amount alongside a whole-flight cap: %s", calls[0].Body)
	}
}

// TestGoogleAds_WriteBudget_SharedBudgetIsRefusedBeforeAnyMutate pins the guard the whole
// capability exists around. A shared budget written through one campaign moves the spend of
// every other campaign attached to it — including campaigns this service did not create and
// was never asked about.
func TestGoogleAds_WriteBudget_SharedBudgetIsRefusedBeforeAnyMutate(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":true}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetShared) {
		t.Fatalf("err = %v, want ErrBudgetShared", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_UnreadSharedFlagIsRefused is the absent-value half, and it is the
// defect this codebase keeps finding in new places: "we could not establish that this budget
// is private" read as "this budget is private". Here the cost of that reading is money.
func TestGoogleAds_WriteBudget_UnreadSharedFlagIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY"}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("err = %v, want ErrBudgetUnwritable: an unread explicitly_shared must be refused, not assumed false", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_MissingBudgetIDIsRefused: nothing to address. Note that a campaign
// row's stored CampaignBudgetID is deliberately NOT a fallback here — an adopted campaign has
// none and a UI-swapped budget leaves a stale one, and writing a stale id is the shared-budget
// failure wearing different clothes.
func TestGoogleAds_WriteBudget_MissingBudgetIDIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"amountMicros":"10000000","period":"DAILY","explicitlyShared":false}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("err = %v, want ErrBudgetUnwritable", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_MissingPeriodIsRefused: the period is what selects between the two
// mutually exclusive amount fields, so an unreadable one cannot be guessed past.
func TestGoogleAds_WriteBudget_MissingPeriodIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","explicitlyShared":false}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("err = %v, want ErrBudgetUnwritable", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_UnmappedPeriodIsRefused covers Google's UNKNOWN/UNSPECIFIED and
// any value added after this client's pinned API version — the case where the platform DID
// answer and the answer is one this service has no mapping for.
func TestGoogleAds_WriteBudget_UnmappedPeriodIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"UNKNOWN","explicitlyShared":false}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("err = %v, want ErrBudgetUnwritable", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_PacingMismatchIsRefusedNotTranslated pins the deliberate scope
// limit. A lifetime amount asked of a daily-paced campaign is REFUSED — not quietly written as
// a daily amount, and not used to switch the campaign's pacing. Switching a live campaign
// between daily pacing and a whole-flight cap reinterprets everything it has already spent
// against, and is not a thing to do as a side effect of an amount change.
func TestGoogleAds_WriteBudget_PacingMismatchIsRefusedNotTranslated(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":false}`))

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 900, Type: model.BudgetLifetime})
	if !errors.Is(err, domain.ErrBudgetUnwritable) {
		t.Fatalf("err = %v, want ErrBudgetUnwritable", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_AbsentProvenanceIsRefused: a row that does not record which
// customer it was created under cannot have its id resolved against the project's current
// connection. ReadSettings fails closed here because the cost is a wrong report; the cost on
// this path is changing another account's spend, so nothing about it may be relaxed.
func TestGoogleAds_WriteBudget_AbsentProvenanceIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":false}`))

	camp := budgetCampaign()
	camp.Result = nil

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown", err)
	}
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("err = %v, want it to ALSO carry ErrCampaignAccountMismatch so callers mapping the mismatch sentinel do not answer an absent provenance differently", err)
	}
	if len(requests()) != 0 {
		t.Errorf("absent provenance still contacted the platform: %+v — the guard must run before the client is resolved", requests())
	}
}

// TestGoogleAds_WriteBudget_CustomerMismatchIsRefused is the second provenance arm: the row
// records a customer, and it is not the one the project's connection resolves to. The campaign
// id would resolve — to a DIFFERENT campaign, in a different account.
func TestGoogleAds_WriteBudget_CustomerMismatchIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":false}`))

	camp := budgetCampaign()
	camp.Result = []byte(`{"customerId":"9876543210"}`)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want ErrCampaignAccountMismatch", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_AbsentPlatformCampaignIsRefused: the platform answered and holds no
// such campaign. There is nothing to write to, and the readback reports it the same way.
func TestGoogleAds_WriteBudget_AbsentPlatformCampaignIsRefused(t *testing.T) {
	d, requests := budgetDispatcher(t, `{"results":[]}`)

	err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
		model.BudgetChange{Amount: 25.50, Type: model.BudgetDaily})
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("err = %v, want ErrPlatformCampaignAbsent", err)
	}
	assertNoMutate(t, requests())
}

// TestGoogleAds_WriteBudget_NonPositiveAmountIsRefused proves the write path converts through
// the SAME validation the create path uses. Without the shared helper an amount this service
// refuses to CREATE with would be reachable by editing — and a zero is accepted by Google as a
// real instruction to stop the campaign spending.
func TestGoogleAds_WriteBudget_NonPositiveAmountIsRefused(t *testing.T) {
	for _, amount := range []float64{0, -5, 0.0000001} {
		d, requests := budgetDispatcher(t, budgetSearchJSON(`{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":false}`))

		err := d.WriteBudget(context.Background(), "proj", model.ProviderGoogleAds, budgetCampaign(),
			model.BudgetChange{Amount: amount, Type: model.BudgetDaily})
		if err == nil {
			t.Errorf("amount %v was accepted; want a refusal", amount)
		}
		assertNoMutate(t, requests())
	}
}
