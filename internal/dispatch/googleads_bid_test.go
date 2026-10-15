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
)

// bidDispatcher wires a GoogleAdsDispatcher against a fake Google Ads API that answers the
// settings query with searchJSON and the ad group mutate with mutateStatus/mutateBody, recording
// every request. It is budgetDispatcher's shape with the mutate arm moved to adGroups:mutate and
// put under the test's control, because half of what this capability has to get right is which
// failures are classified unconfirmed.
func bidDispatcher(t *testing.T, searchJSON string, mutateStatus int, mutateBody string) (*GoogleAdsDispatcher, func() []budgetRequest) {
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
		if strings.Contains(r.URL.Path, "adGroups:mutate") {
			w.WriteHeader(mutateStatus)
			_, _ = io.WriteString(w, mutateBody)
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

// okBidMutate is an acknowledgement naming the ad group the happy-path row records.
const okBidMutate = `{"results":[{"resourceName":"customers/1234567890/adGroups/654"}]}`

// bidSearchJSON renders a settings-query answer reporting one bidding strategy, so each guard
// test can change that one fact and nothing else.
func bidSearchJSON(strategy string) string {
	bidding := ""
	if strategy != "" {
		bidding = `,"biddingStrategyType":"` + strategy + `"`
	}
	return `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"` + bidding + `},` +
		`"campaignBudget":{"id":"555","amountMicros":"10000000","period":"DAILY","explicitlyShared":false}}]}`
}

// bidCampaign is a campaign row recording the happy-path provenance and exactly ONE ad group.
func bidCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result: []byte(`{"customerId":"1234567890","adGroupId":"654","adGroups":[{"name":"g1","id":"654"}]}`),
	}
}

// adGroupMutateCalls returns just the adGroups:mutate requests out of a recording.
func adGroupMutateCalls(reqs []budgetRequest) []budgetRequest {
	var out []budgetRequest
	for _, r := range reqs {
		if strings.Contains(r.Path, "adGroups:mutate") {
			out = append(out, r)
		}
	}
	return out
}

// assertNoBidMutate is the assertion every refusal test shares: a guard that returns the right
// sentinel AFTER the bid was already written is not a guard.
func assertNoBidMutate(t *testing.T, reqs []budgetRequest) {
	t.Helper()
	if m := adGroupMutateCalls(reqs); len(m) != 0 {
		t.Fatalf("refusal still issued %d adGroups:mutate call(s): %+v — the caller is told nothing changed while the bid moved", len(m), m)
	}
}

// TestGoogleAds_WriteBid_ManualCPC is the happy path, and it asserts the WIRE: exactly one
// mutate, naming the recorded ad group, carrying cpc_bid_micros under a mask naming that field
// alone. MANUAL_CPC is also the strategy defaultBiddingStrategy picks for SEARCH, so this is the
// state a Search campaign this service created is actually in — unlike Reddit, Meta and X, where
// the equivalent write needs an operator to switch the strategy first.
func TestGoogleAds_WriteBid_ManualCPC(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if err != nil {
		t.Fatalf("WriteBid: %v", err)
	}

	calls := adGroupMutateCalls(requests())
	if len(calls) != 1 {
		t.Fatalf("got %d adGroups:mutate calls, want exactly 1: %+v", len(calls), calls)
	}
	var req struct {
		Operations []struct {
			UpdateMask string `json:"updateMask"`
			Update     struct {
				ResourceName string `json:"resourceName"`
				CpcBidMicros int64  `json:"cpcBidMicros"`
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
	if op.UpdateMask != "cpc_bid_micros" {
		t.Errorf("updateMask = %q, want cpc_bid_micros: this path must never touch anything else, least of all the bidding strategy", op.UpdateMask)
	}
	if want := "customers/1234567890/adGroups/654"; op.Update.ResourceName != want {
		t.Errorf("resourceName = %q, want %q: the write addresses the AD GROUP the create path recorded, not the campaign", op.Update.ResourceName, want)
	}
	if op.Update.CpcBidMicros != 1_250_000 {
		t.Errorf("cpcBidMicros = %d, want 1250000", op.Update.CpcBidMicros)
	}
}

// TestGoogleAds_WriteBid_SeveralAdGroupsAreRefused is the case Google is the FIRST platform to
// present. Sibling ad groups carry INDEPENDENT max CPC bids — 0.80 on one and 2.40 on another is
// a legitimate campaign — so a request naming only a campaign names no single bid to set.
// Writing to the first would silently leave the others; writing the same amount to all would
// flatten a deliberate spread nobody asked about.
func TestGoogleAds_WriteBid_SeveralAdGroupsAreRefused(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	camp := bidCampaign()
	camp.Result = []byte(`{"customerId":"1234567890","adGroupId":"654","adGroups":[{"name":"g1","id":"654"},{"name":"g2","id":"655"}]}`)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrBidUnwritable) {
		t.Fatalf("err = %v, want ErrBidUnwritable", err)
	}
	// The refusal must name both groups, and in a STABLE order: map iteration is randomised, so
	// without sortedKeys the message would vary between identical requests.
	for _, want := range []string{"654", "655"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name ad group %s, so the caller cannot act on it: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "654, 655") {
		t.Errorf("ad groups are not listed in a stable sorted order: %v", err)
	}
	assertNoBidMutate(t, requests())
}

// TestGoogleAds_WriteBid_NoAdGroupIsRefused: either the campaign was never provisioned, or it
// was ADOPTED, which records none. Picking one by listing the account's ad groups is a decision
// this endpoint does not make.
func TestGoogleAds_WriteBid_NoAdGroupIsRefused(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	camp := bidCampaign()
	camp.Result = []byte(`{"customerId":"1234567890"}`)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrBidUnwritable) {
		t.Fatalf("err = %v, want ErrBidUnwritable", err)
	}
	assertNoBidMutate(t, requests())
}

// TestGoogleAds_WriteBid_NonManualStrategyIsRefused pins the guard that makes the write honest.
// Under every automated strategy Google KEEPS an ad group's cpc_bid_micros and never bids it, so
// reporting the write as a bid change would be false. The empty case is the one that matters
// most: "we could not establish that this campaign bids manually" and "this campaign bids
// manually" are opposite facts, and only one of them justifies the write.
func TestGoogleAds_WriteBid_NonManualStrategyIsRefused(t *testing.T) {
	for _, strategy := range []string{
		"", // did not report it at all
		"TARGET_SPEND",
		"MAXIMIZE_CONVERSIONS",
		"TARGET_CPA",
		"TARGET_ROAS",
		// A portfolio (shared) strategy can never be MANUAL_CPC — Google does not offer it as
		// a portfolio type — so the same guard covers portfolios with no second query.
		"TARGET_IMPRESSION_SHARE",
	} {
		name := strategy
		if name == "" {
			name = "unreported"
		}
		t.Run(name, func(t *testing.T) {
			d, requests := bidDispatcher(t, bidSearchJSON(strategy), http.StatusOK, okBidMutate)

			err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
				model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
			if !errors.Is(err, domain.ErrBidUnwritable) {
				t.Fatalf("err = %v, want ErrBidUnwritable", err)
			}
			assertNoBidMutate(t, requests())
		})
	}
}

// TestGoogleAds_WriteBid_NonCPCBidTypeIsRefused: an ad group's cpc_bid_micros is a
// cost-per-click bid by definition, so a request naming another unit names nothing here. The
// unit is spelled as a literal because BidTypeCPC is the only constant model declares today —
// which is exactly why the guard has to exist before a second one is added.
func TestGoogleAds_WriteBid_NonCPCBidTypeIsRefused(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
		model.BidChange{Amount: 1.25, Type: model.BidType("cpm")})
	if !errors.Is(err, domain.ErrBidUnwritable) {
		t.Fatalf("err = %v, want ErrBidUnwritable", err)
	}
	if len(requests()) != 0 {
		t.Errorf("a unit mismatch still contacted the platform: %+v", requests())
	}
}

// TestGoogleAds_WriteBid_AmountOutOfBoundsIsRejected pins the mapping WriteBudget does not need.
// minCPCBid (0.01) and maxCPCBid (100,000) are the ADAPTER's bounds in the account currency, and
// the service layer knows neither — its own checks accept a wide band this client refuses.
// Without ErrBidAmountRejected each of those would fall through to a 503 and invite a retry that
// can never succeed.
func TestGoogleAds_WriteBid_AmountOutOfBoundsIsRejected(t *testing.T) {
	for _, amount := range []float64{0, -1, 0.009, 100_001} {
		d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

		err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
			model.BidChange{Amount: amount, Type: model.BidTypeCPC})
		if !errors.Is(err, domain.ErrBidAmountRejected) {
			t.Errorf("amount %v: err = %v, want ErrBidAmountRejected (a 400, not a retryable 503)", amount, err)
		}
		// Refused before any call, so the platform was never contacted at all — not merely
		// never mutated.
		if len(requests()) != 0 {
			t.Errorf("amount %v was refused only after contacting the platform: %+v", amount, requests())
		}
	}
}

// TestGoogleAds_WriteBid_AbsentProvenanceIsRefused: Google Ads is ONE customer shared across
// every foundation and ad group ids are bare numerics unique only within a customer, so a row
// that cannot prove which customer it was created under cannot prove the ad group it names is
// its own. The cost of being wrong here is changing a stranger's bid.
func TestGoogleAds_WriteBid_AbsentProvenanceIsRefused(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	camp := bidCampaign()
	camp.Result = nil

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown", err)
	}
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("err = %v, want it to ALSO carry ErrCampaignAccountMismatch so callers mapping the mismatch sentinel do not answer an absent provenance differently", err)
	}
	if len(requests()) != 0 {
		t.Errorf("absent provenance still contacted the platform: %+v", requests())
	}
}

// TestGoogleAds_WriteBid_CustomerMismatchIsRefused: the row records a customer, and it is not the
// one the project's connection resolves to. The ad group id would resolve — to a DIFFERENT ad
// group, in a different account.
func TestGoogleAds_WriteBid_CustomerMismatchIsRefused(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	camp := bidCampaign()
	camp.Result = []byte(`{"customerId":"9876543210","adGroups":[{"name":"g1","id":"654"}]}`)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want ErrCampaignAccountMismatch", err)
	}
	assertNoBidMutate(t, requests())
}

// TestGoogleAds_WriteBid_BadCampaignIDIsRefused: the recorded id is not a Google Ads campaign id,
// so there is nothing to address — refused before a credential is decrypted.
func TestGoogleAds_WriteBid_BadCampaignIDIsRefused(t *testing.T) {
	d, requests := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusOK, okBidMutate)

	camp := bidCampaign()
	camp.PlatformCampaignID = "77-7"

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, camp,
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrBidUnwritable) {
		t.Fatalf("err = %v, want ErrBidUnwritable", err)
	}
	if len(requests()) != 0 {
		t.Errorf("a malformed campaign id still contacted the platform: %+v", requests())
	}
}

// TestGoogleAds_WriteBid_AbsentPlatformCampaignIsRefused: the platform answered and holds no such
// campaign. There is nothing to write to.
func TestGoogleAds_WriteBid_AbsentPlatformCampaignIsRefused(t *testing.T) {
	d, requests := bidDispatcher(t, `{"results":[]}`, http.StatusOK, okBidMutate)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("err = %v, want ErrPlatformCampaignAbsent", err)
	}
	assertNoBidMutate(t, requests())
}

// TestGoogleAds_WriteBid_FailedStrategyReadIsNotUnconfirmed guards the same boundary WriteBudget
// draws: the READ changes nothing, so its failure — even an ambiguous 5xx — is definite.
// Wrapping it would report "the bid may have been applied" about a request that never reached
// adGroups:mutate at all.
func TestGoogleAds_WriteBid_FailedStrategyReadIsNotUnconfirmed(t *testing.T) {
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
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"backend error"}}`)
	}))
	t.Cleanup(apiSrv.Close)
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if err == nil {
		t.Fatal("WriteBid returned nil, want an error")
	}
	if isUnconfirmedWrite(err) {
		t.Fatalf("a failed settings READ was classified unconfirmed: %v — no mutate was built, so nothing can have been applied", err)
	}
	mu.Lock()
	defer mu.Unlock()
	assertNoBidMutate(t, seen)
}

// TestGoogleAds_WriteBid_AmbiguousMutateIsUnconfirmed pins the classification the service's 503
// arm depends on. Each case below is one Google ACCEPTED or may have accepted, so answering "the
// campaign was not modified" would be an affirmative false claim about a money-moving write.
func TestGoogleAds_WriteBid_AmbiguousMutateIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx from the mutate", http.StatusServiceUnavailable, `{"error":{"message":"backend error"}}`},
		{"2xx carrying no result", http.StatusOK, `{"results":[]}`},
		{"2xx naming a different ad group", http.StatusOK, `{"results":[{"resourceName":"customers/1234567890/adGroups/999"}]}`},
		{"2xx naming the right id in another account", http.StatusOK, `{"results":[{"resourceName":"customers/9999999999/adGroups/654"}]}`},
		{"2xx naming the right id on another resource kind", http.StatusOK, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/654"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), tc.status, tc.body)

			err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
				model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
			if err == nil {
				t.Fatal("WriteBid returned nil, want an error: nil is the caller's signal that the platform CONFIRMED the change")
			}
			if !isUnconfirmedWrite(err) {
				t.Fatalf("error does not report Unconfirmed(): %v — the service will answer 'the campaign was not modified' about a write that may have applied", err)
			}
		})
	}
}

// TestGoogleAds_WriteBid_DefiniteMutateFailureIsNotUnconfirmed is the other half: marking a
// definite 4xx unconfirmed would hold the claim lock through a cooldown and send the caller to
// verify a change Google explicitly rejected.
func TestGoogleAds_WriteBid_DefiniteMutateFailureIsNotUnconfirmed(t *testing.T) {
	d, _ := bidDispatcher(t, bidSearchJSON("MANUAL_CPC"), http.StatusBadRequest, `{"error":{"message":"invalid bid"}}`)

	err := d.WriteBid(context.Background(), "proj", model.ProviderGoogleAds, bidCampaign(),
		model.BidChange{Amount: 1.25, Type: model.BidTypeCPC})
	if err == nil {
		t.Fatal("WriteBid returned nil, want an error")
	}
	if isUnconfirmedWrite(err) {
		t.Fatalf("a 4xx from the mutate was classified unconfirmed: %v — Google refused it outright", err)
	}
}
