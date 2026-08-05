// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
	adGroupName := composeAdGroupName(in)
	finalURL := buildAdFinalURL(in)
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
		// After the campaign self-heals to the winner (999), the hierarchy continues under
		// it. Return a pre-existing ad group and ad so the WHOLE tree pre-exists — then a
		// clean self-heal yields AlreadyExisted=true (nothing was created this run).
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[{"Id":111,"Name":`+jsonString(adGroupName)+`}]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[{"Id":222,"FinalUrls":[`+jsonString(finalURL)+`]}]}`)
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
	// The re-lookup ran (the 1115 forced a second QueryByAccountId).
	if lookups != 2 {
		t.Errorf("campaign lookups = %d, want 2 (pre-check + post-1115 re-resolve)", lookups)
	}
	// Campaign self-healed and the ad group + ad both pre-existed → nothing created this run.
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted = false, want true when the reconciled campaign + ad group + ad all pre-existed")
	}
}

// ---- status cascade -------------------------------------------------------

// statusCascadeRecorder collects the entity path + decoded body of every status PUT, under a
// mutex: the handler runs on the server goroutine and t.Cleanup(srv.Close) fires only AFTER
// the assertions, so it orders nothing for the test goroutine.
type statusCascadeRecorder struct {
	mu    sync.Mutex
	calls []statusCall
}

type statusCall struct {
	entity string
	body   map[string]any
}

func (r *statusCascadeRecorder) record(path string, body map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, statusCall{entity: path[strings.LastIndex(path, "/")+1:], body: body})
}

func (r *statusCascadeRecorder) snapshot() []statusCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]statusCall(nil), r.calls...)
}

func (r *statusCascadeRecorder) entities() []string {
	out := []string{}
	for _, c := range r.snapshot() {
		out = append(out, c.entity)
	}
	return out
}

// newStatusCascadeClient wires a client whose API server records each status PUT and replies
// with the per-entity body from replies (an empty string means a clean `{"PartialErrors":[]}`).
func newStatusCascadeClient(t *testing.T, rec *statusCascadeRecorder, replies map[string]string) *Client {
	t.Helper()
	return newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		entity := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		rec.record(r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		if reply, ok := replies[entity]; ok && reply != "" {
			_, _ = io.WriteString(w, reply)
			return
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
}

// TestUpdateCampaignAndChildrenStatus_PauseGatesParentFirst pins the PAUSE order: the campaign
// gate flips FIRST so delivery stops immediately, even if a child call fails afterwards.
func TestUpdateCampaignAndChildrenStatus_PauseGatesParentFirst(t *testing.T) {
	rec := &statusCascadeRecorder{}
	c := newStatusCascadeClient(t, rec, nil)
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	if got := rec.entities(); !reflect.DeepEqual(got, []string{"Campaigns", "AdGroups", "Ads"}) {
		t.Fatalf("pause order = %v, want [Campaigns AdGroups Ads] (gate first)", got)
	}
	assertCascadeParentIDs(t, rec.snapshot())
}

// assertCascadeParentIDs pins each child PUT to its OWN parent: the ad group scopes to the
// campaign and the ad scopes to the AD GROUP. Passing the campaign id as AdGroupId would
// address a different entity entirely and silently toggle nothing (or the wrong thing).
func assertCascadeParentIDs(t *testing.T, calls []statusCall) {
	t.Helper()
	var sawAdGroup, sawAd bool
	for _, call := range calls {
		switch call.entity {
		case "AdGroups":
			sawAdGroup = true
			if got := call.body["CampaignId"]; got != json.Number("321") && got != float64(321) {
				t.Errorf("AdGroups body CampaignId = %v, want the campaign id 321", got)
			}
		case "Ads":
			sawAd = true
			if got := call.body["AdGroupId"]; got != json.Number("654") && got != float64(654) {
				t.Errorf("Ads body AdGroupId = %v, want the AD GROUP id 654, not the campaign id", got)
			}
		}
	}
	if !sawAdGroup || !sawAd {
		t.Errorf("cascade must PUT both children (saw ad group=%v, ad=%v)", sawAdGroup, sawAd)
	}
}

// TestUpdateCampaignAndChildrenStatus_ActivateEnablesChildrenFirst pins the reverse order: on
// ACTIVATE the children go first so the campaign never serves over paused children.
func TestUpdateCampaignAndChildrenStatus_ActivateEnablesChildrenFirst(t *testing.T) {
	rec := &statusCascadeRecorder{}
	c := newStatusCascadeClient(t, rec, nil)
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusActive); err != nil {
		t.Fatalf("UpdateCampaignAndChildrenStatus: %v", err)
	}
	if got := rec.entities(); !reflect.DeepEqual(got, []string{"AdGroups", "Ads", "Campaigns"}) {
		t.Fatalf("activate order = %v, want [AdGroups Ads Campaigns] (gate last)", got)
	}
	assertCascadeParentIDs(t, rec.snapshot())
	for _, call := range rec.snapshot() {
		for _, key := range []string{"Campaigns", "AdGroups", "Ads"} {
			list, ok := call.body[key].([]any)
			if !ok || len(list) == 0 {
				continue
			}
			entry, _ := list[0].(map[string]any)
			if entry["Status"] != StatusActive {
				t.Errorf("%s entry Status = %v, want %s", call.entity, entry["Status"], StatusActive)
			}
		}
	}
}

// TestUpdateCampaignAndChildrenStatus_PartialCascadeIsUnconfirmed pins the honesty rule: once
// an entity HAS been changed, a later definite rejection is an ambiguous OVERALL outcome, so
// it must classify as Unconfirmed rather than "nothing applied".
func TestUpdateCampaignAndChildrenStatus_PartialCascadeIsUnconfirmed(t *testing.T) {
	rejected := `{"PartialErrors":[{"Code":1234,"Message":"nope"}]}`
	for _, tc := range []struct {
		name    string
		status  string
		replies map[string]string
		// wantApplied is the entity text that must appear: it names what DID change.
		wantApplied string
	}{
		{"activate: ad fails after ad group applied", StatusActive, map[string]string{"Ads": rejected}, "ad group"},
		{"activate: campaign fails after both children", StatusActive, map[string]string{"Campaigns": rejected}, "ad group and ad"},
		{"pause: ad group fails after campaign gated", StatusPaused, map[string]string{"AdGroups": rejected}, "campaign"},
		{"pause: ad fails after campaign + ad group", StatusPaused, map[string]string{"Ads": rejected}, "campaign and ad group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &statusCascadeRecorder{}
			c := newStatusCascadeClient(t, rec, tc.replies)
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", tc.status)
			if err == nil {
				t.Fatal("a rejected status PUT must not be reported as success")
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("a PARTIALLY applied cascade must be Unconfirmed (verify before retry), got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantApplied+" status changed") {
				t.Errorf("error must name what DID apply (%q), got: %v", tc.wantApplied, err)
			}
		})
	}
}

// TestUpdateCampaignAndChildrenStatus_FirstStepFailureStaysDefinite is the counterpart: when
// the FIRST call fails nothing was mutated, so a definite rejection must stay definite —
// classifying it Unconfirmed would strand the caller in verify-before-retry forever.
func TestUpdateCampaignAndChildrenStatus_FirstStepFailureStaysDefinite(t *testing.T) {
	rejected := `{"PartialErrors":[{"Code":1234,"Message":"nope"}]}`
	for _, tc := range []struct {
		name, status, firstEntity string
	}{
		{"pause gates the campaign first", StatusPaused, "Campaigns"},
		{"activate enables the ad group first", StatusActive, "AdGroups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &statusCascadeRecorder{}
			c := newStatusCascadeClient(t, rec, map[string]string{tc.firstEntity: rejected})
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", tc.status)
			if err == nil {
				t.Fatal("a rejected status PUT must not be reported as success")
			}
			if IsOutcomeUnconfirmed(err) {
				t.Errorf("a first-step rejection mutated nothing, so it must stay DEFINITE, got: %v", err)
			}
			if got := rec.entities(); len(got) != 1 {
				t.Errorf("cascade issued %v, want it to stop after the failed first step", got)
			}
		})
	}
}

// TestUpdateCampaignAndChildrenStatus_PauseWithoutChildIDs covers the pause path with unknown
// children: pausing the parent already stops delivery, so it must succeed and skip the
// child calls rather than refuse (the ACTIVATE guard is deliberately one-sided).
func TestUpdateCampaignAndChildrenStatus_PauseWithoutChildIDs(t *testing.T) {
	rec := &statusCascadeRecorder{}
	c := newStatusCascadeClient(t, rec, nil)
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "", "", StatusPaused); err != nil {
		t.Fatalf("pausing with unknown children must still gate the campaign, got: %v", err)
	}
	if got := rec.entities(); !reflect.DeepEqual(got, []string{"Campaigns"}) {
		t.Errorf("calls = %v, want only the campaign gate when no child ids are known", got)
	}
}

// TestUpdateCampaignAndChildrenStatus_RejectsBadInputWithoutCalling pins every local
// fail-closed check: each refusal must happen BEFORE any request reaches Microsoft.
func TestUpdateCampaignAndChildrenStatus_RejectsBadInputWithoutCalling(t *testing.T) {
	for _, tc := range []struct {
		name, campaignID, adGroupID, adID, status, wantMsg string
	}{
		{"unsupported status", "321", "654", "987", "Deleted", "unsupported status"},
		{"empty status", "321", "654", "987", "", "unsupported status"},
		{"non-numeric campaign id", "urn:li:campaign:321", "654", "987", StatusPaused, "campaign id"},
		{"empty campaign id", "", "654", "987", StatusPaused, "campaign id"},
		{"non-numeric ad group id", "321", "ag-654", "987", StatusPaused, "ad group id"},
		{"non-numeric ad id", "321", "654", "ad-987", StatusPaused, "ad id"},
		{"activate without an ad group", "321", "", "987", StatusActive, "cannot activate"},
		{"activate without an ad", "321", "654", "", StatusActive, "cannot activate"},
		{"activate with whitespace-only ad group", "321", "   ", "987", StatusActive, "cannot activate"},
		// An ad id with NO ad-group id is unaddressable: the Ads PUT is scoped by AdGroupId,
		// and json.Number("") marshals to a bare 0, so sending it would target a nonexistent
		// ad group and report a no-op as success. Skipping it would leave the ad Active while
		// returning nil. Both are wrong — refuse the pair.
		{"pause with an ad but no ad group", "321", "", "987", StatusPaused, "no ad group id"},
		{"pause with an ad but whitespace-only ad group", "321", "  ", "987", StatusPaused, "no ad group id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &statusCascadeRecorder{}
			c := newStatusCascadeClient(t, rec, nil)
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), tc.campaignID, tc.adGroupID, tc.adID, tc.status)
			if err == nil {
				t.Fatal("malformed input must be rejected")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantMsg)
			}
			if got := rec.entities(); len(got) != 0 {
				t.Errorf("issued %v, want NO upstream call — the refusal is a local check", got)
			}
		})
	}
}

// TestUpdateCampaignAndChildrenStatus_UndecodableBodyIsUnconfirmed pins putStatus's malformed-
// 200 handling: Microsoft may have applied the change, so it must never read as success.
func TestUpdateCampaignAndChildrenStatus_UndecodableBodyIsUnconfirmed(t *testing.T) {
	rec := &statusCascadeRecorder{}
	c := newStatusCascadeClient(t, rec, map[string]string{"Campaigns": `{"PartialErrors":`})
	err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused)
	if err == nil {
		t.Fatal("an undecodable 200 must not be reported as success — the update may have applied")
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("an undecodable 200 leaves the outcome AMBIGUOUS, want Unconfirmed, got %T: %v", err, err)
	}
}

// TestUpdateCampaignAndChildrenStatus_OmittedPartialErrorsIsUnconfirmed pins the gap between a
// DECODABLE body and an ANSWERED one.
//
// `{}` and a top-level `null` are valid JSON and unmarshal without error, leaving PartialErrors
// zero — which reads identically to Microsoft affirming "no entity failed". Before the presence
// check, both reported SUCCESS and the service persisted a status Microsoft never confirmed. The
// distinction matters because these are exactly what a proxy error page or a truncated response
// looks like after it parses.
func TestUpdateCampaignAndChildrenStatus_OmittedPartialErrorsIsUnconfirmed(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":    `{}`,
		"top-level null":  `null`,
		"unrelated field": `{"CampaignIds":[321]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &statusCascadeRecorder{}
			c := newStatusCascadeClient(t, rec, map[string]string{"Campaigns": body})
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused)
			if err == nil {
				t.Fatal("a body that never reported PartialErrors must not read as success")
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("an unanswered PartialErrors leaves the outcome AMBIGUOUS, want Unconfirmed, got %T: %v", err, err)
			}
		})
	}
}

// TestUpdateCampaignAndChildrenStatus_AcceptsEmptyPartialErrorForms is the other half of the
// presence check: `null` and `[]` ARE Microsoft answering "no per-entity failures", so treating
// absence as unconfirmed must not also reject the valid empty forms.
func TestUpdateCampaignAndChildrenStatus_AcceptsEmptyPartialErrorForms(t *testing.T) {
	for name, body := range map[string]string{
		"null":         `{"PartialErrors":null}`,
		"empty array":  `{"PartialErrors":[]}`,
		"with ids too": `{"CampaignIds":[321],"PartialErrors":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &statusCascadeRecorder{}
			c := newStatusCascadeClient(t, rec, map[string]string{"Campaigns": body})
			if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused); err != nil {
				t.Fatalf("an explicitly empty PartialErrors means no failure, got: %v", err)
			}
		})
	}
}

// TestUpdateCampaignAndChildrenStatus_RetriesThrottledStatusPut pins the status PUT as IDEMPOTENT.
//
// Re-applying Active/Paused converges on the same state and cannot double-commit a paid resource,
// so a 429 must be absorbed by the client's bounded retry. Passing idempotent=false turned routine
// Microsoft throttling into an Unconfirmed toggle the dispatcher then had to verify before
// retrying — a strictly worse outcome than one retried request.
// The counter is mutex-guarded like statusCascadeRecorder above: it is written on the httptest
// server's goroutines and read by the assertion, and neither the client returning nor
// t.Cleanup(srv.Close) establishes happens-before with that read. The requests happen to be
// serialized today, so this is a latent race rather than a live one — which is exactly the kind
// that surfaces as a -race flake after an unrelated change.
func TestUpdateCampaignAndChildrenStatus_RetriesThrottledStatusPut(t *testing.T) {
	var (
		mu               sync.Mutex
		campaignAttempts int
	)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		entity := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		if entity == "Campaigns" {
			mu.Lock()
			campaignAttempts++
			first := campaignAttempts == 1
			mu.Unlock()
			if first {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused); err != nil {
		t.Fatalf("a throttled status PUT must be retried, not surfaced: %v", err)
	}
	mu.Lock()
	attempts := campaignAttempts
	mu.Unlock()
	if attempts < 2 {
		t.Errorf("campaign status PUT attempts = %d, want >= 2 (the 429 must be retried)", attempts)
	}
}

// TestUpdateCampaignAndChildrenStatus_PauseWithAdGroupOnly covers the one asymmetric pause
// shape that IS allowed: an ad group with no ad. The ad group is addressable (its PUT is
// scoped by CampaignId), so it must be paused and the ad step simply skipped — and the
// error text for a failure there must not claim the ad group applied when it did.
func TestUpdateCampaignAndChildrenStatus_PauseWithAdGroupOnly(t *testing.T) {
	rec := &statusCascadeRecorder{}
	c := newStatusCascadeClient(t, rec, nil)
	if err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "", StatusPaused); err != nil {
		t.Fatalf("pausing with a known ad group and no ad must succeed, got: %v", err)
	}
	if got := rec.entities(); !reflect.DeepEqual(got, []string{"Campaigns", "AdGroups"}) {
		t.Errorf("calls = %v, want [Campaigns AdGroups] — the ad step has nothing to address", got)
	}
}

// TestUpdateCampaignAndChildrenStatus_AppliedTextNamesOnlyRealChanges pins the operator-facing
// text on a PARTIAL cascade. "applied" is read to decide what to verify by hand, so it must
// never name an entity that was skipped: with no ad group, a failing ad-group step is
// impossible, but a campaign-only cascade that later fails must say "campaign", not
// "campaign and ad group".
func TestUpdateCampaignAndChildrenStatus_AppliedTextNamesOnlyRealChanges(t *testing.T) {
	rejected := `{"PartialErrors":[{"Code":1234,"Message":"nope"}]}`
	rec := &statusCascadeRecorder{}
	c := newStatusCascadeClient(t, rec, map[string]string{"AdGroups": rejected})
	err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused)
	if err == nil {
		t.Fatal("a rejected ad-group update must not be reported as success")
	}
	if strings.Contains(err.Error(), "ad group status changed") {
		t.Errorf("the ad group was REJECTED, so it must not be listed as applied: %v", err)
	}
	if !strings.Contains(err.Error(), "campaign status changed") {
		t.Errorf("the campaign DID apply and must be named so an operator knows to verify it: %v", err)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper so a test can act on the exact
// moment a response has been fully delivered.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestUpdateCampaignAndChildrenStatus_CancelledContextStopsCascade pins the ctx check between
// steps: a cancellation after the gate applied must stop the cascade and report a PARTIAL
// (Unconfirmed) outcome naming the campaign, not silently dispatch the remaining PUTs.
func TestUpdateCampaignAndChildrenStatus_CancelledContextStopsCascade(t *testing.T) {
	rec := &statusCascadeRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	// cancelAfterCampaign is armed by the handler (server goroutine) and fired by the
	// RoundTripper wrapper (client goroutine). It MUST be atomic: RoundTrip returns once the
	// response headers are read, which is not ordered against the handler returning, and
	// t.Cleanup(srv.Close) fires only after the assertions — so nothing else supplies a
	// happens-before edge. A plain bool would both race and flake (a stale false read would
	// skip cancel(), the cascade would complete, and the failure would point at production
	// code that is correct). CompareAndSwap additionally gives the fire-once semantics a
	// Load/Store pair would not.
	var cancelAfterCampaign atomic.Bool
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		rec.record(r.URL.Path, body)
		if strings.HasSuffix(r.URL.Path, "Campaigns") {
			cancelAfterCampaign.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	base := c.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		// The campaign PUT completed and its status DID apply; only now cancel, so the next
		// step is abandoned cleanly between requests. CompareAndSwap fires exactly once.
		if err == nil && cancelAfterCampaign.CompareAndSwap(true, false) {
			cancel()
		}
		return resp, err
	})
	err := c.UpdateCampaignAndChildrenStatus(ctx, "321", "654", "987", StatusPaused)
	if err == nil {
		t.Fatal("a cancelled cascade must not report success — the children were never paused")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want a context.Canceled cause, got %T: %v", err, err)
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("the campaign gate DID apply, so the outcome is partial/Unconfirmed, got: %v", err)
	}
	if got := rec.entities(); !reflect.DeepEqual(got, []string{"Campaigns"}) {
		t.Errorf("calls = %v, want the cascade to STOP at the cancellation, not dispatch more PUTs", got)
	}
	// NOTE ON WHAT THIS DOES AND DOES NOT PIN: the explicit ctx.Err() check between steps and
	// a cancelled putStatus produce the SAME partialCascadeError here, so deleting the check
	// would not fail this test — it only avoids dispatching a request that is already doomed.
	// What IS pinned, and what would break without the surrounding structure, is the reported
	// shape: a cancellation after the gate applied must be a partial (Unconfirmed) naming the
	// campaign, never a bare error implying nothing changed.
	var partial *partialCascadeError
	if !errors.As(err, &partial) {
		t.Fatalf("a between-step cancellation must be a partialCascadeError, got %T: %v", err, err)
	}
	if partial.applied != "campaign" {
		t.Errorf("applied = %q, want %q — only the campaign gate had been applied", partial.applied, "campaign")
	}
	if partial.stage != "ad group" {
		t.Errorf("stage = %q, want %q — the ad group is the step that was abandoned", partial.stage, "ad group")
	}
}

// TestUpdateCampaignAndChildrenStatus_CancelledDuring429BackoffReportsUnconfirmed pins the
// fix for the context cancellation during 429 backoff bug: a cancelled context while
// doRequest is sleeping out a 429 retry must wrap the ctx.Err() in a transportError so
// that IsOutcomeUnconfirmed correctly classifies it as ambiguous (the PUT may have already
// applied), not as a definite failure. Without the wrap, the bare ctx.Err() would match
// neither transportError nor apiError, misleading the cascade into thinking nothing changed.
func TestUpdateCampaignAndChildrenStatus_CancelledDuring429BackoffReportsUnconfirmed(t *testing.T) {
	var (
		mu               sync.Mutex
		campaignAttempts int
	)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		entity := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		if entity == "Campaigns" {
			mu.Lock()
			campaignAttempts++
			first := campaignAttempts == 1
			mu.Unlock()
			if first {
				// Return 429 with a long retry so the RoundTripper wrapper has time to cancel
				// the context while doRequest is sleeping.
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	})
	base := c.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	ctx, cancel := context.WithCancel(context.Background())
	var cancelledDuring429 atomic.Bool
	c.httpClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		// If this is a 429 on the Campaigns endpoint, arm a separate goroutine to cancel
		// the context after a short delay (allowing the 429 retry loop to enter time.After).
		if err == nil && resp.StatusCode == http.StatusTooManyRequests &&
			strings.HasSuffix(r.URL.Path, "Campaigns") &&
			cancelledDuring429.CompareAndSwap(false, true) {
			go func() {
				time.Sleep(10 * time.Millisecond) // Let the 429 backoff start
				cancel()
			}()
		}
		return resp, err
	})
	err := c.UpdateCampaignAndChildrenStatus(ctx, "321", "654", "987", StatusPaused)
	if err == nil {
		t.Fatal("a cancellation during 429 backoff must report an error — the outcome is ambiguous")
	}
	// The key check: the cancellation during backoff must be classified as ambiguous/Unconfirmed.
	// Without the fix (wrapping ctx.Err() in transportError), the bare context.Canceled would
	// match neither transportError nor apiError, so IsOutcomeUnconfirmed would return false.
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("a 429 that was received and then cancelled during backoff is ambiguous (the PUT may have applied), want IsOutcomeUnconfirmed==true, got: %v", err)
	}
}

// TestUpdateCampaignAndChildrenStatus_MalformedPartialErrorsList pins the fix for the
// malformed PartialErrors array bug: a response like {"PartialErrors":[null]} decodes
// without error, but the array contains no valid error codes and should be treated as an
// unconfirmed outcome (the status Microsoft returned is unusable), not as success.
func TestUpdateCampaignAndChildrenStatus_MalformedPartialErrorsList(t *testing.T) {
	tests := []struct {
		name      string
		respBody  string
		wantError bool
	}{
		{
			name:      "null-only-list",
			respBody:  `{"PartialErrors":[null]}`,
			wantError: true, // Malformed array with null should be rejected
		},
		{
			name:      "empty-object-in-list",
			respBody:  `{"PartialErrors":[{}]}`,
			wantError: true, // Malformed array with {} should be rejected
		},
		{
			name:      "empty-list",
			respBody:  `{"PartialErrors":[]}`,
			wantError: false, // Empty list is a valid "no errors" response
		},
		{
			name:      "null-field",
			respBody:  `{"PartialErrors":null}`,
			wantError: false, // null field is a valid "no errors" response
		},
		{
			name:      "valid-error",
			respBody:  `{"PartialErrors":[{"Code":1234}]}`,
			wantError: true, // Real error should be rejected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.respBody)
			})
			err := c.UpdateCampaignAndChildrenStatus(context.Background(), "321", "654", "987", StatusPaused)
			if tt.wantError && err == nil {
				t.Errorf("response %q should be rejected but was accepted", tt.respBody)
			}
			if !tt.wantError && err != nil {
				t.Errorf("response %q should be accepted but got error: %v", tt.respBody, err)
			}
			// The null-only and empty-object cases should specifically be Unconfirmed,
			// not generic rejections.
			if (tt.name == "null-only-list" || tt.name == "empty-object-in-list") && err != nil {
				if !IsOutcomeUnconfirmed(err) {
					t.Errorf("malformed PartialErrors should be Unconfirmed, got: %v", err)
				}
			}
		})
	}
}
