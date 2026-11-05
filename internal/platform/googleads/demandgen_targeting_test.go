// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDemandGenAudience is an Audience under testAccount()'s customer id.
const testDemandGenAudience = "customers/1234567890/audiences/4242"

// ---------------------------------------------------------------------------
// validateDemandGenAudience
// ---------------------------------------------------------------------------

func TestValidateDemandGenAudience_AcceptsThisAccountsAudience(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"absent":    {"", ""},
		"canonical": {testDemandGenAudience, testDemandGenAudience},
		"trimmed":   {"  " + testDemandGenAudience + "\n", testDemandGenAudience},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := validateDemandGenAudience(campaignKindDemandGen, "1234567890", tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Every malformed spelling is refused before any request. Google would refuse most of
// these too, but only at the criteria mutate — after the budget, campaign and ad group.
func TestValidateDemandGenAudience_RefusesMalformedNames(t *testing.T) {
	for name, tc := range map[string]struct{ in, wantSub string }{
		"blank":                {"   ", "must not be blank"},
		"bare id":              {"4242", "not a customers/<id>/audiences/<id>"},
		"user list":            {"customers/1234567890/userLists/4242", "not a customers/<id>/audiences/<id>"},
		"custom audience":      {"customers/1234567890/customAudiences/4242", "not a customers/<id>/audiences/<id>"},
		"zero id":              {"customers/1234567890/audiences/0", "positive canonical id"},
		"leading zero":         {"customers/1234567890/audiences/04242", "positive canonical id"},
		"trailing segment":     {"customers/1234567890/audiences/4242/x", "not a customers/<id>/audiences/<id>"},
		"non-numeric id":       {"customers/1234567890/audiences/abc", "not a customers/<id>/audiences/<id>"},
		"non-numeric customer": {"customers/abc/audiences/4242", "not a customers/<id>/audiences/<id>"},
		// INVALID_CUSTOMER_ID upstream — but only after the budget (validateOnly probe,
		// 2026-11-03), so it must be refused here.
		"another account": {"customers/9999999999/audiences/4242", "belongs to customer 9999999999"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateDemandGenAudience(campaignKindDemandGen, "1234567890", tc.in)
			if err == nil {
				t.Fatalf("%q must be refused", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// Off Demand Gen the field is refused rather than dropped: no other cascade reads it,
// so accepting it would report a targeted campaign that is not. Driven through the
// whole preflight on every other kind, so the refusal is proven reachable — not just
// present in the helper — and names the channel.
func TestPreflightRefusesDemandGenAudienceOffDemandGen(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for _, kind := range []string{campaignKindSearch, campaignKindPerformanceMax, campaignKindVideo, campaignKindDisplay} {
		t.Run(kind, func(t *testing.T) {
			in := demandGenInput()
			if kind == campaignKindPerformanceMax {
				in.PerformanceMaxCreative = fullPMaxCreative()
			}
			if _, err := c.preflightCampaignKind(kind, in); err != nil {
				t.Fatalf("the base input must pass on %s, got %v", kind, err)
			}
			in.DemandGenAudience = testDemandGenAudience
			_, err := c.preflightCampaignKind(kind, in)
			if err == nil {
				t.Fatalf("preflight(%s) accepted DemandGenAudience; it must refuse rather than drop", kind)
			}
			if !strings.Contains(err.Error(), "a Demand Gen audience is not supported on "+kind) {
				t.Errorf("error = %q, want the Demand Gen audience refusal naming %s", err, kind)
			}
		})
	}
}

// The preflight carries the validated name to the cascade, and refuses a foreign one on
// the Demand Gen kind itself (the adoption path runs this same preflight).
func TestPreflightCarriesTheDemandGenAudience(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	in := demandGenInput()
	in.DemandGenAudience = " " + testDemandGenAudience + " "
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if pf.demandGenAudience != testDemandGenAudience {
		t.Errorf("pf.demandGenAudience = %q, want %q", pf.demandGenAudience, testDemandGenAudience)
	}
	in.DemandGenAudience = "customers/9999999999/audiences/4242"
	if _, err := c.preflightCampaignKind(campaignKindDemandGen, in); err == nil {
		t.Error("a foreign-account Audience must be refused at preflight")
	}
}

// AudienceSegments stay refused on Demand Gen, but the message now says WHY — the ad
// group is audience-grouped — and names the field that does work there.
func TestPreflightRefusesUserListsOnDemandGenPointingAtTheAudience(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	in := demandGenInput()
	in.AudienceSegments = []string{"customers/1234567890/userLists/123"}
	_, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err == nil {
		t.Fatal("user lists must be refused on Demand Gen")
	}
	for _, want := range []string{"audience segments are not supported on " + campaignKindDemandGen, "audience-grouped", "DemandGenAudience"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	// The other refusing channels keep their own reason: the audience-grouped sentence is
	// about Demand Gen only.
	for _, kind := range []string{campaignKindVideo, campaignKindPerformanceMax} {
		_, err = c.preflightCampaignKind(kind, in)
		if err == nil || !strings.Contains(err.Error(), "audience segments are not supported on "+kind) || strings.Contains(err.Error(), "audience-grouped") {
			t.Errorf("%s must refuse user lists with its own reason, got %v", kind, err)
		}
	}
	// Display is the one non-Search channel that ACCEPTS them (#462): it attaches user
	// lists as targeting criteria, so the Demand Gen refusal must not leak onto it.
	pf, err := c.preflightCampaignKind(campaignKindDisplay, in)
	if err != nil {
		t.Fatalf("Display must accept user lists (#462), got %v", err)
	}
	if got := pf.displayTargeting.userLists; len(got) != 1 || got[0] != in.AudienceSegments[0] {
		t.Errorf("pf.displayTargeting.userLists = %v, want %v", got, in.AudienceSegments)
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

// dgTargetingRig serves the Demand Gen cascade and records every request. The
// adGroupCriteria handler is the test's: call is 0 for the first adGroupCriteria
// request, 1 for the second. Handlers never call t.Fatal (they run on the server's
// goroutine).
type dgTargetingRig struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (r *dgTargetingRig) criteriaBodies() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for i, p := range r.paths {
		if strings.HasSuffix(p, "adGroupCriteria:mutate") {
			out = append(out, r.bodies[i])
		}
	}
	return out
}

func newDGTargetingRig(t *testing.T, criteria func(call int, body []byte, w http.ResponseWriter)) (*dgTargetingRig, *Client) {
	t.Helper()
	rig := &dgTargetingRig{}
	criteriaCalls := 0
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		rig.paths = append(rig.paths, r.URL.Path)
		rig.bodies = append(rig.bodies, body)
		call := -1
		if strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate") {
			call = criteriaCalls
			criteriaCalls++
		}
		rig.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case call >= 0:
			criteria(call, body, w)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	return rig, NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
}

// echoCriteria answers one adGroupCriterion per requested operation, ids 900, 901, …
// on ad group 333 — Google's composite {adGroupId}~{criterionId} shape.
func echoCriteria(body []byte, w http.ResponseWriter) {
	var req struct {
		Operations []json.RawMessage `json:"operations"`
	}
	_ = json.Unmarshal(body, &req)
	parts := make([]string, 0, len(req.Operations))
	for i := range req.Operations {
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/adGroupCriteria/333~%d"}`, 900+i))
	}
	_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("decode sent body: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("sent body\n  %s\nwant\n  %s", got, want)
	}
}

// The whole payload, exactly: geo keeps its own mutate unchanged, and the audience and
// languages follow in ONE adGroupCriteria:mutate — audience first, languages in order,
// no status and no other arm — on the ad group just created. Nothing goes to
// campaignCriteria: a campaign-level language is refused on Demand Gen.
func TestCreateDemandGenCampaign_AttachesAudienceAndLanguagesToTheAdGroup(t *testing.T) {
	rig, c := newDGTargetingRig(t, func(_ int, body []byte, w http.ResponseWriter) { echoCriteria(body, w) })
	in := demandGenInput()
	in.GeoTargets = []string{"US"}
	in.DemandGenAudience = testDemandGenAudience
	in.Languages = []string{"EN", "de"}

	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}

	bodies := rig.criteriaBodies()
	if len(bodies) != 2 {
		t.Fatalf("got %d adGroupCriteria mutates, want 2 (geo, then audience+languages)", len(bodies))
	}
	jsonEqual(t, bodies[0], `{"operations":[{"create":{"adGroup":"customers/1234567890/adGroups/333","location":{"geoTargetConstant":"geoTargetConstants/2840"}}}]}`)
	jsonEqual(t, bodies[1], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/333","audience":{"audience":"customers/1234567890/audiences/4242"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","language":{"languageConstant":"languageConstants/1000"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","language":{"languageConstant":"languageConstants/1001"}}}]}`)
	rig.mu.Lock()
	for _, p := range rig.paths {
		if strings.HasSuffix(p, "campaignCriteria:mutate") {
			t.Errorf("Demand Gen must send no campaign criteria, got %s", p)
		}
	}
	rig.mu.Unlock()

	if !reflect.DeepEqual(res.AudienceCriteriaIDs, []string{"900"}) {
		t.Errorf("AudienceCriteriaIDs = %v, want [900]", res.AudienceCriteriaIDs)
	}
	if !reflect.DeepEqual(res.TargetingCriterionIDs, []string{"901", "902"}) {
		t.Errorf("TargetingCriterionIDs = %v, want [901 902]", res.TargetingCriterionIDs)
	}
	wantStep := "Audience and language targeting applied: 3 ad-group criteria (audience customers/1234567890/audiences/4242; 2 language(s))"
	if !hasExactStep(res.Steps, wantStep) {
		t.Errorf("steps = %q, want %q", res.Steps, wantStep)
	}
}

// Each half alone: the mutate carries only what was asked for, the ids land in the right
// field, and the step names only that half.
func TestCreateDemandGenCampaign_AudienceOrLanguagesAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		audience     string
		languages    []string
		wantBody     string
		wantAudience []string
		wantLanguage []string
		wantStep     string
	}{
		"audience only": {
			audience:     testDemandGenAudience,
			wantBody:     `{"operations":[{"create":{"adGroup":"customers/1234567890/adGroups/333","audience":{"audience":"customers/1234567890/audiences/4242"}}}]}`,
			wantAudience: []string{"900"},
			wantStep:     "Audience and language targeting applied: 1 ad-group criteria (audience customers/1234567890/audiences/4242)",
		},
		"languages only": {
			languages:    []string{"JA"},
			wantBody:     `{"operations":[{"create":{"adGroup":"customers/1234567890/adGroups/333","language":{"languageConstant":"languageConstants/1005"}}}]}`,
			wantLanguage: []string{"900"},
			wantStep:     "Audience and language targeting applied: 1 ad-group criteria (1 language(s))",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig, c := newDGTargetingRig(t, func(_ int, body []byte, w http.ResponseWriter) { echoCriteria(body, w) })
			in := demandGenInput()
			in.DemandGenAudience = tc.audience
			in.Languages = tc.languages
			res, err := c.CreateDemandGenCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateDemandGenCampaign: %v", err)
			}
			bodies := rig.criteriaBodies()
			if len(bodies) != 1 {
				t.Fatalf("got %d adGroupCriteria mutates, want 1", len(bodies))
			}
			jsonEqual(t, bodies[0], tc.wantBody)
			if !reflect.DeepEqual(res.AudienceCriteriaIDs, tc.wantAudience) {
				t.Errorf("AudienceCriteriaIDs = %v, want %v", res.AudienceCriteriaIDs, tc.wantAudience)
			}
			if !reflect.DeepEqual(res.TargetingCriterionIDs, tc.wantLanguage) {
				t.Errorf("TargetingCriterionIDs = %v, want %v", res.TargetingCriterionIDs, tc.wantLanguage)
			}
			if !hasExactStep(res.Steps, tc.wantStep) {
				t.Errorf("steps = %q, want %q", res.Steps, tc.wantStep)
			}
		})
	}
}

// Neither asked for: no criteria mutate at all, so every Demand Gen create before #459
// sends exactly what it sent.
func TestCreateDemandGenCampaign_NoAudienceOrLanguageSendsNoCriteria(t *testing.T) {
	rig, c := newDGTargetingRig(t, func(_ int, body []byte, w http.ResponseWriter) {
		t.Errorf("no adGroupCriteria mutate expected, got %s", body)
		http.Error(w, "unexpected", http.StatusBadRequest)
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), demandGenInput())
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if len(rig.criteriaBodies()) != 0 || res.AudienceCriteriaIDs != nil || res.TargetingCriterionIDs != nil {
		t.Errorf("expected no criteria, got bodies=%d audience=%v languages=%v", len(rig.criteriaBodies()), res.AudienceCriteriaIDs, res.TargetingCriterionIDs)
	}
}

// After the ad group exists every failure is a PARTIAL result: non-nil, carrying the
// campaign and ad group ids and the geo already attached, with no criterion ids
// recorded. A definite 4xx is a plain failure; a 5xx, a short 2xx and a 2xx naming the
// wrong ad group or account are UNCONFIRMED, because the criteria may exist.
func TestCreateDemandGenCampaign_AudienceLanguageFailureIsAPartialResult(t *testing.T) {
	for name, tc := range map[string]struct {
		respond         func(w http.ResponseWriter)
		wantUnconfirmed bool
	}{
		"definite 4xx": {
			respond: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, oneAudienceFailureBody)
			},
		},
		"5xx": {
			respond:         func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
			wantUnconfirmed: true,
		},
		"short 2xx": {
			respond: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~900"}]}`)
			},
			wantUnconfirmed: true,
		},
		"another ad group": {
			respond: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/444~900"},{"resourceName":"customers/1234567890/adGroupCriteria/444~901"}]}`)
			},
			wantUnconfirmed: true,
		},
		"another account": {
			respond: func(w http.ResponseWriter) {
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/9999999999/adGroupCriteria/333~900"},{"resourceName":"customers/9999999999/adGroupCriteria/333~901"}]}`)
			},
			wantUnconfirmed: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, c := newDGTargetingRig(t, func(call int, body []byte, w http.ResponseWriter) {
				if call == 0 { // geo succeeds
					echoCriteria(body, w)
					return
				}
				tc.respond(w)
			})
			in := demandGenInput()
			in.GeoTargets = []string{"US"}
			in.DemandGenAudience = testDemandGenAudience
			in.Languages = []string{"EN"}

			res, err := c.CreateDemandGenCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected an error from the audience/language step")
			}
			if res == nil {
				t.Fatal("expected a NON-NIL partial: the campaign and ad group exist")
			}
			if res.CampaignID != "222" || res.AdGroupID != "333" {
				t.Errorf("partial = campaign %q, ad group %q; want 222 and 333", res.CampaignID, res.AdGroupID)
			}
			if !reflect.DeepEqual(res.GeoCriterionIDs, []string{"900"}) {
				t.Errorf("GeoCriterionIDs = %v, want the geo already attached [900]", res.GeoCriterionIDs)
			}
			if res.AudienceCriteriaIDs != nil || res.TargetingCriterionIDs != nil {
				t.Errorf("no criterion id may be recorded from a failed step, got audience=%v languages=%v", res.AudienceCriteriaIDs, res.TargetingCriterionIDs)
			}
			if !strings.Contains(err.Error(), "demand gen audience/language targeting") {
				t.Errorf("error = %v, want it to name the audience/language step", err)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.wantUnconfirmed {
				t.Errorf("UNCONFIRMED in error = %v, want %v: %v", got, tc.wantUnconfirmed, err)
			}
		})
	}
}

// oneAudienceFailureBody is a GoogleAdsFailure in the shape gaqlError builds, carrying the
// error code a validateOnly probe on 2026-11-03 returned for a second audience criterion.
// The category key is this fixture's choice, not the probe's: the client classifies the
// failure by HTTP status alone, so it does not change what the tests below exercise.
const oneAudienceFailureBody = `{"error":{"code":3,"status":"INVALID_ARGUMENT","details":[` +
	`{"@type":"type.googleapis.com/google.ads.googleads.v23.errors.GoogleAdsFailure",` +
	`"errors":[{"errorCode":{"criterionError":"ONE_AUDIENCE_ALLOWED_PER_AD_GROUP"},"message":"boom"}]}]}}`

// A definite failure names only what the ad group is missing out of what was ASKED for:
// a languages-only create never had an Audience to lose, and an audience-only one never
// had languages.
func TestCreateDemandGenCampaign_DefiniteFailureNamesOnlyWhatWasAsked(t *testing.T) {
	for name, tc := range map[string]struct {
		audience  string
		languages []string
		want      string
		notWant   string
	}{
		"both": {
			audience: testDemandGenAudience, languages: []string{"EN"},
			want: "it has NO audience or language criteria, so if enabled it would serve without the named Audience and in every language",
		},
		"audience only": {
			audience: testDemandGenAudience,
			want:     "it has NO audience criterion, so if enabled it would serve without the named Audience)",
			notWant:  "language",
		},
		"languages only": {
			languages: []string{"EN"},
			want:      "it has NO language criteria, so if enabled it would serve in every language)",
			notWant:   "Audience",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, c := newDGTargetingRig(t, func(_ int, _ []byte, w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, oneAudienceFailureBody)
			})
			in := demandGenInput()
			in.DemandGenAudience = tc.audience
			in.Languages = tc.languages
			res, err := c.CreateDemandGenCampaign(context.Background(), in)
			if err == nil || res == nil {
				t.Fatalf("want a partial result and an error, got res=%v err=%v", res, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "UNCONFIRMED") {
				t.Errorf("a definite 4xx must not read UNCONFIRMED: %v", err)
			}
			if tc.notWant != "" {
				clause := err.Error()[strings.Index(err.Error(), "created; "):]
				if strings.Contains(clause, tc.notWant) {
					t.Errorf("consequence clause %q mentions %q, which was not asked for", clause, tc.notWant)
				}
			}
		})
	}
}

// A context already done sends NOTHING: the step must not fire a mutate it cannot see
// the outcome of, and its error must say no request was made.
func TestCreateDemandGenAdGroupTargeting_CancelledContextSendsNothing(t *testing.T) {
	rig, c := newDGTargetingRig(t, func(_ int, body []byte, w http.ResponseWriter) {
		t.Errorf("no request expected, got %s", body)
		http.Error(w, "unexpected", http.StatusBadRequest)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	audienceIDs, languageIDs, err := c.createDemandGenAdGroupTargeting(ctx, "customers/1234567890/adGroups/333", "333",
		testDemandGenAudience, []languageInfo{{LanguageConstant: "languageConstants/1000"}})
	if err == nil {
		t.Fatal("a cancelled context must fail the step")
	}
	if !strings.Contains(err.Error(), "aborted before any request") {
		t.Errorf("error = %q, want it to say it aborted before any request", err)
	}
	if audienceIDs != nil || languageIDs != nil {
		t.Errorf("no ids may be returned, got %v %v", audienceIDs, languageIDs)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.paths) != 0 {
		t.Errorf("sent %d requests, want 0: %v", len(rig.paths), rig.paths)
	}
}

// The closing step's no-geo arms ask for GEO targeting by name: an audience or languages
// may already be attached, so "add targeting" would send an operator to redo them.
func TestDemandGenClosingStep_NoGeoArmsAskForGeoTargeting(t *testing.T) {
	for _, hasAd := range []bool{true, false} {
		got := demandGenClosingStep(false, hasAd)
		if !strings.Contains(got, "add geo targeting") {
			t.Errorf("demandGenClosingStep(false, %v) = %q, want it to ask for geo targeting", hasAd, got)
		}
	}
	for _, hasAd := range []bool{true, false} {
		if got := demandGenClosingStep(true, hasAd); strings.Contains(got, "targeting") {
			t.Errorf("demandGenClosingStep(true, %v) = %q, want no targeting advice when geo is set", hasAd, got)
		}
	}
}

// hasExactStep reports whether steps holds want exactly. It is not search_parity_test.go's
// containsStep, which matches a substring: these tests pin the whole step line.
func hasExactStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}
