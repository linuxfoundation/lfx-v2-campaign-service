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
	"testing"
	"time"
)

// newCampaignClient wires a token server + an API server whose budget/campaign
// mutate handlers are supplied per-test.
func newCampaignClient(t *testing.T, budgetH, campaignH http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			budgetH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			campaignH(w, r)
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

func gaqlError(status int, category, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","details":[`+
			`{"@type":"type.googleapis.com/google.ads.googleads.v23.errors.GoogleAdsFailure",`+
			`"errors":[{"errorCode":{"`+category+`":"`+code+`"},"message":"boom"}]}]}}`)
	}
}

func sampleInput() CampaignInput {
	return CampaignInput{EventName: "KubeCon", Project: "CNCF", BudgetUSD: 50, NameSuffix: "brief-1"}
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
	if res.CampaignBudgetID != "" {
		t.Errorf("budget id must be empty (never confirmed), got %q", res.CampaignBudgetID)
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
	for _, b := range []float64{0, -5, maxBudgetUSD + 1, math.NaN(), math.Inf(1), math.Inf(-1), 0.0000001} {
		if _, err := c.CreateCampaign(context.Background(), CampaignInput{EventName: "E", BudgetUSD: b}); err == nil {
			t.Errorf("budget %v should be rejected before any call", b)
		}
	}
	// Missing attribution (no Project AND no EventName) must be rejected.
	if _, err := c.CreateCampaign(context.Background(), CampaignInput{BudgetUSD: 50, NameSuffix: "x"}); err == nil {
		t.Error("a campaign with no Project and no EventName should be rejected")
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
		{"429", &apiError{StatusCode: 429, Method: http.MethodPost}, false},
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

func TestIsDuplicateNameErr_GatedTo4xx(t *testing.T) {
	dup := `{"error":{"details":[{"@type":"x.GoogleAdsFailure","errors":[{"errorCode":{"campaignBudgetError":"DUPLICATE_NAME"}}]}]}}`
	if !isDuplicateNameErr(&apiError{StatusCode: 400, Body: dup}) {
		t.Error("400 DUPLICATE_NAME must match")
	}
	// A 5xx carrying the code must NOT be a known duplicate (stays ambiguous).
	if isDuplicateNameErr(&apiError{StatusCode: 503, Body: dup}) {
		t.Error("5xx DUPLICATE_NAME must NOT be treated as a known duplicate")
	}
}

func TestComposeName_DeterministicAndBounded(t *testing.T) {
	in := CampaignInput{EventName: " KubeCon ", Project: " CNCF ", NameSuffix: " brief-1 "}
	got := composeName("Budget", in)
	if got != "LFX | Budget | CNCF | KubeCon | brief-1" {
		t.Errorf("composeName = %q", got)
	}
	// Stable across calls (deterministic → retry collides on DUPLICATE_NAME).
	if composeName("Budget", in) != got {
		t.Error("composeName must be deterministic")
	}
}

// decode reads a JSON request body into a map.
func decode(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
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
