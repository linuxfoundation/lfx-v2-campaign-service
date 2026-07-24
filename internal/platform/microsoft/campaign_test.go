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

// validInput is a well-formed CampaignInput the create tests can start from. It carries
// a RegistrationURL because CreateCampaign completes the full Campaign->AdGroup->Ad
// hierarchy and the ad requires a destination.
func validInput() CampaignInput {
	return CampaignInput{
		EventName:       "KubeCon",
		EventSlug:       "kubecon",
		Project:         "CNCF",
		Budget:          50,
		NameSuffix:      "brief-1",
		RegistrationURL: "https://events.example.org/register",
	}
}

// campaignsAPI dispatches every route CreateCampaign touches across the full hierarchy.
// ALL routes are POST (v13 REST reads are POST /<Entity>/QueryBy…; creates are POST
// /<Entity>). Lookup routes are checked BEFORE the bare create routes. *Seen fields
// capture the decoded request bodies; *Body / *Status script each step.
type campaignsAPI struct {
	// Campaign (MS-2).
	getBody    string // QueryByAccountId response
	postBody   string // create response
	postStatus int
	postSeen   *createCampaignsRequest
	querySeen  *queryCampaignsRequest
	// AdGroup (MS-2.5).
	adGroupGetBody  string
	adGroupPostBody string
	adGroupStatus   int
	adGroupSeen     *createAdGroupsRequest
	// Ad (MS-2.5).
	adGetBody  string
	adPostBody string
	adStatus   int
	adSeen     *createAdsRequest
}

func (h *campaignsAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("unexpected non-POST request %s %s", r.Method, p)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		// ---- reads (QueryBy…) — check BEFORE the bare create routes ----
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			decodeTo(t, r, h.querySeen)
			writeOr(w, h.getBody, `{"Campaigns":[]}`)
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			writeOr(w, h.adGroupGetBody, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			writeOr(w, h.adGetBody, `{"Ads":[]}`)
		// ---- creates ----
		case strings.HasSuffix(p, "/Campaigns"):
			decodeTo(t, r, h.postSeen)
			writeStatusOr(w, h.postStatus, h.postBody, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			decodeTo(t, r, h.adGroupSeen)
			writeStatusOr(w, h.adGroupStatus, h.adGroupPostBody, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			decodeTo(t, r, h.adSeen)
			writeStatusOr(w, h.adStatus, h.adPostBody, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

// decodeTo decodes the request body into *dst when dst is non-nil.
func decodeTo[T any](t *testing.T, r *http.Request, dst *T) {
	t.Helper()
	if dst == nil {
		return
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		t.Errorf("decode request body for %s: %v", r.URL.Path, err)
	}
}

func writeOr(w http.ResponseWriter, body, dflt string) {
	if body == "" {
		body = dflt
	}
	_, _ = io.WriteString(w, body)
}

func writeStatusOr(w http.ResponseWriter, status int, body, dflt string) {
	if status != 0 {
		w.WriteHeader(status)
	}
	writeOr(w, body, dflt)
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
	// The campaign already existed, but this run still CREATED the ad group and ad (they
	// were not pre-provided), so AlreadyExisted must be false: the run produced something
	// new. AlreadyExisted is true ONLY when all three levels pre-existed
	// (see TestCreateCampaign_AlreadyExistedWhenWholeTreePreexists).
	if res.AlreadyExisted {
		t.Error("AlreadyExisted = true, want false when the ad group/ad were created this run")
	}
}

func TestCreateCampaign_AlreadyExistedWhenWholeTreePreexists(t *testing.T) {
	in := validInput()
	name := composeName(in)
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
	// Every level is pre-provided by its lookup, so nothing is created this run.
	api := &campaignsAPI{
		getBody:        `{"Campaigns":[{"Id":999,"Name":` + jsonString(name) + `}]}`,
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(adGroupName) + `}]}`,
		adGetBody:      `{"Ads":[{"Id":222,"FinalUrls":[` + jsonString(finalURL) + `]}]}`,
	}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Method == http.MethodPost &&
			(strings.HasSuffix(p, "/Campaigns") || strings.HasSuffix(p, "/AdGroups") || strings.HasSuffix(p, "/Ads")) {
			t.Errorf("create POST %s issued despite every level pre-existing", p)
		}
		base(w, r)
	})
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted = false, want true when campaign, ad group, AND ad all pre-existed")
	}
	if res.CampaignID != "999" || res.AdGroupID != "111" || res.AdID != "222" {
		t.Errorf("ids = %q/%q/%q, want the existing 999/111/222", res.CampaignID, res.AdGroupID, res.AdID)
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
