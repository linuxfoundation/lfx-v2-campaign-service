// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// newCampaignClient wires a token server + an API server whose budget/campaign
// mutate handlers are supplied per-test. adGroups:mutate/adGroupAds:mutate get
// default "happy" handlers (okAdGroup/okAdGroupAd) unless the test needs to
// exercise the ad-group/ad cascade itself — see newCampaignClientFull.
func newCampaignClient(t *testing.T, budgetH, campaignH http.HandlerFunc) *Client {
	t.Helper()
	return newCampaignClientFull(t, budgetH, campaignH, okAdGroup, okAdGroupAd)
}

// newCampaignClientFull is newCampaignClient with the ad-group/ad mutate handlers
// also supplied per-test, for tests that exercise the GA-3 cascade itself.
func newCampaignClientFull(t *testing.T, budgetH, campaignH, adGroupH, adGroupAdH http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			budgetH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			campaignH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			adGroupH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			adGroupAdH(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

func okBudget(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
}
func okCampaign(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
}
func okAdGroup(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
}
func okAdGroupAd(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
}

func gaqlError(status int, category, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","details":[`+
			`{"@type":"type.googleapis.com/google.ads.googleads.v23.errors.GoogleAdsFailure",`+
			`"errors":[{"errorCode":{"`+category+`":"`+code+`"},"message":"boom"}]}]}}`)
	}
}

func sampleInput() CampaignInput {
	return CampaignInput{
		EventName:       "KubeCon",
		Project:         "CNCF",
		Budget:          50,
		NameSuffix:      "brief-1",
		RegistrationURL: "https://events.linuxfoundation.org/kubecon",
	}
}

func TestCreateCampaign_HappyPath(t *testing.T) {
	var budgetBody, campaignBody map[string]any
	c := newCampaignClient(t,
		func(w http.ResponseWriter, r *http.Request) { budgetBody = decode(t, r); okBudget(w, r) },
		func(w http.ResponseWriter, r *http.Request) { campaignBody = decode(t, r); okCampaign(w, r) },
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignBudgetID != "111" || res.CampaignID != "222" {
		t.Errorf("ids = budget %q / campaign %q, want 111 / 222", res.CampaignBudgetID, res.CampaignID)
	}
	if res.Platform != "google-ads" {
		t.Errorf("platform = %q", res.Platform)
	}
	// The account the campaign was created under is part of the result, not just the
	// console URL: campaign ids are unique only within a customer, so a later
	// account-scoped read has to be able to confirm it is talking to the same one.
	if res.CustomerID != "1234567890" {
		t.Errorf("CustomerID = %q, want the creating account 1234567890", res.CustomerID)
	}
	// Budget body assertions: micros conversion + non-shared + STANDARD.
	op := firstCreate(t, budgetBody)
	if op["amountMicros"] != float64(50*microsPerUnit) {
		t.Errorf("amountMicros = %v, want %d", op["amountMicros"], 50*microsPerUnit)
	}
	if op["deliveryMethod"] != "STANDARD" {
		t.Errorf("deliveryMethod = %v", op["deliveryMethod"])
	}
	if op["explicitlyShared"] != false {
		t.Errorf("explicitlyShared = %v, want false", op["explicitlyShared"])
	}
	// Campaign body: PAUSED, SEARCH, references the budget resourceName, manualCpc.
	cop := firstCreate(t, campaignBody)
	if cop["status"] != "PAUSED" || cop["advertisingChannelType"] != "SEARCH" {
		t.Errorf("campaign status/channel = %v / %v", cop["status"], cop["advertisingChannelType"])
	}
	if cop["campaignBudget"] != "customers/1234567890/campaignBudgets/111" {
		t.Errorf("campaignBudget = %v", cop["campaignBudget"])
	}
	if _, ok := cop["manualCpc"]; !ok {
		t.Error("campaign create must carry a manualCpc bidding strategy")
	}
	// v23 requires the EU political-advertising declaration on every create.
	if cop["containsEuPoliticalAdvertising"] != "DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING" {
		t.Errorf("containsEuPoliticalAdvertising = %v, want DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING", cop["containsEuPoliticalAdvertising"])
	}
	// v23 SEARCH create must target at least one network (else
	// CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK after the budget commits). We target
	// Google Search only; the other flags are sent explicitly false.
	ns, ok := cop["networkSettings"].(map[string]any)
	if !ok {
		t.Fatalf("campaign create must carry networkSettings, got %v", cop["networkSettings"])
	}
	if ns["targetGoogleSearch"] != true {
		t.Errorf("networkSettings.targetGoogleSearch = %v, want true", ns["targetGoogleSearch"])
	}
	if ns["targetSearchNetwork"] != false || ns["targetContentNetwork"] != false {
		t.Errorf("networkSettings search/content = %v / %v, want false / false", ns["targetSearchNetwork"], ns["targetContentNetwork"])
	}
}

func TestCreateCampaign_BudgetMicrosRounds(t *testing.T) {
	// float64(2.01)*1e6 == 2009999.99…; a truncating int64() would drop a micro and
	// send 2009999. The conversion must round to 2010000.
	var budgetBody map[string]any
	c := newCampaignClient(t,
		func(w http.ResponseWriter, r *http.Request) { budgetBody = decode(t, r); okBudget(w, r) },
		okCampaign,
	)
	in := sampleInput()
	in.Budget = 2.01
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got := firstCreate(t, budgetBody)["amountMicros"]; got != float64(2010000) {
		t.Errorf("amountMicros = %v, want 2010000 (rounded, not truncated)", got)
	}
}

func TestCreateCampaign_Campaign429IsUnconfirmed(t *testing.T) {
	c := newCampaignClient(t, okBudget,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a mutating 429 must be UNCONFIRMED (doRequest suppresses its retry because it may have committed), got: %v", err)
	}
	if res == nil || res.CampaignBudgetID != "111" {
		t.Fatalf("partial must carry the created budget id, got %+v", res)
	}
}

func TestCreateCampaign_CampaignDuplicateNameIsUnconfirmedExists(t *testing.T) {
	c := newCampaignClient(t, okBudget,
		gaqlError(http.StatusBadRequest, "campaignError", "DUPLICATE_CAMPAIGN_NAME"),
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on DUPLICATE_CAMPAIGN_NAME")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("DUPLICATE_CAMPAIGN_NAME must read as already-exists, got: %v", err)
	}
	if res == nil || res.CampaignBudgetID != "111" {
		t.Fatalf("partial must carry the created budget id, got %+v", res)
	}
}

func TestCreateCampaign_BudgetAmbiguous5xxIsUnconfirmed(t *testing.T) {
	c := newCampaignClient(t,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted after budget 5xx")
			okCampaign(w, nil)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on budget 5xx")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("budget 5xx must be UNCONFIRMED, got: %v", err)
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a name-carrying partial, got %+v", res)
	}
	// The possibly-orphaned resource is a BUDGET, so the partial must carry the budget
	// NAME (not just the campaign name) — that's how a caller reconciles it (no id yet).
	if res.CampaignBudgetName == "" {
		t.Error("budget-ambiguous partial must carry CampaignBudgetName for reconcile")
	}
	if res.CampaignBudgetID != "" {
		t.Errorf("budget id must be empty (never confirmed), got %q", res.CampaignBudgetID)
	}
	// A partial is exactly when knowing the account matters most: reconciling by name
	// means knowing which Google Ads account to look in.
	if res.CustomerID != "1234567890" {
		t.Errorf("partial must carry the creating CustomerID, got %q", res.CustomerID)
	}
}

// A context ALREADY cancelled before the first mutate — with the OAuth token already
// cached, so the cancellation isn't caught during a token fetch — must return a clean
// (nil, err), NOT an UNCONFIRMED partial: nothing was sent, so the budget can't exist.
func TestCreateCampaign_PreSendCancelledWithCachedTokenIsCleanFailure(t *testing.T) {
	// `armed` flips true after the warm-up create; while armed, NEITHER mutate handler
	// may be hit (the cancelled call must reach no endpoint).
	var armed atomic.Bool
	guard := func(next http.HandlerFunc, name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if armed.Load() {
				t.Errorf("%s must not be attempted on an already-cancelled context", name)
			}
			next(w, r)
		}
	}
	c := newCampaignClient(t, guard(okBudget, "budget mutate"), guard(okCampaign, "campaign mutate"))
	// Warm the token cache with one successful create, so the cancelled call below
	// reaches httpClient.Do directly (no token fetch to catch the ctx error first).
	if _, err := c.CreateCampaign(context.Background(), sampleInput()); err != nil {
		t.Fatalf("warm-up create failed: %v", err)
	}
	armed.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled BEFORE the call — nothing can be sent
	res, err := c.CreateCampaign(ctx, sampleInput())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("a pre-send cancelled context must return a context.Canceled error, got: %v", err)
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a pre-send failure must NOT be UNCONFIRMED (nothing was sent): %v", err)
	}
	if res != nil {
		t.Errorf("a clean pre-send failure returns nil result, got %+v", res)
	}
}

func TestCreateCampaign_BudgetDefinite4xxIsCleanFailure(t *testing.T) {
	c := newCampaignClient(t,
		gaqlError(http.StatusBadRequest, "campaignBudgetError", "INVALID_BUDGET_AMOUNT"),
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted after budget 4xx")
			okCampaign(w, nil)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on budget 4xx")
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a definite 4xx must NOT be UNCONFIRMED: %v", err)
	}
	if res != nil {
		t.Errorf("a clean pre-budget failure returns nil result, got %+v", res)
	}
}

func TestCreateCampaign_BudgetDuplicateNameIsUnconfirmedExists(t *testing.T) {
	c := newCampaignClient(t,
		gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME"),
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted")
			okCampaign(w, nil)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on DUPLICATE_NAME")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("DUPLICATE_NAME must read as already-exists, got: %v", err)
	}
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a name-carrying partial for reconcile, got %+v", res)
	}
}

func TestCreateCampaign_Budget2xxNoResourceNameIsUnconfirmed(t *testing.T) {
	c := newCampaignClient(t,
		func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"results":[]}`) },
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted")
			okCampaign(w, nil)
		},
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("budget 2xx-with-no-resource-name must be UNCONFIRMED, got: %v", err)
	}
}

// armAfterBudgetCtx is a context wrapper whose Err() reports Canceled only once
// armed, and — crucially — Done() always returns nil so the HTTP transport never
// aborts the in-flight budget request/response on it (Err() being non-nil does not,
// by itself, cancel a request; the transport keys on Done()). This lets the budget
// mutate complete cleanly (id 111) and then makes the client's own ctx.Err() check
// BETWEEN the two mutates observe the cancellation — deterministically exercising the
// post-budget cancellation branch without a data race or a raced in-flight abort.
type armAfterBudgetCtx struct {
	context.Context
	armed *atomic.Bool
}

func (c armAfterBudgetCtx) Err() error {
	if c.armed.Load() {
		return context.Canceled
	}
	return nil
}
func (c armAfterBudgetCtx) Done() <-chan struct{} { return nil }

// If the context is cancelled AFTER the budget is created but BEFORE the campaign
// mutate, the campaign create must be skipped (a done context would fail it anyway)
// and the created budget returned as a reconcilable partial — so a retry reconciles
// the orphan budget by name instead of firing on a dead context.
func TestCreateCampaign_CtxCancelledAfterBudgetKeepsBudgetPartial(t *testing.T) {
	var armed atomic.Bool
	ctx := armAfterBudgetCtx{Context: context.Background(), armed: &armed}
	c := newCampaignClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			okBudget(w, r)    // budget succeeds cleanly → id 111
			armed.Store(true) // now the caller's context reads as cancelled
		},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must NOT be attempted after the context is cancelled")
			okCampaign(w, nil)
		},
	)
	res, err := c.CreateCampaign(ctx, sampleInput())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("expected a context.Canceled error, got: %v", err)
	}
	// The created budget must be reconcilable in the partial.
	if res == nil || res.CampaignBudgetID != "111" {
		t.Fatalf("partial must carry the created budget id 111, got %+v", res)
	}
	if res.CampaignID != "" {
		t.Errorf("campaign id must be empty (never attempted), got %q", res.CampaignID)
	}
}

func TestCreateCampaign_CampaignAmbiguous5xxKeepsBudgetPartial(t *testing.T) {
	c := newCampaignClient(t, okBudget,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("campaign 5xx must be UNCONFIRMED, got: %v", err)
	}
	// The already-created budget must be reconcilable in the partial result.
	if res == nil || res.CampaignBudgetID != "111" {
		t.Fatalf("partial must carry the created budget id 111, got %+v", res)
	}
	if res.CampaignID != "" {
		t.Errorf("campaign id must be empty (never confirmed), got %q", res.CampaignID)
	}
}

func TestCreateCampaign_CampaignDefinite4xxKeepsBudgetPartial(t *testing.T) {
	c := newCampaignClient(t, okBudget,
		gaqlError(http.StatusBadRequest, "campaignError", "INCOMPATIBLE_BIDDING_STRATEGY"),
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil {
		t.Fatal("expected an error on campaign 4xx")
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("campaign definite 4xx must NOT be UNCONFIRMED: %v", err)
	}
	// Budget still succeeded, so the partial must carry it (orphan reconcilable).
	if res == nil || res.CampaignBudgetID != "111" {
		t.Fatalf("partial must carry the created budget id, got %+v", res)
	}
}

func TestCreateCampaign_RejectsBadInput(t *testing.T) {
	c := newCampaignClient(t,
		func(w http.ResponseWriter, _ *http.Request) { t.Error("no call expected"); okBudget(w, nil) },
		func(w http.ResponseWriter, _ *http.Request) { t.Error("no call expected"); okCampaign(w, nil) },
	)
	// Bad budgets: zero, negative, over-max, NaN, ±Inf, and a sub-micro value that
	// rounds to 0 amountMicros. All must be rejected BEFORE any :mutate call.
	// (Project+EventName are set so we exercise the budget checks, not the
	// attribution checks that run first.)
	for _, b := range []float64{0, -5, maxBudget + 1, math.NaN(), math.Inf(1), math.Inf(-1), 0.0000001} {
		if _, err := c.CreateCampaign(context.Background(), CampaignInput{Project: "P", EventName: "E", Budget: b}); err == nil {
			t.Errorf("budget %v should be rejected before any call", b)
		}
	}
	// Both attribution fields are required INDEPENDENTLY: a missing Project OR a
	// missing EventName must be rejected before any :mutate call (a campaign with
	// only one segment is mis-attributed by the name-parsing data pipeline).
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{EventName: "E", Budget: 50}); err == nil {
		t.Error("a campaign with no Project should be rejected")
	}
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{Project: "P", Budget: 50}); err == nil {
		t.Error("a campaign with no EventName should be rejected")
	}
	// A delimiter-only value ("|||") is non-empty raw but sanitizes to "", which would
	// drop the segment from the composed name — must be rejected like an empty field.
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{Project: "|||", EventName: "E", Budget: 50}); err == nil {
		t.Error("a pipe-only Project (sanitizes to empty) should be rejected")
	}
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{Project: "P", EventName: " | ", Budget: 50}); err == nil {
		t.Error("a pipe-only EventName (sanitizes to empty) should be rejected")
	}
}

// TestCreateCampaign_RejectsBadAdGroupAdInputBeforeAnyMutate confirms the
// ad-group/ad inputs (destination URL, ad copy, ad-group name) are validated
// BEFORE the first (budget) mutate — a bad RegistrationURL must not leave an
// orphaned budget+campaign committed in Google Ads for what is purely a local
// input-validation failure.
func TestCreateCampaign_RejectsBadAdGroupAdInputBeforeAnyMutate(t *testing.T) {
	c := newCampaignClient(t,
		func(w http.ResponseWriter, _ *http.Request) { t.Error("no budget call expected"); okBudget(w, nil) },
		func(w http.ResponseWriter, _ *http.Request) { t.Error("no campaign call expected"); okCampaign(w, nil) },
	)
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		Project:         "P",
		EventName:       "E",
		Budget:          50,
		RegistrationURL: "not-a-valid-url",
	})
	if err == nil {
		t.Fatal("an invalid RegistrationURL should be rejected before any mutate call")
	}
	if res != nil {
		t.Errorf("a preflight ad-group/ad input rejection must return a nil result (nothing was created), got %+v", res)
	}
}

// --- unit tests for the pure helpers ---

func TestResourceID(t *testing.T) {
	cases := map[string]string{
		"customers/123/campaigns/456":    "456",
		"customers/1/campaignBudgets/99": "99",
		"":                               "",
		"noslash":                        "",
		"customers/123/campaigns/":       "",
	}
	for in, want := range cases {
		if got := resourceID(in); got != want {
			t.Errorf("resourceID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseErrorCodes(t *testing.T) {
	body := []byte(`{"error":{"details":[{"@type":"x.GoogleAdsFailure","errors":[` +
		`{"errorCode":{"fieldError":"REQUIRED"}},{"errorCode":{"campaignBudgetError":"DUPLICATE_NAME"}}]}]}}`)
	codes := parseErrorCodes(body)
	if len(codes) != 2 || codes[0] != "REQUIRED" || codes[1] != "DUPLICATE_NAME" {
		t.Fatalf("codes = %v, want [REQUIRED DUPLICATE_NAME]", codes)
	}
	// Non-GoogleAdsFailure detail is ignored; malformed body -> nil.
	if got := parseErrorCodes([]byte(`{"error":{"details":[{"@type":"other","errors":[{"errorCode":{"x":"Y"}}]}]}}`)); got != nil {
		t.Errorf("non-GoogleAdsFailure detail must be ignored, got %v", got)
	}
	if got := parseErrorCodes([]byte(`not json`)); got != nil {
		t.Errorf("malformed body must yield nil, got %v", got)
	}
}

func TestParseErrorCodes_BoundsHostileBody(t *testing.T) {
	long := strings.Repeat("A", maxErrorCodeLen+1)
	body := []byte(`{"error":{"details":[{"@type":"x.GoogleAdsFailure","errors":[` +
		`{"errorCode":{"a":"` + long + `"}},{"errorCode":{"b":"DUPLICATE_NAME"}}]}]}}`)
	codes := parseErrorCodes(body)
	if len(codes) != 1 || codes[0] != "DUPLICATE_NAME" {
		t.Errorf("over-long code must be dropped, got %v", codes)
	}
}

// End-to-end regression for the full-body-before-truncation parse: doRequest parses
// error codes from the RAW body and only THEN truncates apiError.Body to
// maxErrorBodyChars. A real Google error JSON exceeds that bound, so if the codes
// were re-parsed from the truncated Body the duplicate code (placed here AFTER the
// bound) would be lost and duplicate detection would silently break. This drives a
// >maxErrorBodyChars body through doRequest via CreateCampaign and asserts the
// DUPLICATE_NAME is still detected (surfaces as already-exists), not misclassified.
func TestCreateCampaign_DuplicateCodeAfterTruncationBoundStillDetected(t *testing.T) {
	// Pad the message so the errorCode object lands well past maxErrorBodyChars.
	pad := strings.Repeat("x", maxErrorBodyChars*2)
	dupAfterBound := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"`+pad+`","details":[`+
			`{"@type":"type.googleapis.com/google.ads.googleads.v23.errors.GoogleAdsFailure",`+
			`"errors":[{"errorCode":{"campaignBudgetError":"DUPLICATE_NAME"},"message":"dup"}]}]}}`)
	}
	c := newCampaignClient(t, dupAfterBound,
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted")
			okCampaign(w, nil)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("DUPLICATE_NAME past the truncation bound must still read as already-exists, got: %v", err)
	}
	// Budget duplicate → name-carrying partial for name-based reconcile.
	if res == nil || res.CampaignName == "" {
		t.Fatalf("expected a name-carrying partial, got %+v", res)
	}
}

func TestCreateOutcomeAmbiguous_GoogleAds(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"5xx-POST", &apiError{StatusCode: 500, Method: http.MethodPost}, true},
		{"5xx-GET", &apiError{StatusCode: 503, Method: http.MethodGet}, true},
		{"3xx-POST", &apiError{StatusCode: 302, Method: http.MethodPost}, true},
		{"3xx-GET-not-a-create", &apiError{StatusCode: 302, Method: http.MethodGet}, false},
		{"400", &apiError{StatusCode: 400, Method: http.MethodPost}, false},
		{"429-mutating-is-ambiguous", &apiError{StatusCode: 429, Method: http.MethodPost}, true},
		{"transport", &transportError{Method: http.MethodPost, Err: io.ErrUnexpectedEOF}, true},
		{"plain", errors.New("x"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := createOutcomeAmbiguous(tc.err); got != tc.want {
			t.Errorf("%s: createOutcomeAmbiguous = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsDuplicateNameErr_GatedTo4xxAndFamily(t *testing.T) {
	budgetDup := apiError{StatusCode: 400, ErrorCodes: []string{"DUPLICATE_NAME"}}
	campaignDup := apiError{StatusCode: 400, ErrorCodes: []string{"DUPLICATE_CAMPAIGN_NAME"}}

	// Budget check matches the budget code, NOT the campaign code (different codes).
	if !isDuplicateBudgetNameErr(&budgetDup) {
		t.Error("400 CampaignBudgetError.DUPLICATE_NAME must match isDuplicateBudgetNameErr")
	}
	if isDuplicateBudgetNameErr(&campaignDup) {
		t.Error("DUPLICATE_CAMPAIGN_NAME must NOT match the budget check")
	}
	// Campaign check matches the campaign code, NOT the budget code.
	if !isDuplicateCampaignNameErr(&campaignDup) {
		t.Error("400 CampaignError.DUPLICATE_CAMPAIGN_NAME must match isDuplicateCampaignNameErr")
	}
	if isDuplicateCampaignNameErr(&budgetDup) {
		t.Error("budget DUPLICATE_NAME must NOT match the campaign check")
	}
	// A 5xx carrying either code must NOT be a known duplicate (stays ambiguous).
	if isDuplicateBudgetNameErr(&apiError{StatusCode: 503, ErrorCodes: []string{"DUPLICATE_NAME"}}) {
		t.Error("5xx DUPLICATE_NAME must NOT be treated as a known duplicate")
	}
	// A 429 carrying either code must NOT be a known duplicate: createOutcomeAmbiguous
	// classifies a mutating 429 as possibly-committed, so the throttled request itself
	// may be the one that created the resource — reading "already exists" would skip
	// the required reconcile. (The duplicate predicates run before the ambiguity check
	// on the create path, so the exclusion must live here.)
	if isDuplicateBudgetNameErr(&apiError{StatusCode: 429, ErrorCodes: []string{"DUPLICATE_NAME"}}) {
		t.Error("429 DUPLICATE_NAME must NOT be treated as a known duplicate (it is ambiguous)")
	}
	if isDuplicateCampaignNameErr(&apiError{StatusCode: 429, ErrorCodes: []string{"DUPLICATE_CAMPAIGN_NAME"}}) {
		t.Error("429 DUPLICATE_CAMPAIGN_NAME must NOT be treated as a known duplicate (it is ambiguous)")
	}
}

func TestComposeName_DeterministicAndBounded(t *testing.T) {
	in := CampaignInput{EventName: " KubeCon ", Project: " CNCF ", NameSuffix: " brief-1 "}
	got := ComposeName("Budget", in)
	if got != "LFX | Budget | CNCF | KubeCon | brief-1" {
		t.Errorf("ComposeName = %q", got)
	}
	// Stable across calls (deterministic → retry collides on DUPLICATE_NAME).
	if ComposeName("Budget", in) != got {
		t.Error("ComposeName must be deterministic")
	}
}

// A raw "|" in a caller-supplied segment must be stripped, not passed through:
// otherwise it injects extra pipe-delimited fields into the composed name and
// breaks the name-based attribution / reconciliation that splits on "|".
func TestComposeName_StripsPipeInjection(t *testing.T) {
	in := CampaignInput{Project: "A | B", EventName: "C||D", NameSuffix: "e"}
	got := ComposeName("Budget", in)
	if got != "LFX | Budget | A B | C D | e" {
		t.Errorf("composeName must strip injected pipes, got %q", got)
	}
}

func TestSanitizeNamePart(t *testing.T) {
	cases := map[string]string{
		"  hello  ":  "hello",
		"a | b":      "a b",
		"a||b":       "a b",
		"a  b\tc":    "a b c",
		"|leading":   "leading",
		"trailing|":  "trailing",
		"":           "",
		"   ":        "",
		"a\x00b":     "a b", // NUL (v23 forbids it in a name) → space, then collapsed
		"a\x00\x00b": "a b", // runs of control chars collapse to one space
		"\x00lead":   "lead",
		"trail\x00":  "trail",
		"a\x1bb":     "a b", // ESC (another control char) → space
		"\x00":       "",    // a lone NUL sanitizes to empty (rejected upstream)
	}
	for in, want := range cases {
		if got := sanitizeNamePart(in); got != want {
			t.Errorf("sanitizeNamePart(%q) = %q, want %q", in, got, want)
		}
	}
}

// An over-length composed name must be rejected BEFORE any :mutate call. Google Ads
// v23 limits CampaignBudget.name to 255 UTF-8 bytes and Campaign.name to 256
// characters; the budget name is composed+validated first, so for an ASCII name its
// 255-byte cap is the binding preflight guard. This asserts an oversized name never
// reaches a paid mutate (which would otherwise create nothing but waste a call, or —
// if only one side were checked — orphan a budget).
func TestCreateCampaign_OversizedNameRejectedPreflight(t *testing.T) {
	c := newCampaignClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("budget must not be attempted")
			okBudget(w, nil)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted")
			okCampaign(w, nil)
		},
	)
	// A ~300-char ASCII EventName makes both composed names exceed their caps; the
	// budget's 255-byte limit is checked first.
	in := CampaignInput{Project: "CNCF", EventName: strings.Repeat("x", 300), Budget: 50}
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "name exceeds") {
		t.Errorf("oversized name must be rejected preflight, got: %v", err)
	}
}

// validateEntityName must measure in the UNIT it is told to: the budget in UTF-8
// bytes and the campaign in characters. A multibyte name is the discriminator — e.g.
// 200 two-byte runes is 400 bytes (over the 255-byte budget cap) but only 200
// characters (under the 256-char campaign cap). Guards against measuring the budget
// in characters (which would let a multibyte name slip past the API's byte ceiling).
func TestValidateEntityName_UnitsBytesVsRunes(t *testing.T) {
	multibyte := strings.Repeat("é", 200) // 200 runes, 400 UTF-8 bytes
	// Budget: measured in bytes -> 400 > 255 -> rejected.
	if err := validateEntityName("budget", multibyte, len(multibyte), maxBudgetNameBytes, "UTF-8 bytes"); err == nil {
		t.Error("a 400-byte budget name must be rejected (byte-measured)")
	}
	// Campaign: measured in runes -> 200 <= 256 -> accepted.
	if err := validateEntityName("campaign", multibyte, utf8.RuneCountInString(multibyte), maxCampaignNameRunes, "characters"); err != nil {
		t.Errorf("a 200-rune campaign name must be accepted (rune-measured), got: %v", err)
	}
	// A 257-rune campaign name is over the 256-char cap.
	over := strings.Repeat("a", 257)
	if err := validateEntityName("campaign", over, utf8.RuneCountInString(over), maxCampaignNameRunes, "characters"); err == nil {
		t.Error("a 257-char campaign name must be rejected")
	}
}

// A 2xx whose resourceName is present but MALFORMED (no trailing id segment)
// yields no reconcilable id, so it must be treated as UNCONFIRMED, not a
// confirmed create — at both the budget and campaign steps.
func TestCreateCampaign_MalformedBudgetResourceNameIsUnconfirmed(t *testing.T) {
	c := newCampaignClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			// resourceName with an empty id segment → resourceID() returns "".
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/"}]}`)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("campaign must not be attempted")
			okCampaign(w, nil)
		},
	)
	_, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("malformed budget resourceName must be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateCampaign_MalformedCampaignResourceNameIsUnconfirmed(t *testing.T) {
	c := newCampaignClient(t, okBudget,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"noslash"}]}`)
		},
	)
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("malformed campaign resourceName must be UNCONFIRMED, got: %v", err)
	}
	// Budget succeeded, so it must remain reconcilable in the partial.
	if res == nil || res.CampaignBudgetID != "111" {
		t.Fatalf("partial must carry the created budget id 111, got %+v", res)
	}
}

func TestFirstResourceName(t *testing.T) {
	rn, id, err := firstResourceName([]byte(`{"results":[{"resourceName":"customers/1/campaigns/222"}]}`))
	if err != nil || rn != "customers/1/campaigns/222" || id != "222" {
		t.Fatalf("valid: got (%q,%q,%v)", rn, id, err)
	}
	for _, body := range []string{
		`{`,                                 // malformed JSON
		`{"results":[]}`,                    // no results
		`{"results":[{"resourceName":""}]}`, // empty resourceName
		`{"results":[{"resourceName":"noslash"}]}`,                // no id segment
		`{"results":[{"resourceName":"customers/1/campaigns/"}]}`, // empty id segment
	} {
		if _, _, err := firstResourceName([]byte(body)); err == nil {
			t.Errorf("firstResourceName(%s) must error", body)
		}
	}
}

// decode reads a JSON request body into a map.
func decode(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	m, err := decodeRequest(r)
	if err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

// decodeRequest is a handler-safe decoder that returns an error instead of calling t.Fatal.
// Use this inside httptest.Server handler goroutines to avoid calling t.Fatalf/FailNow from
// a non-test goroutine, which is not safe. See test-hygiene.md:httptest-handler-state-needs-synchronized-handoff.
func decodeRequest(r *http.Request) (map[string]any, error) {
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// firstCreate returns operations[0].create from a decoded :mutate request body.
func firstCreate(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	ops, ok := body["operations"].([]any)
	if !ok || len(ops) == 0 {
		t.Fatalf("no operations in body: %v", body)
	}
	op, ok := ops[0].(map[string]any)
	if !ok {
		t.Fatalf("operation[0] not an object: %v", ops[0])
	}
	create, ok := op["create"].(map[string]any)
	if !ok {
		t.Fatalf("operation[0].create not an object: %v", op)
	}
	return create
}

// TestUpdateCampaignStatus_SendsUpdateMask verifies the mutate carries an UPDATE operation
// with an updateMask of "status" — and no create payload, since a :mutate operation must
// carry exactly one of create/update/remove.
func TestUpdateCampaignStatus_SendsUpdateMask(t *testing.T) {
	// Guarded: the handler runs on the server goroutine and the assertions read on the test
	// goroutine BEFORE the deferred Close() that would supply a happens-before edge.
	var mu sync.Mutex
	var body string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	if err := c.UpdateCampaignStatus(context.Background(), "222", StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignStatus: %v", err)
	}
	mu.Lock()
	got := body
	mu.Unlock()
	for _, want := range []string{`"updateMask":"status"`, `"status":"PAUSED"`, `campaigns/222`} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, `"create"`) {
		t.Errorf("an update operation must not carry a create: %s", got)
	}
}

// TestUpdateCampaignStatus_RejectsBadInput guards the input contract BEFORE any request. The
// campaign id interpolates into a resourceName, so a non-numeric id could alter the resource
// path — reject it rather than send it.
func TestUpdateCampaignStatus_RejectsBadInput(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	cases := []struct {
		name       string
		campaignID string
		status     string
	}{
		{"unsupported status", "222", "ACTIVE"}, // Google spells it ENABLED
		{"empty campaign id", "  ", StatusPaused},
		{"non-numeric campaign id", "222/../333", StatusPaused},
		{"campaign id with a query delimiter", "222?x=1", StatusPaused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			reached = false
			mu.Unlock()
			if err := c.UpdateCampaignStatus(context.Background(), tc.campaignID, tc.status); err == nil {
				t.Fatal("expected a rejection")
			}
			mu.Lock()
			sawCall := reached
			mu.Unlock()
			if sawCall {
				t.Error("no API call should be made — the guard is up front")
			}
		})
	}
}

// TestUpdateCampaignStatus_RetriesThrottle pins the idempotent=true choice, which is the
// OPPOSITE of the create path's. A status flip has no idempotency key problem — re-applying
// the same ENABLED/PAUSED converges on identical state — so a throttled attempt must be
// retried rather than surfaced as an avoidable UNCONFIRMED failure under ordinary Google
// throttling. Without this test, flipping the flag back to false would silently remove
// throttle resilience.
func TestUpdateCampaignStatus_RetriesThrottle(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	if err := c.UpdateCampaignStatus(context.Background(), "222", StatusPaused); err != nil {
		t.Fatalf("a throttled status update must be retried, not failed: %v", err)
	}
	mu.Lock()
	total := attempts
	mu.Unlock()
	if total < 2 {
		t.Errorf("attempts = %d, want >1 (the 429 must be retried; idempotent=false would abort)", total)
	}
}

// TestUpdateCampaignStatus_CancelDuringBackoffIsUnconfirmed pins the ambiguity of a
// cancellation that lands WHILE waiting to retry a 429. A request has already been sent (the
// 429 proves Google received it), so the outcome is AMBIGUOUS — not "not applied". Returning
// the bare context error would match neither transportError nor apiError, so the caller would
// be told the mutation definitely did not apply and would get no reconcile signal.
//
// This path is reachable in production without any user cancellation: the orchestrator wraps
// every toggle in context.WithTimeout(toggleCallTimeout), so a 429 followed by a deadline
// mid-backoff lands here.
func TestUpdateCampaignStatus_CancelDuringBackoffIsUnconfirmed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	var hits int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Retry-After", "2") // long enough to cancel mid-backoff
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	err := c.UpdateCampaignStatus(ctx, "222", StatusPaused)
	if err == nil {
		t.Fatal("expected an error when the context is cancelled during the retry backoff")
	}
	if hits == 0 {
		t.Fatal("fixture did not send a request, so there is no ambiguity to classify")
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("a cancellation AFTER a request was sent must stay UNCONFIRMED (verify before retry), got %T: %v", err, err)
	}
}

// TestUpdateCampaignStatus_AlreadyCanceledContextWithCachedToken exercises the path the
// dispatcher-level test cannot reach: a client whose OAuth token is ALREADY CACHED, with a
// context that is done before the call.
//
// This is the only case where the guard could matter. With a cold cache the token fetch
// surfaces the context error pre-send anyway; only with a warm cache does doRequest reach
// httpClient.Do, where an immediate context.Canceled would otherwise be wrapped as a
// transportError and misreported as an AMBIGUOUS upstream mutation. Priming here works because
// the same *Client is reused, unlike the dispatcher which builds a fresh client per call.
func TestUpdateCampaignStatus_AlreadyCanceledContextWithCachedToken(t *testing.T) {
	var mu sync.Mutex
	var apiCalls int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		apiCalls++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
	}))
	defer apiSrv.Close()

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	// Prime: this succeeds and leaves a valid token cached on THIS client.
	if err := c.UpdateCampaignStatus(context.Background(), "222", StatusPaused); err != nil {
		t.Fatalf("priming call should succeed: %v", err)
	}
	mu.Lock()
	primed := apiCalls
	mu.Unlock()
	if primed != 1 {
		t.Fatalf("priming call should have hit the API once, got %d", primed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.UpdateCampaignStatus(ctx, "222", StatusPaused)
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
	mu.Lock()
	after := apiCalls
	mu.Unlock()
	if after != primed {
		t.Errorf("no request may be sent on an already-done context, but api calls went %d -> %d", primed, after)
	}
	// Nothing was sent, so the outcome is NOT ambiguous — reporting it as such would tell the
	// caller to go verify a mutation that never left the process.
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("nothing was sent, so the outcome must not be UNCONFIRMED: %v", err)
	}
}

// TestValidateResourceKind_WrongCustomerRejected covers the account-ownership branch of
// validateResourceKind for every kind that reaches it: a 2xx mutate response naming a
// resource under a DIFFERENT customer must be rejected before the id is persisted.
//
// Without this, removing the `pathParts[1] != c.account.CustomerID` comparison would leave
// the suite green while the client persisted ids belonging to another Google Ads account —
// ids a later mutate would then address, or an operator would then chase in the wrong
// account. The existing negative cases only cover wrong KINDS and malformed composite ids,
// neither of which exercises this comparison.
func TestValidateResourceKind_WrongCustomerRejected(t *testing.T) {
	c := NewClient(testCreds(), testAccount(), WithClock(fixedClock()))
	const otherCustomer = "9999999999"

	cases := []struct {
		name             string
		kind             string
		resourceName     string
		requireNumericID bool
	}{
		{"campaign from another account", "campaigns", "customers/" + otherCustomer + "/campaigns/111", true},
		{"ad group from another account", "adGroups", "customers/" + otherCustomer + "/adGroups/222", true},
		{"ad group ad from another account", "adGroupAds", "customers/" + otherCustomer + "/adGroupAds/222~333", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.validateResourceKind(tc.kind, tc.resourceName, tc.requireNumericID)
			if err == nil {
				t.Fatalf("expected %s resource under customer %s to be rejected, got nil", tc.kind, otherCustomer)
			}
			if !strings.Contains(err.Error(), "different account") {
				t.Errorf("expected a different-account diagnostic, got: %v", err)
			}
			// The same shape under the CORRECT customer must still pass, so the test
			// fails on a broken ownership check rather than on an unrelated rejection.
			ok := strings.Replace(tc.resourceName, otherCustomer, testAccount().CustomerID, 1)
			if err := c.validateResourceKind(tc.kind, ok, tc.requireNumericID); err != nil {
				t.Errorf("expected %q to be accepted, got: %v", ok, err)
			}
		})
	}
}

// The Search half of the PRESENCE assertion — see the Demand Gen sibling in demandgen_test.go
// for why both channels need their own case. Location criteria alone do not restrict delivery:
// Google defaults positiveGeoTargetType to PRESENCE_OR_INTEREST, so a "US-targeted" campaign
// still serves to anyone worldwide showing interest in the US.
func TestCreateCampaign_RestrictsGeoToPresence(t *testing.T) {
	var body string
	c := newCampaignClient(t, okBudget, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		okCampaign(w, r)
	})

	if _, err := c.CreateCampaign(context.Background(), sampleInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !strings.Contains(body, `"positiveGeoTargetType":"PRESENCE"`) {
		t.Errorf("Search campaign create does not set PRESENCE — its location criteria would be read as PRESENCE_OR_INTEREST.\nbody=%s", body)
	}
}
