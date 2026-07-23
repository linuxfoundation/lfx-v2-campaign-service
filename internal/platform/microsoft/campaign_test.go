// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// validInput is a well-formed CampaignInput the create tests can start from.
func validInput() CampaignInput {
	return CampaignInput{
		EventName:  "KubeCon",
		EventSlug:  "kubecon",
		Project:    "CNCF",
		Budget:     50,
		NameSuffix: "brief-1",
	}
}

// campaignsAPI dispatches the two paths CreateCampaign touches: the POST
// Campaigns/QueryByAccountId find-by-name lookup and the POST Campaigns create.
// getBody/postBody/postStatus let each test script the two independently. postSeen /
// querySeen capture the decoded create / lookup bodies.
type campaignsAPI struct {
	getBody    string
	postBody   string
	postStatus int
	postSeen   *createCampaignsRequest
	querySeen  *queryCampaignsRequest
}

func (h *campaignsAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		// The lookup is a POST to .../Campaigns/QueryByAccountId — check it BEFORE the
		// plain create route (both are POST).
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns/QueryByAccountId"):
			if h.querySeen != nil {
				var req queryCampaignsRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode query body: %v", err)
				}
				*h.querySeen = req
			}
			body := h.getBody
			if body == "" {
				body = `{"Campaigns":[]}`
			}
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns"):
			if h.postSeen != nil {
				var req createCampaignsRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode create body: %v", err)
				}
				*h.postSeen = req
			}
			if h.postStatus != 0 {
				w.WriteHeader(h.postStatus)
			}
			body := h.postBody
			if body == "" {
				body = `{"CampaignIds":[321],"PartialErrors":[]}`
			}
			_, _ = io.WriteString(w, body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

// ---- validation ------------------------------------------------------------

func TestCreateCampaign_ValidationRejectsBadInput(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("validation failure must not send any request")
		w.WriteHeader(http.StatusInternalServerError)
	})
	cases := map[string]func(*CampaignInput){
		"empty project":     func(in *CampaignInput) { in.Project = "   " },
		"delimiter project": func(in *CampaignInput) { in.Project = "|||" },
		"empty event":       func(in *CampaignInput) { in.EventName = "" },
		"zero budget":       func(in *CampaignInput) { in.Budget = 0 },
		"negative budget":   func(in *CampaignInput) { in.Budget = -5 },
		"over-max budget":   func(in *CampaignInput) { in.Budget = maxBudget + 1 },
		"oversized name":    func(in *CampaignInput) { in.EventName = strings.Repeat("x", 200) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			mutate(&in)
			if _, err := c.CreateCampaign(context.Background(), in); err == nil {
				t.Fatalf("%s: expected a validation error, got nil", name)
			}
		})
	}
}

func TestCreateCampaign_NaNBudgetRejected(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("NaN budget must not send any request")
		w.WriteHeader(http.StatusInternalServerError)
	})
	in := validInput()
	in.Budget = nan()
	if _, err := c.CreateCampaign(context.Background(), in); err == nil {
		t.Fatal("expected a NaN-budget error, got nil")
	}
}

func nan() float64 { var z float64; return z / z } //nolint:staticcheck // intentional NaN for a test input

// ---- happy path + request shape --------------------------------------------

func TestCreateCampaign_CreatesPausedSearchCampaign(t *testing.T) {
	var seen createCampaignsRequest
	var query queryCampaignsRequest
	api := &campaignsAPI{postSeen: &seen, querySeen: &query}
	c := newAPIClient(t, api.handler(t))

	res, err := c.CreateCampaign(context.Background(), validInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if res.CampaignID != "321" {
		t.Errorf("CampaignID = %q, want 321", res.CampaignID)
	}
	if res.Platform != "microsoft-ads" {
		t.Errorf("Platform = %q, want microsoft-ads", res.Platform)
	}
	if res.AlreadyExisted {
		t.Error("AlreadyExisted = true, want false on a fresh create")
	}
	// The lookup POSTed the required AccountId + CampaignType in its body.
	if query.AccountId.String() != testAccount().AccountID {
		t.Errorf("query AccountId = %q, want the account id %q", query.AccountId, testAccount().AccountID)
	}
	if query.CampaignType != campaignTypeSearch {
		t.Errorf("query CampaignType = %q, want %q", query.CampaignType, campaignTypeSearch)
	}
	// The create body carries the required top-level AccountId (a sibling to Campaigns).
	if seen.AccountId.String() != testAccount().AccountID {
		t.Errorf("create AccountId = %q, want the account id %q", seen.AccountId, testAccount().AccountID)
	}
	if len(seen.Campaigns) != 1 {
		t.Fatalf("sent %d campaigns, want 1", len(seen.Campaigns))
	}
	got := seen.Campaigns[0]
	// TimeZone is deprecated but still Add:Required, so it must be present (defaulted).
	if got.TimeZone != defaultTimeZone {
		t.Errorf("TimeZone = %q, want the default %q (deprecated but Add:Required)", got.TimeZone, defaultTimeZone)
	}
	if got.Status != campaignStatusPaused {
		t.Errorf("Status = %q, want %q", got.Status, campaignStatusPaused)
	}
	if got.CampaignType != campaignTypeSearch {
		t.Errorf("CampaignType = %q, want %q", got.CampaignType, campaignTypeSearch)
	}
	if got.BudgetType != budgetTypeDailyStandard {
		t.Errorf("BudgetType = %q, want %q", got.BudgetType, budgetTypeDailyStandard)
	}
	// Budget is a plain decimal in account currency — NO micros conversion.
	if got.DailyBudget != 50 {
		t.Errorf("DailyBudget = %v, want 50 (no micros conversion)", got.DailyBudget)
	}
	if !strings.Contains(got.Name, "CNCF") || !strings.Contains(got.Name, "KubeCon") || !strings.Contains(got.Name, "brief-1") {
		t.Errorf("composed name %q missing a segment", got.Name)
	}
}

func TestCreateCampaign_LookupCancelIsCleanAbort(t *testing.T) {
	// The lookup (POST QueryByAccountId) is cancelled mid-flight: the handler signals it
	// has started and then blocks on a release channel; the caller cancels its context
	// while the request is in flight. Because the lookup creates nothing and the create
	// never runs, this is a clean (nil, err) abort — NOT an UNCONFIRMED reconcile-partial.
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Campaigns") { // the bare create route
			t.Error("create POST must not run when the lookup was cancelled")
		}
		select {
		case started <- struct{}{}:
		default: // a retry re-enters; only the first signals
		}
		// Hold the response open until the test releases us (closed right after the
		// assertions below, BEFORE the httptest server's own Close cleanup runs, so the
		// handler goroutine can't deadlock that Close).
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	res, err := c.CreateCampaign(ctx, validInput())
	close(release) // let the blocked handler return
	if err == nil {
		t.Fatal("expected a context error")
	}
	if res != nil {
		t.Errorf("a cancelled lookup is a clean abort (nil result), got %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got: %v", err)
	}
}

// ---- idempotency: find-or-create -------------------------------------------

func TestCreateCampaign_ReturnsExistingByNameWithoutCreating(t *testing.T) {
	in := validInput()
	name := composeName(in)
	// The lookup returns a campaign with the SAME deterministic name (different casing,
	// to exercise the case-insensitive match).
	api := &campaignsAPI{
		getBody:  `{"Campaigns":[{"Id":999,"Name":` + jsonString(strings.ToUpper(name)) + `}]}`,
		postBody: `{"CampaignIds":[888]}`, // create must NOT be reached
	}
	createReached := false
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The lookup is also a POST now; only the bare /Campaigns create must not fire.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns") {
			createReached = true
		}
		base(w, r)
	})

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if createReached {
		t.Error("create POST was issued despite an existing campaign by name (case-insensitive)")
	}
	if res.CampaignID != "999" {
		t.Errorf("CampaignID = %q, want 999 (existing)", res.CampaignID)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted = false, want true when returning an existing campaign")
	}
}

func TestCreateCampaign_LookupFailureIsUnconfirmed(t *testing.T) {
	// The lookup 500s. We cannot confirm the campaign is absent, so the result is
	// UNCONFIRMED (a name-only partial + error), NOT a clean failure.
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Campaigns") { // the bare create route
			t.Error("create POST must not run when the lookup failed")
		}
		// The lookup (QueryByAccountId) 500s.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"Errors":[{"ErrorCode":"InternalError"}]}`)
	})
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on lookup failure")
	}
	if res == nil {
		t.Fatal("expected a name-only partial result, got nil")
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty on an unconfirmed lookup", res.CampaignID)
	}
	if res.CampaignName == "" {
		t.Error("partial result should carry the deterministic name for reconciliation")
	}
}

// ---- PartialErrors-on-200 and malformed 200 --------------------------------

func TestCreateCampaign_PartialErrorOn200IsDefiniteFailure(t *testing.T) {
	// A 200 whose id slot is null and PartialErrors is present = definite rejection.
	api := &campaignsAPI{
		postBody: `{"CampaignIds":[null],"PartialErrors":[{"ErrorCode":"CampaignServiceInvalidDailyBudget"}]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected a definite rejection error")
	}
	// Definite failure ⇒ nil result (nothing was created, nothing to reconcile).
	if res != nil {
		t.Errorf("expected nil result on a definite PartialError rejection, got %+v", res)
	}
	if !strings.Contains(err.Error(), "CampaignServiceInvalidDailyBudget") {
		t.Errorf("error should surface the PartialError code, got: %v", err)
	}
}

func TestCreateCampaign_DuplicateNamePartialErrorIsAlreadyExists(t *testing.T) {
	// The pre-check lookup finds nothing, but the create loses a race (or the name was
	// created between lookup and create) → a duplicate-name PartialError. This is NOT a
	// clean failure: the campaign exists, so it is surfaced as already-exists with a
	// name-only partial for reconcile-by-name.
	api := &campaignsAPI{
		postBody: `{"CampaignIds":[null],"PartialErrors":[{"Code":1115,"ErrorCode":"CampaignServiceCannotCreateDuplicateCampaign"}]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected a duplicate-name error")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatal("expected a name-only partial for reconciliation")
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty (not created here)", res.CampaignID)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should read as already-exists, got: %v", err)
	}
}

func TestCreateCampaign_Malformed200IsUnconfirmed(t *testing.T) {
	// A 200 with no id and no PartialError is a malformed success: UNCONFIRMED.
	api := &campaignsAPI{postBody: `{"CampaignIds":[]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error on a malformed 200")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatal("expected a name-only partial for reconciliation")
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty when unconfirmed", res.CampaignID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error should be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateCampaign_MalformedCampaignIDIsUnconfirmed(t *testing.T) {
	// A 200 whose CampaignIds[0] is a non-positive-integer (here negative) is NOT a usable
	// id — firstCampaignID rejects it via numberID and the outcome is UNCONFIRMED, not a
	// bogus success carrying "-5".
	api := &campaignsAPI{postBody: `{"CampaignIds":[-5],"PartialErrors":[]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error on a malformed campaign id")
	}
	if res == nil || res.CampaignID != "" {
		t.Fatalf("expected a name-only partial with no id, got %+v", res)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error should be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateCampaign_NullPartialErrorIsUnconfirmed(t *testing.T) {
	// PartialErrors is position-aligned and can contain null placeholders. A
	// {"CampaignIds":[null],"PartialErrors":[null]} body has a non-empty PartialErrors
	// slice but NO actual error — it must stay UNCONFIRMED (the campaign may exist), not
	// be mis-reported as a definite rejection.
	api := &campaignsAPI{postBody: `{"CampaignIds":[null],"PartialErrors":[null]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error on a null-only PartialErrors body")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatal("expected a name-only partial for reconciliation")
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty when unconfirmed", res.CampaignID)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a null-only PartialErrors must be UNCONFIRMED, got: %v", err)
	}
}

// ---- ambiguous transport / 5xx on create -----------------------------------

func TestCreateCampaign_ServerErrorOnCreateIsUnconfirmed(t *testing.T) {
	// The lookup succeeds (empty), the create 500s ⇒ mutating 5xx is ambiguous ⇒
	// UNCONFIRMED with a name-only partial.
	api := &campaignsAPI{postStatus: http.StatusInternalServerError, postBody: `{"Errors":[{"ErrorCode":"InternalError"}]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on a 500 create")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatal("expected a name-only partial on an ambiguous create")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a 5xx create should be UNCONFIRMED, got: %v", err)
	}
}

func TestCreateCampaign_Definite4xxOnCreateIsCleanFailure(t *testing.T) {
	// A 400 on the create (definite client error, not 429) means nothing was created.
	api := &campaignsAPI{postStatus: http.StatusBadRequest, postBody: `{"Errors":[{"ErrorCode":"BadRequest"}]}`}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected an error on a 400 create")
	}
	if res != nil {
		t.Errorf("a definite 4xx create should return nil result, got %+v", res)
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a definite 4xx must not be UNCONFIRMED, got: %v", err)
	}
}

// ---- context already done --------------------------------------------------

func TestCreateCampaign_CancelledContextBeforeSendIsCleanFailure(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be sent on an already-cancelled context")
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := c.CreateCampaign(ctx, validInput())
	if err == nil {
		t.Fatal("expected a context error")
	}
	if res != nil {
		t.Errorf("a pre-send cancellation is a clean failure (nil result), got %+v", res)
	}
}

// ---- helpers ---------------------------------------------------------------

func TestComposeName_SanitizesAndOrders(t *testing.T) {
	in := CampaignInput{Project: "a|b", EventName: "  Big  Event ", NameSuffix: "s\x00uf"}
	name := composeName(in)
	if strings.Contains(name, "\x00") {
		t.Error("composed name retained a NUL control character")
	}
	if !strings.HasPrefix(name, "LFX | Search Campaign | ") {
		t.Errorf("name %q missing the LFX | Search Campaign prefix", name)
	}
	if strings.Contains(name, "a|b") {
		t.Errorf("delimiter not stripped from a project segment: %q", name)
	}
}

func TestToMSDate(t *testing.T) {
	d := toMSDate(time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC))
	if d.Year != 2026 || d.Month != 7 || d.Day != 22 {
		t.Errorf("toMSDate = %+v, want {7 22 2026}", d)
	}
}

// jsonString quotes s as a JSON string literal for embedding in a test fixture body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestNumberID(t *testing.T) {
	valid := map[string]string{"42": "42", "1": "1", "9999999999": "9999999999", " 3 ": "3"}
	for in, want := range valid {
		n := json.Number(in)
		if got := numberID(&n); got != want {
			t.Errorf("numberID(%q) = %q, want %q", in, got, want)
		}
	}
	// Malformed numbers must be rejected (→ "" → treated as UNCONFIRMED/no-id), not
	// accepted as a bogus id: zero, negative, fractional, and exponent forms.
	for _, bad := range []string{"0", "-1", "1.5", "1e3", "0.0", "+5", "abc", ""} {
		n := json.Number(bad)
		if got := numberID(&n); got != "" {
			t.Errorf("numberID(%q) = %q, want empty (malformed id must be rejected)", bad, got)
		}
	}
	if numberID(nil) != "" {
		t.Error("numberID(nil) must be empty")
	}
}
