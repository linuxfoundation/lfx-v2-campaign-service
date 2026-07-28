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

func TestCreateCampaign_DuplicateNameNumericCodeOnlyIsAlreadyExists(t *testing.T) {
	// A BatchError may carry only the numeric Code 1115 (no symbolic ErrorCode enum). 1115
	// IS CampaignServiceCannotCreateDuplicateCampaign, so it must still be recognized as
	// already-exists, not a generic partial failure.
	api := &campaignsAPI{
		postBody: `{"CampaignIds":[null],"PartialErrors":[{"Code":1115}]}`,
	}
	c := newAPIClient(t, api.handler(t))
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected a duplicate-name error")
	}
	if res == nil || res.CampaignName == "" {
		t.Fatal("expected a name-only partial for reconciliation")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("numeric Code 1115 must be recognized as already-exists, got: %v", err)
	}
}

func TestCreateCampaign_LookupMatchWithNoIDIsUnconfirmed(t *testing.T) {
	// The name-lookup finds the campaign by its unique name but its Id is null. Treating
	// that as absent would run CreateCampaigns and create a DUPLICATE. It must instead be
	// UNCONFIRMED (verify before retrying), with no create issued.
	in := validInput()
	name := composeName(in)
	postReached := false
	api := &campaignsAPI{
		getBody: `{"Campaigns":[{"Id":null,"Name":` + jsonString(name) + `}]}`,
	}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns") {
			postReached = true
		}
		base(w, r)
	})
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error when the matching campaign has no usable id")
	}
	if postReached {
		t.Error("create POST issued despite a name-matching campaign (would duplicate)")
	}
	if !strings.Contains(err.Error(), "verify") && !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a name-match with an unusable id must be UNCONFIRMED, got: %v", err)
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

// TestFirstCampaignID_BoundsMaterialization: a malformed 200 packed with thousands of null
// CampaignIds / PartialErrors must NOT expand into millions of retained slice elements — the
// bounded slice types cap what's kept while still decoding the (valid) arrays, and the first
// real id is still read. Guards against the per-create OOM risk on an up-to-8-MiB body.
func TestFirstCampaignID_BoundsMaterialization(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"CampaignIds":[321`)
	for i := 0; i < 5000; i++ {
		b.WriteString(",null")
	}
	b.WriteString(`],"PartialErrors":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"Code":9999}`)
	}
	b.WriteString(`]}`)

	// The leading id is still read despite the huge tail.
	if id, err := firstCampaignID([]byte(b.String())); err != nil || id != "321" {
		t.Fatalf("firstCampaignID = (%q, %v), want (\"321\", nil) — leading id must survive the bounded decode", id, err)
	}

	// The bounded slice types retain at most the cap, not all 5000+ entries.
	var resp createCampaignsResponse
	if err := json.Unmarshal([]byte(b.String()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.CampaignIds) > maxDecodedErrorItems {
		t.Errorf("CampaignIds retained %d, want <= the %d cap", len(resp.CampaignIds), maxDecodedErrorItems)
	}
	if len(resp.PartialErrors) > maxDecodedErrorItems {
		t.Errorf("PartialErrors retained %d, want <= the %d cap", len(resp.PartialErrors), maxDecodedErrorItems)
	}
}

func TestCreateCampaign_NullPartialErrorIsUnconfirmed(t *testing.T) {
	// v13's PartialErrors is a SPARSE BatchError list (a failed item only, carrying an Index),
	// so a real single-item failure never produces a null-only entry. This defensively covers a
	// MALFORMED body that null-pads anyway: {"CampaignIds":[null],"PartialErrors":[null]} has a
	// non-empty slice but NO actual error, so it must stay UNCONFIRMED (the campaign may exist),
	// not be mis-reported as a definite rejection.
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

// TestCreateCampaign_DuplicateRaceReLookupErrorSurfacesCause: when the duplicate-name
// self-heal re-lookup itself FAILS (a 500, not merely "no id"), the returned error must
// surface that reconciliation-lookup cause — not silently wrap only the original duplicate
// error, which would hide whether the id couldn't be resolved because of a 500, an auth
// failure, or a timeout.
func TestCreateCampaign_DuplicateRaceReLookupErrorSurfacesCause(t *testing.T) {
	in := validInput()
	lookups := 0
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			lookups++
			if lookups == 1 {
				_, _ = io.WriteString(w, `{"Campaigns":[]}`) // pre-check: absent → create runs
			} else {
				// re-lookup after the 1115 FAILS with a 500 (the reconciliation can't resolve).
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"Errors":[{"ErrorCode":"InternalError"}]}`)
			}
		case strings.HasSuffix(p, "/Campaigns"):
			// The create loses the duplicate race → 1115 duplicate-name PartialError.
			_, _ = io.WriteString(w, `{"CampaignIds":[null],"PartialErrors":[{"Code":1115}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
		}
	})
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error when the reconciliation lookup fails")
	}
	if res == nil || res.CampaignID != "" {
		t.Fatalf("expected a name-only partial with no id, got %+v", res)
	}
	if !strings.Contains(err.Error(), "reconciliation lookup failed") {
		t.Errorf("error must surface the re-lookup cause, got: %v", err)
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
	// accepted as a bogus id: zero, negative, fractional, exponent, and — since Microsoft ids
	// are signed 64-bit — a digits-only value that OVERFLOWS int64 (the first out-of-range
	// value 9223372036854775808 = math.MaxInt64+1, plus a 20-digit overflow), which the
	// digits-only regex alone would wrongly pass.
	for _, bad := range []string{"0", "-1", "1.5", "1e3", "0.0", "+5", "abc", "", "9223372036854775808", "99999999999999999999"} {
		n := json.Number(bad)
		if got := numberID(&n); got != "" {
			t.Errorf("numberID(%q) = %q, want empty (malformed id must be rejected)", bad, got)
		}
	}
	// The largest VALID signed-64-bit id must still be accepted (boundary).
	if maxN := json.Number("9223372036854775807"); numberID(&maxN) != "9223372036854775807" {
		t.Error("numberID must accept math.MaxInt64 (the largest valid id)")
	}
	if numberID(nil) != "" {
		t.Error("numberID(nil) must be empty")
	}
}

// TestCreateCampaign_OmittedCampaignsFieldIsUnconfirmed: a 2xx lookup body that OMITS the
// Campaigns field (nil pointer, not an empty list) can't confirm the campaign is absent, so it
// must be UNCONFIRMED (no create issued) — not treated as "absent" (which would run the paid
// create and risk a duplicate). A PRESENT empty list, by contrast, is a real "absent".
// TestLookupCampaignByName covers the streaming lookup directly: it finds a name match
// without materializing the whole array, preserves the omitted/null-vs-present distinction,
// and reports a matched-but-no-id case. Streaming means a huge (malformed) body costs O(1).
func TestLookupCampaignByName(t *testing.T) {
	// present + match with a usable id, even when the match is buried after many entries.
	var big strings.Builder
	big.WriteString(`{"Campaigns":[`)
	for i := 0; i < 5000; i++ {
		big.WriteString(`{"Id":null,"Name":"other"},`)
	}
	big.WriteString(`{"Id":777,"Name":"TARGET"}]}`)
	id, matched, present, err := lookupCampaignByName([]byte(big.String()), "target") // case-insensitive
	if err != nil || !present || !matched || id != "777" {
		t.Fatalf("buried match: got id=%q matched=%v present=%v err=%v, want 777/true/true/nil", id, matched, present, err)
	}

	// omitted field → present=false (UNCONFIRMED).
	if _, m, p, err := lookupCampaignByName([]byte(`{}`), "x"); err != nil || m || p {
		t.Errorf("omitted Campaigns: got matched=%v present=%v err=%v, want false/false/nil", m, p, err)
	}
	// null field → present=false.
	if _, _, p, err := lookupCampaignByName([]byte(`{"Campaigns":null}`), "x"); err != nil || p {
		t.Errorf("null Campaigns: present=%v err=%v, want false/nil", p, err)
	}
	// present empty → present=true, matched=false (genuine absence, safe to create).
	if _, m, p, err := lookupCampaignByName([]byte(`{"Campaigns":[]}`), "x"); err != nil || m || !p {
		t.Errorf("empty Campaigns: matched=%v present=%v err=%v, want false/true/nil", m, p, err)
	}
	// matched but no usable id → matched=true, id="".
	if id, m, _, err := lookupCampaignByName([]byte(`{"Campaigns":[{"Id":null,"Name":"z"}]}`), "z"); err != nil || !m || id != "" {
		t.Errorf("match-no-id: id=%q matched=%v err=%v, want \"\"/true/nil", id, m, err)
	}

	// FAIL-CLOSED on a truncated body: an unterminated Campaigns array must ERROR (→ the caller
	// treats it as UNCONFIRMED), NOT be reported as present-with-no-match (a clean "absent" that
	// would let the paid create run and risk a duplicate).
	for _, bad := range []string{
		`{"Campaigns":[`,                     // array opened, never closed (EOF mid-array)
		`{"Campaigns":[{"Id":1,"Name":"a"}`,  // one element, no closing ]
		`{"Campaigns":[{"Id":1,"Name":"a"},`, // trailing comma then EOF
	} {
		if _, m, p, err := lookupCampaignByName([]byte(bad), "a"); err == nil {
			t.Errorf("truncated %q must error (fail closed), got matched=%v present=%v err=nil", bad, m, p)
		}
	}
}

func TestCreateCampaign_OmittedCampaignsFieldIsUnconfirmed(t *testing.T) {
	postReached := false
	api := &campaignsAPI{getBody: `{}`} // no Campaigns field
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns") {
			postReached = true
		}
		base(w, r)
	})
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected UNCONFIRMED when the lookup omits the Campaigns field")
	}
	if postReached {
		t.Error("create POST issued despite an unconfirmable lookup (would risk a duplicate)")
	}
	if !strings.Contains(err.Error(), "verify") && !strings.Contains(err.Error(), "cannot confirm") {
		t.Errorf("an omitted-Campaigns lookup must be UNCONFIRMED, got: %v", err)
	}
	if res == nil || res.CampaignName == "" {
		t.Error("expected a name-only partial for reconciliation")
	}
}

// TestCreateCampaign_CallerTimeZonePassThrough: a caller-supplied in.TimeZone (non-empty) must
// reach the create body verbatim, not be overridden by defaultTimeZone.
func TestCreateCampaign_CallerTimeZonePassThrough(t *testing.T) {
	var seen createCampaignsRequest
	api := &campaignsAPI{postSeen: &seen}
	c := newAPIClient(t, api.handler(t))
	in := validInput()
	in.TimeZone = "PacificTimeUSCanadaTijuana2" // a distinct, non-default sentinel
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(seen.Campaigns) != 1 {
		t.Fatalf("sent %d campaigns, want 1", len(seen.Campaigns))
	}
	if got := seen.Campaigns[0].TimeZone; got != in.TimeZone {
		t.Errorf("TimeZone = %q, want the caller-supplied %q (not the default)", got, in.TimeZone)
	}
}

// TestCreateCampaign_DuplicateNameRaceSelfHeals: a duplicate-name PartialError on the create
// (a race lost between the pre-check lookup and the create) is SELF-HEALED by re-looking the
// campaign up by name and returning it as already-exists with the winner's id — mirroring the
// ad-group 1214 path — rather than forcing the caller to reconcile by name.
func TestCreateCampaign_DuplicateNameRaceSelfHeals(t *testing.T) {
	in := validInput()
	name := composeName(in)
	lookups := 0
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			lookups++
			if lookups == 1 {
				_, _ = io.WriteString(w, `{"Campaigns":[]}`) // pre-check: absent → create runs
			} else {
				// re-lookup after the 1115: the winner exists.
				_, _ = io.WriteString(w, `{"Campaigns":[{"Id":999,"Name":`+jsonString(name)+`}]}`)
			}
		case strings.HasSuffix(p, "/Campaigns"):
			// The create loses the duplicate race → 1115 duplicate-name PartialError.
			_, _ = io.WriteString(w, `{"CampaignIds":[null],"PartialErrors":[{"Code":1115}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
		}
	})
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("a duplicate-name race should self-heal to the existing campaign, got: %v", err)
	}
	if res.CampaignID != "999" {
		t.Errorf("CampaignID = %q, want the reconciled 999", res.CampaignID)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted = false, want true on a reconciled duplicate race")
	}
}
