// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newGeoClient wires the full Search cascade plus the campaignCriteria:mutate and
// adGroupCriteria:mutate endpoints the geo paths use, capturing each criteria request
// BODY so the tests can assert what was actually sent rather than that a call happened.
func newGeoClient(t *testing.T, campaignCriteriaH, adGroupCriteriaH http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			okAdGroupAd(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			campaignCriteriaH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			adGroupCriteriaH(w, r)
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

// capturedMutate is a criteria handler that records the request body and replies with
// `count` results whose resource names are built by `name`.
func capturedMutate(body *string, count int, name func(i int) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		parts := make([]string, 0, count)
		for i := 0; i < count; i++ {
			parts = append(parts, `{"resourceName":"`+name(i)+`"}`)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
}

// Criterion ids are NUMERIC in both composite resource names (compositeResourceID
// rejects a non-numeric half), so the fixtures must be too — a letter suffix here
// exercises the rejection path, not the happy path.
func campaignCriterionName(i int) string {
	return "customers/1234567890/campaignCriteria/222~" + strconv.Itoa(900+i)
}

func adGroupCriterionName(i int) string {
	return "customers/1234567890/adGroupCriteria/333~" + strconv.Itoa(900+i)
}

func failHandler(t *testing.T, label string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("unexpected call to %s", label)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// validateGeoTargets
// ---------------------------------------------------------------------------

// Each code must resolve to ITS OWN constant, not merely to "some" constant. A map
// that returned 2840 for everything would satisfy a bare "resolves without error"
// assertion while targeting the United States for every campaign in the world.
func TestValidateGeoTargets_ResolvesEachCodeToItsOwnConstant(t *testing.T) {
	got, err := validateGeoTargets([]string{"US", "GB", "DE", "JP"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"2840", "2826", "2276", "2392"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q (a mismatch here targets the WRONG COUNTRY)", i, got[i], want[i])
		}
	}
}

func TestValidateGeoTargets_NormalisesCaseAndWhitespaceAndDedupes(t *testing.T) {
	got, err := validateGeoTargets([]string{" us ", "Us", "gb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "2840" || got[1] != "2826" {
		t.Fatalf("got %v, want [2840 2826]", got)
	}
}

// The guard that makes this ticket's fix real: an unmapped code must be REFUSED, not
// dropped. Dropping it creates a campaign with no criteria that spends worldwide while
// reporting success — the exact defect being fixed.
func TestValidateGeoTargets_RejectsUnmappedCode(t *testing.T) {
	for _, code := range []string{"USA", "XX", "U", "US/CA"} {
		if _, err := validateGeoTargets([]string{code}); err == nil {
			t.Errorf("code %q: expected an error, got nil (an unmapped code must never be silently dropped)", code)
		}
	}
}

func TestValidateGeoTargets_RejectsEmptyEntry(t *testing.T) {
	if _, err := validateGeoTargets([]string{"US", "  "}); err == nil {
		t.Fatal("expected an error for a blank geo target")
	}
}

// Empty input is the pre-LFXV2-3283 behaviour and must stay a clean no-op, so callers
// that predate the field keep working.
func TestValidateGeoTargets_EmptyIsNoError(t *testing.T) {
	got, err := validateGeoTargets(nil)
	if err != nil || got != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestGeoTargetResource_RendersConstantPath(t *testing.T) {
	if got := geoTargetResource("2840"); got != "geoTargetConstants/2840" {
		t.Fatalf("got %q, want geoTargetConstants/2840", got)
	}
}

// ---------------------------------------------------------------------------
// Search: campaign-level criteria
// ---------------------------------------------------------------------------

// The body assertion this ticket turns on. Distinctive per-country constants (US=2840,
// JP=2392) must each land in their OWN operation's location.geoTargetConstant, under
// `campaign` — not swapped, not collapsed, and not in the adGroup field.
func TestCreateCampaign_AttachesCampaignLevelGeoCriteria(t *testing.T) {
	var body string
	c := newGeoClient(t,
		capturedMutate(&body, 2, campaignCriterionName),
		failHandler(t, "adGroupCriteria:mutate (Search geo must attach at CAMPAIGN level)"))

	in := sampleInput()
	in.GeoTargets = []string{"US", "JP"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		Operations []struct {
			Create struct {
				Campaign string `json:"campaign"`
				AdGroup  string `json:"adGroup"`
				Location *struct {
					GeoTargetConstant string `json:"geoTargetConstant"`
				} `json:"location"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode captured body: %v (body=%s)", err, body)
	}
	if len(req.Operations) != 2 {
		t.Fatalf("got %d operations, want 2 (body=%s)", len(req.Operations), body)
	}

	want := []string{"geoTargetConstants/2840", "geoTargetConstants/2392"}
	for i, op := range req.Operations {
		if op.Create.Location == nil {
			t.Fatalf("operation %d carries no location", i)
		}
		if op.Create.Location.GeoTargetConstant != want[i] {
			t.Errorf("operation %d: geoTargetConstant = %q, want %q", i, op.Create.Location.GeoTargetConstant, want[i])
		}
		if op.Create.Campaign != "customers/1234567890/campaigns/222" {
			t.Errorf("operation %d: campaign = %q, want the created campaign resource", i, op.Create.Campaign)
		}
		// The level is the trap this ticket names: an adGroup field here means the
		// Search payload was built with the Demand Gen shape.
		if op.Create.AdGroup != "" {
			t.Errorf("operation %d: adGroup = %q, want empty (Search criteria attach at CAMPAIGN level)", i, op.Create.AdGroup)
		}
	}

	if len(res.GeoCriterionIDs) != 2 {
		t.Errorf("GeoCriterionIDs = %v, want 2 ids", res.GeoCriterionIDs)
	}
}

// No geo targets must send NO criteria request at all — the failHandler proves it,
// preserving the behaviour of every caller predating the field.
func TestCreateCampaign_NoGeoTargetsSendsNoCriteriaRequest(t *testing.T) {
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate"),
		failHandler(t, "adGroupCriteria:mutate"))

	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.GeoCriterionIDs) != 0 {
		t.Errorf("GeoCriterionIDs = %v, want empty", res.GeoCriterionIDs)
	}
}

// An unmapped code must fail BEFORE the budget mutate — nothing paid may exist. The
// failing handlers on every endpoint prove no upstream call was made at all.
func TestCreateCampaign_UnmappedGeoFailsBeforeAnyMutate(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream call may be made for an invalid geo target, got %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))

	in := sampleInput()
	in.GeoTargets = []string{"USA"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for an unmapped geo target")
	}
	if res != nil {
		t.Errorf("expected a nil result (nothing was created), got %+v", res)
	}
	if !strings.Contains(err.Error(), "USA") {
		t.Errorf("error should name the offending code, got %v", err)
	}
}

// ValidateCampaignInput backs the ADOPTION path, which returns before CreateCampaign
// runs. Without geo validation there, the same input would be accepted or refused
// depending on whether a same-name campaign happened to exist.
func TestValidateCampaignInput_RejectsUnmappedGeo(t *testing.T) {
	c := newGeoClient(t, failHandler(t, "campaignCriteria"), failHandler(t, "adGroupCriteria"))
	in := sampleInput()
	in.GeoTargets = []string{"XX"}
	if err := c.ValidateCampaignInput(in); err == nil {
		t.Fatal("expected ValidateCampaignInput to reject an unmapped geo target")
	}
}

// A geo failure after the campaign exists must return the campaign ALONGSIDE the error,
// never (nil, err) — the campaign is real and spends, so the claim must be retained.
func TestCreateCampaign_GeoFailureKeepsCampaignPartial(t *testing.T) {
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.GeoTargets = []string{"US"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error")
	}
	if res == nil {
		t.Fatal("expected a non-nil partial result: the campaign exists upstream and must stay reconcilable")
	}
	if res.CampaignID != "222" {
		t.Errorf("CampaignID = %q, want the created campaign id", res.CampaignID)
	}
}

// ---------------------------------------------------------------------------
// Demand Gen: ad-group-level criteria
// ---------------------------------------------------------------------------

// The other half of the ticket's trap: Demand Gen rejects campaign-level location
// criteria, so the payload must carry `adGroup` and must NOT be sent to
// campaignCriteria:mutate.
func TestCreateDemandGenCampaign_AttachesAdGroupLevelGeoCriteria(t *testing.T) {
	var body string
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate (Demand Gen REJECTS campaign-level location criteria)"),
		capturedMutate(&body, 2, adGroupCriterionName))

	in := sampleInput()
	in.GeoTargets = []string{"DE", "BR"}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		Operations []struct {
			Create struct {
				AdGroup  string `json:"adGroup"`
				Campaign string `json:"campaign"`
				Location *struct {
					GeoTargetConstant string `json:"geoTargetConstant"`
				} `json:"location"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode captured body: %v (body=%s)", err, body)
	}
	if len(req.Operations) != 2 {
		t.Fatalf("got %d operations, want 2 (body=%s)", len(req.Operations), body)
	}

	want := []string{"geoTargetConstants/2276", "geoTargetConstants/2076"}
	for i, op := range req.Operations {
		if op.Create.Location == nil {
			t.Fatalf("operation %d carries no location", i)
		}
		if op.Create.Location.GeoTargetConstant != want[i] {
			t.Errorf("operation %d: geoTargetConstant = %q, want %q", i, op.Create.Location.GeoTargetConstant, want[i])
		}
		if op.Create.AdGroup != "customers/1234567890/adGroups/333" {
			t.Errorf("operation %d: adGroup = %q, want the created ad group resource", i, op.Create.AdGroup)
		}
		if op.Create.Campaign != "" {
			t.Errorf("operation %d: campaign = %q, want empty (Demand Gen criteria attach at AD GROUP level)", i, op.Create.Campaign)
		}
	}

	if len(res.GeoCriterionIDs) != 2 {
		t.Errorf("GeoCriterionIDs = %v, want 2 ids", res.GeoCriterionIDs)
	}
}

// The closing step used to say "no geo targeting set" unconditionally. Once geo is
// applied that string is a lie, and a step list is what an operator reads.
func TestCreateDemandGenCampaign_ClosingStepReportsGeoHonestly(t *testing.T) {
	var body string
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate"),
		capturedMutate(&body, 1, adGroupCriterionName))

	in := sampleInput()
	in.GeoTargets = []string{"US"}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(res.Steps, "\n")
	if strings.Contains(joined, "no geo targeting set") {
		t.Errorf("steps still claim no geo targeting after applying it:\n%s", joined)
	}
	if !strings.Contains(joined, "Geo targeting applied") {
		t.Errorf("steps do not report the applied geo targeting:\n%s", joined)
	}
}

// ...and the untargeted case must KEEP saying so, so the absence is never read as
// targeting that was applied.
func TestCreateDemandGenCampaign_UntargetedStillSaysNoGeo(t *testing.T) {
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate"),
		failHandler(t, "adGroupCriteria:mutate"))

	res, err := c.CreateDemandGenCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "no geo targeting set") {
		t.Errorf("an untargeted Demand Gen create must still say so, got:\n%s", strings.Join(res.Steps, "\n"))
	}
}

// A criterion resource name reporting a DIFFERENT ad group does not describe what this
// call created, so its ids must not be trusted or persisted.
func TestCreateDemandGenCampaign_WrongAdGroupInCriterionNameIsUnconfirmed(t *testing.T) {
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate"),
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/999~901"}]}`)
		})

	in := sampleInput()
	in.GeoTargets = []string{"US"}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error for a criterion naming a different ad group")
	}
	// Assert the SPECIFIC error, not merely that one occurred: several unrelated guards
	// in this path also return non-nil, so a bare err != nil check would stay green with
	// the ad-group-mismatch guard deleted.
	if !strings.Contains(err.Error(), "reports a different ad group id") {
		t.Errorf("error should name the ad-group mismatch, got %v", err)
	}
	if res == nil {
		t.Fatal("expected a non-nil partial result")
	}
	if len(res.GeoCriterionIDs) != 0 {
		t.Errorf("GeoCriterionIDs = %v, want empty (the ids were not trustworthy)", res.GeoCriterionIDs)
	}
}

// A short mutate response (fewer results than operations) means an unknown number of
// criteria committed — it must be UNCONFIRMED, not a silent partial success.
func TestCreateCampaign_ShortGeoMutateResponseIsUnconfirmed(t *testing.T) {
	var body string
	c := newGeoClient(t,
		capturedMutate(&body, 1, campaignCriterionName), // 1 result for 2 operations
		failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.GeoTargets = []string{"US", "GB"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an UNCONFIRMED error for a short mutate response")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error should be UNCONFIRMED, got %v", err)
	}
	if res == nil || len(res.GeoCriterionIDs) != 0 {
		t.Errorf("no geo ids may be reported for a short response, got %+v", res)
	}
}

// A mutating create must NEVER be retried on 429: there is no idempotency key, so a
// retry double-creates a paid resource. The location criteria calls are mutating, so a
// 429 must be surfaced after exactly ONE attempt rather than backed off and repeated.
func TestCreateCampaign_GeoCriteria429IsNotRetried(t *testing.T) {
	var attempts int
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":8,"status":"RESOURCE_EXHAUSTED"}}`)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.GeoTargets = []string{"US"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for a 429 on the criteria mutate")
	}
	if attempts != 1 {
		t.Errorf("campaignCriteria:mutate was attempted %d times; a mutating create must never be retried (no idempotency key — a retry double-creates a paid resource)", attempts)
	}
	if res == nil {
		t.Fatal("expected a non-nil partial result: the campaign exists upstream")
	}
}

// The same, for the Demand Gen (ad-group) level.
func TestCreateDemandGenCampaign_GeoCriteria429IsNotRetried(t *testing.T) {
	var attempts int
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate"),
		func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":8,"status":"RESOURCE_EXHAUSTED"}}`)
		})

	in := sampleInput()
	in.GeoTargets = []string{"US"}
	if _, err := c.CreateDemandGenCampaign(context.Background(), in); err == nil {
		t.Fatal("expected an error for a 429 on the criteria mutate")
	}
	if attempts != 1 {
		t.Errorf("adGroupCriteria:mutate was attempted %d times; a mutating create must never be retried", attempts)
	}
}
