// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// interestRejectingServers mimics the LIVE API's actual behaviour: the ad-group create is
// refused with a 400 naming "invalid interests" whenever the payload carries an `interests`
// key, and accepted once it does not. `reject` selects which dimensions the 400 names, so a
// test can reproduce interests alone, communities alone, or both in one response.
func interestRejectingServers(t *testing.T, reject string) (*Client, func() []map[string]any, func()) {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))

	var mu sync.Mutex
	var adGroupBodies []map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(r.URL.Path, "/ad_groups"):
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			mu.Lock()
			adGroupBodies = append(adGroupBodies, env.Data)
			mu.Unlock()

			targeting, _ := env.Data["targeting"].(map[string]any)
			_, hasInterests := targeting["interests"]
			_, hasCommunities := targeting["communities"]
			// Reject only what this server was told to reject AND what the payload carries,
			// so the retry (which carries neither) is accepted.
			var fields []string
			if hasInterests && strings.Contains(reject, "interests") {
				fields = append(fields, `{"field":"interests","message":"You cannot set invalid interests: {'Machine Learning'}."}`)
			}
			if hasCommunities && strings.Contains(reject, "communities") {
				fields = append(fields, `{"field":"communities","message":"invalid communities"}`)
			}
			if len(fields) > 0 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Bad request","fields":[` + strings.Join(fields, ",") + `]}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_2"}})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(mux)

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	bodies := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), adGroupBodies...)
	}
	return c, bodies, func() { apiSrv.Close(); tokenSrv.Close() }
}

func interestInput() CampaignInput {
	return CampaignInput{
		EventName:       "KubeCon",
		Project:         "tlf",
		RegistrationURL: "https://example.com/reg",
		BudgetUSD:       100,
		StartDate:       "2026-09-01",
		EndDate:         "2026-09-10",
		GeoTargets:      []string{"us"},
		Keywords:        []string{"k8s"},
		Objective:       "traffic",
		// The shape the brief generator actually produces: human-readable LABELS, where
		// Reddit's API wants opaque ids ("technology_v3"). Every AI-generated brief sends
		// these, which is why this path is the common case rather than an edge case.
		Interests: []string{"Artificial Intelligence", "Machine Learning"},
	}
}

// The regression this fixes. Reddit rejected the ad group over invalid interests, the client
// gave up, and the PAUSED campaign created moments earlier was ORPHANED -- a real paid object
// with nothing pointing at it. Dropping one targeting dimension is recoverable in Reddit Ads
// Manager; an orphaned campaign is not.
func TestCreateCampaign_RetriesWithoutInvalidInterests(t *testing.T) {
	c, bodies, cleanup := interestRejectingServers(t, "interests")
	defer cleanup()

	res, err := c.CreateCampaign(context.Background(), interestInput())
	if err != nil {
		t.Fatalf("an invalid-interests 400 must be recovered, not orphan the campaign: %v", err)
	}
	if res.AdGroupID != "ag_2" {
		t.Errorf("AdGroupID = %q, want ag_2 -- the retry must succeed", res.AdGroupID)
	}

	sent := bodies()
	if len(sent) != 2 {
		t.Fatalf("expected exactly 2 ad-group POSTs (the rejected one and the retry), got %d", len(sent))
	}
	// The FIRST attempt must carry the interests: a client that never sent them would pass
	// this test while silently dropping targeting Reddit would have accepted.
	first, _ := sent[0]["targeting"].(map[string]any)
	if _, ok := first["interests"]; !ok {
		t.Error("the first attempt must carry interests; dropping them unconditionally would lose targeting Reddit accepts for valid ids")
	}
	// The RETRY must not.
	second, _ := sent[1]["targeting"].(map[string]any)
	if _, ok := second["interests"]; ok {
		t.Error("the retry still carries interests -- it would fail again for the same reason")
	}
}

// The operator has to learn WHICH dimension was lost: interests and communities are re-added
// in different places in Reddit Ads Manager, so "some targeting was skipped" is not actionable.
func TestCreateCampaign_ReportsDroppedInterests(t *testing.T) {
	c, _, cleanup := interestRejectingServers(t, "interests")
	defer cleanup()

	res, err := c.CreateCampaign(context.Background(), interestInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	joined := strings.Join(res.Steps, " | ")
	if !strings.Contains(joined, "interests") {
		t.Errorf("the steps never mention interests, so nobody learns the targeting was dropped: %v", res.Steps)
	}
	if !strings.Contains(joined, "add manually") {
		t.Errorf("the steps do not tell the operator to re-add it manually: %v", res.Steps)
	}
}

// Both dimensions can be named in ONE 400. A per-dimension retry would send a second doomed
// request, and every extra ad-group POST is another chance to create one we lose track of.
func TestCreateCampaign_OneRetryDropsBothRejectedDimensions(t *testing.T) {
	c, bodies, cleanup := interestRejectingServers(t, "interests communities")
	defer cleanup()

	in := interestInput()
	in.Subreddits = []string{"kubernetes"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("a 400 naming both dimensions must be recovered in one retry: %v", err)
	}
	sent := bodies()
	if len(sent) != 2 {
		t.Fatalf("expected exactly 2 ad-group POSTs, got %d -- a per-dimension retry sends a second doomed request", len(sent))
	}
	retry, _ := sent[1]["targeting"].(map[string]any)
	if _, ok := retry["interests"]; ok {
		t.Error("the retry still carries interests")
	}
	if _, ok := retry["communities"]; ok {
		t.Error("the retry still carries communities")
	}
	joined := strings.Join(res.Steps, " | ")
	if !strings.Contains(joined, "interests") || !strings.Contains(joined, "communities") {
		t.Errorf("both dropped dimensions must be named for the operator: %v", res.Steps)
	}
}

// A 400 that names NEITHER dimension is a different failure, and retrying it would be a blind
// re-POST that could duplicate the ad group. It must propagate with the campaign reported as
// created, so the orphan is recorded rather than silently retried.
func TestCreateCampaign_DoesNotRetryAnUnrelated400(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	var mu sync.Mutex
	adGroupPosts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ad_accounts/t2_test") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(r.URL.Path, "/ad_groups"):
			mu.Lock()
			adGroupPosts++
			mu.Unlock()
			// A 400 about something the fallback has no answer for.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Bad request","fields":[{"field":"bid_strategy","message":"This field is required"}]}}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(mux)
	defer apiSrv.Close()

	c := NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	res, err := c.CreateCampaign(context.Background(), interestInput())
	if err == nil {
		t.Fatal("a 400 the fallback cannot address must propagate")
	}
	mu.Lock()
	posts := adGroupPosts
	mu.Unlock()
	if posts != 1 {
		t.Errorf("ad-group POSTs = %d, want 1 -- an unrelated 400 must not be blindly retried", posts)
	}
	// The campaign DID get created, and the partial result is what records the orphan. Losing
	// it here would leave a paid campaign upstream that nothing points at.
	if res == nil || res.CampaignID != "camp_1" {
		t.Errorf("the partial result must carry the created campaign id so the orphan is recorded, got %+v", res)
	}
}

// The SURVIVING-interests case, which no test reached before: communities are rejected while
// interests are accepted, so the retry keeps interests and the Steps trail must say so.
//
// It matters more than a reporting detail. Every fallback fixture rejects interests by
// construction, so the branch reporting interests as kept was unreachable — and it is the
// only line telling an operator their interest targeting survived. A claim no test and (per
// LFXV2-3261) no production run has ever validated is exactly the kind that drifts.
func TestCreateCampaign_KeepsInterestsWhenOnlyCommunitiesAreRejected(t *testing.T) {
	c, bodies, cleanup := interestRejectingServers(t, "communities")
	defer cleanup()

	in := interestInput()
	in.Subreddits = []string{"kubernetes"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	sent := bodies()
	if len(sent) != 2 {
		t.Fatalf("expected 2 ad-group POSTs (rejected + retry), got %d", len(sent))
	}
	// The retry drops BOTH by design — baseTargeting carries neither — so interests are
	// genuinely gone from the wire even though only communities were named. The Steps trail
	// must therefore report interests as DROPPED, not as surviving. Asserting this pins the
	// deliberate trade: one retry, both dimensions, honestly reported.
	retry, _ := sent[1]["targeting"].(map[string]any)
	if _, ok := retry["interests"]; ok {
		t.Error("the retry still carries interests")
	}

	joined := strings.Join(res.Steps, " | ")
	if strings.Count(joined, "Targeting:") != 1 {
		t.Errorf("expected exactly one \"Targeting:\" step, got %d in %v", strings.Count(joined, "Targeting:"), res.Steps)
	}
	if !strings.Contains(joined, "interests") {
		t.Errorf("the steps must account for interests either way: %v", res.Steps)
	}
}

// The genuinely-surviving case: interests supplied, NOTHING rejected. This is the branch the
// single "Targeting:" line exists to serve, and the one no fixture could reach while every
// server rejected interests unconditionally.
func TestCreateCampaign_ReportsSurvivingInterests(t *testing.T) {
	c, _, cleanup := interestRejectingServers(t, "nothing-rejected")
	defer cleanup()

	res, err := c.CreateCampaign(context.Background(), interestInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	joined := strings.Join(res.Steps, " | ")
	if strings.Count(joined, "Targeting:") != 1 {
		t.Errorf("expected exactly one \"Targeting:\" step, got %d in %v", strings.Count(joined, "Targeting:"), res.Steps)
	}
	if !strings.Contains(joined, "2 interests") {
		t.Errorf("surviving interests must be reported in the targeting line: %v", res.Steps)
	}
	if strings.Contains(joined, "skipped") {
		t.Errorf("nothing was rejected, so nothing should be reported as skipped: %v", res.Steps)
	}
}

// REJECTED and DROPPED are different facts and the Steps trail must not conflate them.
//
// Reddit names one or both dimensions in its 400; the retry then drops BOTH regardless,
// because baseTargeting carries neither. An earlier version reported "retrying without
// communities" while also dropping interests, which told the operator Reddit had refused
// something it never saw — and interests and communities are re-added in different places
// in Reddit Ads Manager, so a wrong attribution sends them to the wrong screen.
func TestCreateCampaign_DistinguishesRejectedFromCollateralDrops(t *testing.T) {
	// Only communities are rejected; interests are supplied and therefore collateral.
	c, _, cleanup := interestRejectingServers(t, "communities")
	defer cleanup()

	in := interestInput()
	in.Subreddits = []string{"kubernetes"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	joined := strings.Join(res.Steps, " | ")

	if !strings.Contains(joined, "rejected communities") {
		t.Errorf("the step must name what Reddit actually rejected: %v", res.Steps)
	}
	// The load-bearing half: interests were NOT rejected, so they must be reported as
	// dropped alongside rather than as something Reddit refused.
	if !strings.Contains(joined, "also dropping interests") {
		t.Errorf("interests were dropped as collateral and must be reported as such, not as a Reddit rejection: %v", res.Steps)
	}
	if strings.Contains(joined, "rejected interests") {
		t.Errorf("interests were never rejected by Reddit; reporting them as rejected misattributes the failure: %v", res.Steps)
	}
}
