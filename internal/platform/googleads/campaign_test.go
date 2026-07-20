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
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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
	return CampaignInput{EventName: "KubeCon", Project: "CNCF", Budget: 50, NameSuffix: "brief-1"}
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
	// v23 requires the EU political-advertising declaration on every create.
	if cop["containsEuPoliticalAdvertising"] != "DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING" {
		t.Errorf("containsEuPoliticalAdvertising = %v, want DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING", cop["containsEuPoliticalAdvertising"])
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
	got := composeName("Budget", in)
	if got != "LFX | Budget | CNCF | KubeCon | brief-1" {
		t.Errorf("composeName = %q", got)
	}
	// Stable across calls (deterministic → retry collides on DUPLICATE_NAME).
	if composeName("Budget", in) != got {
		t.Error("composeName must be deterministic")
	}
}

// A raw "|" in a caller-supplied segment must be stripped, not passed through:
// otherwise it injects extra pipe-delimited fields into the composed name and
// breaks the name-based attribution / reconciliation that splits on "|".
func TestComposeName_StripsPipeInjection(t *testing.T) {
	in := CampaignInput{Project: "A | B", EventName: "C||D", NameSuffix: "e"}
	got := composeName("Budget", in)
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
