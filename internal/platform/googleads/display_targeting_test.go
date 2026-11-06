// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// ---------------------------------------------------------------------------
// Display audiences and placement exclusions (#462)
// ---------------------------------------------------------------------------

// The four segment lists and a blocklist, all on the test account (1234567890).
func displayTargetingInput() CampaignInput {
	in := demandGenInput()
	in.AudienceSegments = []string{"customers/1234567890/userLists/11"}
	in.CustomAudiences = []string{"customers/1234567890/customAudiences/22"}
	in.CustomAffinities = []string{"customers/1234567890/customInterests/33"}
	in.UserInterests = []string{"customers/1234567890/userInterests/44"}
	in.ExcludedPlacements = []string{"example.com", "News.Example.org"}
	return in
}

// pathRecorder records the order the cascade's mutates arrive in.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		p.mu.Unlock()
		h(w, r)
	}
}

func (p *pathRecorder) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

// displayTargetingServer serves the Display shell with no creative, routing the two
// criteria mutates to the handlers under test. The creative mutates fail the test:
// no creative is supplied, so the cascade must not reach them.
func displayTargetingServer(t *testing.T, rec *pathRecorder, adGroupH, audienceH, placementH http.HandlerFunc) *Client {
	t.Helper()
	inner := displayCascade(t, nil, adGroupH, failHandler(t, "assets:mutate"), failHandler(t, "adGroupAds:mutate"))
	srv := demandGenTLSServer(t, rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			audienceH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			placementH(w, r)
		default:
			inner(w, r)
		}
	}))
	return demandGenClient(t, srv)
}

// sameJSON compares two JSON documents structurally, so the assertion is on the payload
// rather than on key order or whitespace.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("decoding sent body %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("decoding wanted body: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("payload mismatch\n got: %s\nwant: %s", got, want)
	}
}

func badRequest(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
}

// The whole feature on the wire: one adGroupCriteria:mutate carrying the four segment
// kinds in order, each in the oneof arm Google names, then one campaignCriteria:mutate of
// negative placements — in that order, after the ad group, and with the ad group itself
// carrying targetingSetting AUDIENCE bidOnly false (after the four demographic defaults,
// restated unchanged), which declares them targeting.
func TestCreateDisplayCampaign_SendsAudiencesAndPlacementExclusions(t *testing.T) {
	rec := &pathRecorder{}
	adGroupH, readAdGroup := capturedMutate(1, displayAdGroupResource)
	audienceH, readAudiences := capturedMutate(4, adGroupCriterionName)
	placementH, readPlacements := capturedMutate(2, campaignCriterionName)
	c := displayTargetingServer(t, rec, adGroupH, audienceH, placementH)

	res, err := c.CreateDisplayCampaign(context.Background(), displayTargetingInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sameJSON(t, readAudiences(), `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/333","status":"ENABLED","userList":{"userList":"customers/1234567890/userLists/11"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","status":"ENABLED","customAudience":{"customAudience":"customers/1234567890/customAudiences/22"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","status":"ENABLED","customAffinity":{"customAffinity":"customers/1234567890/customInterests/33"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","status":"ENABLED","userInterest":{"userInterestCategory":"customers/1234567890/userInterests/44"}}}
	]}`)
	// Lower-cased, never otherwise rewritten, and explicitly negative.
	sameJSON(t, readPlacements(), `{"operations":[
		{"create":{"campaign":"customers/1234567890/campaigns/222","negative":true,"placement":{"url":"example.com"}}},
		{"create":{"campaign":"customers/1234567890/campaigns/222","negative":true,"placement":{"url":"news.example.org"}}}
	]}`)

	adGroup, _ := json.Marshal(mutateCreate(t, readAdGroup())["targetingSetting"])
	wantSetting := `{"targetRestrictions":[` +
		`{"bidOnly":true,"targetingDimension":"AGE_RANGE"},` +
		`{"bidOnly":true,"targetingDimension":"GENDER"},` +
		`{"bidOnly":true,"targetingDimension":"PARENTAL_STATUS"},` +
		`{"bidOnly":true,"targetingDimension":"INCOME_RANGE"},` +
		`{"bidOnly":false,"targetingDimension":"AUDIENCE"}]}`
	if string(adGroup) != wantSetting {
		t.Errorf("ad group targetingSetting = %s, want %s — audiences must TARGET, demographics keep Google's default", adGroup, wantSetting)
	}
	if _, ok := mutateCreate(t, readAdGroup())["optimizedTargetingEnabled"]; ok {
		t.Error("optimizedTargetingEnabled must not be sent; it is left at Google's default")
	}

	want := []string{"campaignBudgets:mutate", "campaigns:mutate", "adGroups:mutate", "adGroupCriteria:mutate", "campaignCriteria:mutate"}
	if got := rec.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("mutate order = %v, want %v", got, want)
	}

	if want := []string{"900", "901", "902", "903"}; !reflect.DeepEqual(res.AudienceCriteriaIDs, want) {
		t.Errorf("AudienceCriteriaIDs = %v, want %v", res.AudienceCriteriaIDs, want)
	}
	if want := []string{"900", "901"}; !reflect.DeepEqual(res.ExcludedPlacementCriteriaIDs, want) {
		t.Errorf("ExcludedPlacementCriteriaIDs = %v, want %v", res.ExcludedPlacementCriteriaIDs, want)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, s := range []string{
		"Audience targeting applied: 4 ad group criteria (1 user list(s); 1 custom audience(s); 1 custom affinity segment(s); 1 user interest(s)) — attached as targeting (AUDIENCE bidOnly false), not observation; optimized targeting is left at Google's default and may widen reach",
		"Placement exclusions applied: 2 campaign negative placement criteria",
	} {
		if !strings.Contains(steps, s) {
			t.Errorf("steps lack %q:\n%s", s, steps)
		}
	}
}

// Each half is independent: a campaign with only placement exclusions sends no
// adGroupCriteria mutate, and one with only audiences sends no campaignCriteria mutate.
func TestCreateDisplayCampaign_EachHalfAlone(t *testing.T) {
	t.Run("placements only", func(t *testing.T) {
		placementH, _ := capturedMutate(1, campaignCriterionName)
		adGroupH, readAdGroup := capturedMutate(1, displayAdGroupResource)
		c := displayTargetingServer(t, &pathRecorder{}, adGroupH, failHandler(t, "adGroupCriteria:mutate"), placementH)
		in := demandGenInput()
		in.ExcludedPlacements = []string{"example.com"}
		res, err := c.CreateDisplayCampaign(context.Background(), in)
		if err != nil || len(res.ExcludedPlacementCriteriaIDs) != 1 || len(res.AudienceCriteriaIDs) != 0 {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		// No audience input: the ad group payload is byte-identical to the pre-#462 one.
		want := `{"operations":[{"create":{"name":"` + displayAdGroupName(in) +
			`","campaign":"customers/1234567890/campaigns/222","status":"ENABLED","type":"DISPLAY_STANDARD"}}]}`
		if got := readAdGroup(); got != want {
			t.Errorf("ad group body = %s, want exactly %s", got, want)
		}
	})
	t.Run("one user list only", func(t *testing.T) {
		audienceH, read := capturedMutate(1, adGroupCriterionName)
		c := displayTargetingServer(t, &pathRecorder{}, okAdGroup, audienceH, failHandler(t, "campaignCriteria:mutate"))
		in := demandGenInput()
		in.AudienceSegments = []string{"customers/1234567890/userLists/11"}
		res, err := c.CreateDisplayCampaign(context.Background(), in)
		if err != nil || len(res.AudienceCriteriaIDs) != 1 {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if !strings.Contains(read(), `"userList":{"userList":"customers/1234567890/userLists/11"}`) {
			t.Errorf("user list not sent: %s", read())
		}
	})
}

// A failure of either criteria step happens after the campaign exists, so it returns the
// partial result with the error: definite refusals are failures, ambiguous ones
// UNCONFIRMED, and an audience failure never attempts the placement step.
func TestCreateDisplayCampaign_TargetingFailuresKeepThePartial(t *testing.T) {
	wrongAdGroup := func(w http.ResponseWriter, r *http.Request) {
		h, _ := capturedMutate(4, func(int) string { return "customers/1234567890/adGroupCriteria/999~900" })
		h(w, r)
	}
	shortAudience, _ := capturedMutate(1, adGroupCriterionName)
	foreignAudience := func(w http.ResponseWriter, r *http.Request) {
		h, _ := capturedMutate(4, func(int) string { return "customers/9999999999/adGroupCriteria/333~900" })
		h(w, r)
	}
	foreignPlacement := func(w http.ResponseWriter, r *http.Request) {
		h, _ := capturedMutate(2, func(int) string { return "customers/9999999999/campaignCriteria/222~900" })
		h(w, r)
	}
	okAudience, _ := capturedMutate(4, adGroupCriterionName)
	shortPlacement, _ := capturedMutate(1, campaignCriterionName)
	otherCampaign := func(w http.ResponseWriter, r *http.Request) {
		h, _ := capturedMutate(2, func(int) string { return "customers/1234567890/campaignCriteria/999~900" })
		h(w, r)
	}

	cases := []struct {
		name                   string
		audienceH, placementH  http.HandlerFunc
		wantSub                string
		wantUnconfirmed        bool
		wantAudienceIDs        int
		placementNeverAttempts bool
	}{
		{"audience 4xx", badRequest, nil, "display audience targeting failed", false, 0, true},
		{"audience 5xx", serverError, nil, "display audience targeting UNCONFIRMED", true, 0, true},
		{"audience short 2xx", shortAudience, nil, "display audience targeting UNCONFIRMED", true, 0, true},
		{"audience other ad group", wrongAdGroup, nil, "reports a different ad group id", true, 0, true},
		{"audience foreign account", foreignAudience, nil, "malformed/wrong-kind/wrong-account criterion resource name", true, 0, true},
		{"placement 4xx", okAudience, badRequest, "placement exclusions failed", false, 4, false},
		{"placement 5xx", okAudience, serverError, "placement exclusions UNCONFIRMED", true, 4, false},
		{"placement foreign account", okAudience, foreignPlacement, "placement exclusions UNCONFIRMED", true, 4, false},
		{"placement short 2xx", okAudience, shortPlacement, "malformed/short mutate response", true, 4, false},
		{"placement other campaign", okAudience, otherCampaign, "reports a different campaign id", true, 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			placementH := tc.placementH
			if tc.placementNeverAttempts {
				placementH = failHandler(t, "campaignCriteria:mutate")
			}
			c := displayTargetingServer(t, &pathRecorder{}, okAdGroup, tc.audienceH, placementH)
			res, err := c.CreateDisplayCampaign(context.Background(), displayTargetingInput())
			if err == nil {
				t.Fatal("want an error")
			}
			if res == nil {
				t.Fatal("the campaign and ad group exist — the partial must not be dropped")
			}
			if res.CampaignID != "222" || res.AdGroupID != "333" {
				t.Errorf("partial = campaign %q / ad group %q, want 222/333", res.CampaignID, res.AdGroupID)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
			// The text is the classification on these arms, as on every sibling criteria
			// step: UNCONFIRMED exactly when the criteria may exist upstream.
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.wantUnconfirmed {
				t.Errorf("UNCONFIRMED in error = %v, want %v: %v", got, tc.wantUnconfirmed, err)
			}
			if outcome, _ := CreateOutcome(res, err); outcome != domain.CreateOutcomeCampaignCreatedSetupIncomplete {
				t.Errorf("CreateOutcome = %q, want %q — the campaign exists", outcome, domain.CreateOutcomeCampaignCreatedSetupIncomplete)
			}
			if len(res.AudienceCriteriaIDs) != tc.wantAudienceIDs {
				t.Errorf("AudienceCriteriaIDs = %v, want %d", res.AudienceCriteriaIDs, tc.wantAudienceIDs)
			}
			if len(res.ExcludedPlacementCriteriaIDs) != 0 {
				t.Errorf("ExcludedPlacementCriteriaIDs = %v, want none", res.ExcludedPlacementCriteriaIDs)
			}
		})
	}
}

// Every refusal is decided before the budget mutate: the server fails the test on any
// request at all.
func TestCreateDisplayCampaign_RefusesBadTargetingBeforeAnyRequest(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*CampaignInput)
		wantSub string
	}{
		"foreign user list": {
			func(in *CampaignInput) { in.AudienceSegments = []string{"customers/9999999999/userLists/11"} },
			"is on customer \"9999999999\", not this campaign's account \"1234567890\"",
		},
		"foreign custom audience": {
			func(in *CampaignInput) { in.CustomAudiences = []string{"customers/9999999999/customAudiences/22"} },
			"is on customer \"9999999999\", not this campaign's account \"1234567890\"",
		},
		"foreign custom affinity": {
			func(in *CampaignInput) { in.CustomAffinities = []string{"customers/9999999999/customInterests/33"} },
			"is on customer \"9999999999\", not this campaign's account \"1234567890\"",
		},
		"foreign user interest": {
			func(in *CampaignInput) { in.UserInterests = []string{"customers/9999999999/userInterests/44"} },
			"is on customer \"9999999999\", not this campaign's account \"1234567890\"",
		},
		"an Audience resource": {
			func(in *CampaignInput) { in.AudienceSegments = []string{"customers/1234567890/audiences/55"} },
			"is an Audience resource, which a Display ad group refuses",
		},
		"a user interest in CustomAudiences": {
			func(in *CampaignInput) { in.CustomAudiences = []string{"customers/1234567890/userInterests/44"} },
			"on Display it belongs in UserInterests",
		},
		"a custom audience in AudienceSegments": {
			func(in *CampaignInput) { in.AudienceSegments = []string{"customers/1234567890/customAudiences/22"} },
			"on Display it belongs in CustomAudiences",
		},
		"malformed name": {
			func(in *CampaignInput) { in.CustomAffinities = []string{"customInterests/33"} },
			"is not a customers/{customerId}/customInterests/{id} resource name",
		},
		"placement with a scheme": {
			func(in *CampaignInput) { in.ExcludedPlacements = []string{"https://example.com"} },
			"carries a scheme",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("request %s sent for an input the preflight must refuse", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			})
			c := demandGenClient(t, srv)
			in := demandGenInput()
			tc.mutate(&in)
			res, err := c.CreateDisplayCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("want a preflight refusal")
			}
			if res != nil {
				t.Errorf("res = %+v, want nil — nothing was created", res)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// Off Display the three new segment lists and the blocklist are refused by name, with
// the channel named and Display offered — never validated and then dropped. Search's own
// AudienceSegments stays accepted; the other channels still refuse it.
func TestPreflightRefusesDisplayTargetingOffDisplay(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	cases := map[string]struct {
		mutate  func(*CampaignInput)
		wantSub string
	}{
		"CustomAudiences":    {func(in *CampaignInput) { in.CustomAudiences = []string{"customers/1234567890/customAudiences/22"} }, "CustomAudiences are not supported on"},
		"CustomAffinities":   {func(in *CampaignInput) { in.CustomAffinities = []string{"customers/1234567890/customInterests/33"} }, "CustomAffinities are not supported on"},
		"UserInterests":      {func(in *CampaignInput) { in.UserInterests = []string{"customers/1234567890/userInterests/44"} }, "UserInterests are not supported on"},
		"ExcludedPlacements": {func(in *CampaignInput) { in.ExcludedPlacements = []string{"example.com"} }, "placement exclusions are not supported on"},
	}
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen, campaignKindPerformanceMax, campaignKindVideo} {
		for name, tc := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				in := demandGenInput()
				if kind == campaignKindPerformanceMax {
					in.PerformanceMaxCreative = fullPMaxCreative()
				}
				if _, err := c.preflightCampaignKind(kind, in); err != nil {
					t.Fatalf("clean input must pass first: %v", err)
				}
				tc.mutate(&in)
				_, err := c.preflightCampaignKind(kind, in)
				if err == nil {
					t.Fatalf("preflight(%s) accepted %s; it must refuse rather than drop", kind, name)
				}
				for _, sub := range []string{tc.wantSub, kind, "create a Display campaign"} {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("err = %q, want it to contain %q", err, sub)
					}
				}
			})
		}
	}
	// AudienceSegments on Search keeps its pre-#462 contract: accepted, shape-checked only.
	in := sampleInput()
	in.AudienceSegments = []string{"customers/1/userLists/2"}
	if _, err := c.preflightCampaignKind(campaignKindSearch, in); err != nil {
		t.Errorf("Search refused a user list it has always accepted: %v", err)
	}
}

func TestValidateDisplayAudienceList(t *testing.T) {
	const acct = "1234567890"
	t.Run("empty is a no-op", func(t *testing.T) {
		if got, err := validateDisplayAudienceList("UserInterests", "userInterests", acct, nil); got != nil || err != nil {
			t.Errorf("= (%v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("trims and dedupes", func(t *testing.T) {
		got, err := validateDisplayAudienceList("UserInterests", "userInterests", acct,
			[]string{" customers/1234567890/userInterests/44 ", "customers/1234567890/userInterests/44", "customers/1234567890/userInterests/45"})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"customers/1234567890/userInterests/44", "customers/1234567890/userInterests/45"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	for name, entry := range map[string]string{
		"blank":              "  ",
		"three segments":     "customers/1234567890/userInterests",
		"five segments":      "customers/1234567890/userInterests/44/x",
		"non-numeric id":     "customers/1234567890/userInterests/abc",
		"non-numeric owner":  "customers/acct/userInterests/44",
		"not customers":      "accounts/1234567890/userInterests/44",
		"unknown collection": "customers/1234567890/combinedAudiences/44",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := validateDisplayAudienceList("UserInterests", "userInterests", acct, []string{entry}); err == nil {
				t.Errorf("%q accepted", entry)
			}
		})
	}
	t.Run("caps the list", func(t *testing.T) {
		names := make([]string, maxAudienceSegments+1)
		for i := range names {
			names[i] = "customers/1234567890/userInterests/44"
		}
		if _, err := validateDisplayAudienceList("UserInterests", "userInterests", acct, names); err == nil {
			t.Error("an over-cap list was accepted")
		}
	})
}

func TestValidateExcludedPlacements(t *testing.T) {
	t.Run("lower-cases and dedupes, sending www and bare domains as written", func(t *testing.T) {
		got, err := validateExcludedPlacements([]string{" Example.com ", "example.com", "www.example.com", "xn--bcher-kva.example", "1password.com"})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"example.com", "www.example.com", "xn--bcher-kva.example", "1password.com"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	long := strings.Repeat("a", 63) + "."
	// The scheme and the path/query/port/credentials each get their own message, so a
	// caller knows what to drop rather than reading that the entry "is not a domain".
	for entry, want := range map[string]string{
		"https://example.com": "carries a scheme",
		"example.com/sports":  "with no path, query, fragment, port or credentials",
		"example.com?x=1":     "with no path, query, fragment, port or credentials",
		"example.com#top":     "with no path, query, fragment, port or credentials",
		"example.com:8080":    "with no path, query, fragment, port or credentials",
		"user@example.com":    "with no path, query, fragment, port or credentials",
		// strings.ToLower folds these to ASCII ("examplek.com", "i.com"), so ASCII is
		// checked before case is folded: the entry is refused, never rewritten.
		"exampleK.com":   "is not ASCII",
		"İ.com":          "is not ASCII",
		"bücher.example": "is not ASCII",
	} {
		if _, err := validateExcludedPlacements([]string{entry}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateExcludedPlacements(%q) = %v, want an error containing %q", entry, err, want)
		}
	}
	// A refused entry may carry a secret in its query or credentials, and the error text
	// reaches logs: neither may be echoed back.
	for entry, secret := range map[string]string{
		"example.com?x=1":               "x=1",
		"user:pass@example.com":         "pass",
		"https://example.com/p?token=s": "token=s",
		"https://u:pass@example.com":    "pass",
	} {
		_, err := validateExcludedPlacements([]string{entry})
		if err == nil {
			t.Fatalf("%q accepted", entry)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error for %q echoes %q: %v", entry, secret, err)
		}
	}
	for name, entry := range map[string]string{
		"IPv4 literal":   "192.168.0.1",
		"blank":          " ",
		"scheme":         "https://example.com",
		"path":           "example.com/sports",
		"query":          "example.com?x=1",
		"fragment":       "example.com#top",
		"port":           "example.com:8080",
		"credentials":    "user@example.com",
		"wildcard":       "*.example.com",
		"single label":   "localhost",
		"trailing dot":   "example.com.",
		"empty label":    "example..com",
		"leading hyphen": "-example.com",
		"trailing hyph":  "example-.com",
		"underscore":     "ex_ample.com",
		"non-ASCII":      "bücher.example",
		"long label":     strings.Repeat("a", 64) + ".com",
		"too long":       strings.Repeat(long, 4) + "com",
		"inner space":    "exa mple.com",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := validateExcludedPlacements([]string{entry}); err == nil {
				t.Errorf("%q accepted", entry)
			}
		})
	}
	t.Run("caps the list", func(t *testing.T) {
		list := make([]string, maxExcludedPlacements+1)
		for i := range list {
			list[i] = "example.com"
		}
		if _, err := validateExcludedPlacements(list); err == nil {
			t.Error("an over-cap list was accepted")
		}
	})
}

// ---------------------------------------------------------------------------
// #462 beside #473: one Display ad group carrying both the income and parental-status
// exclusions and the audience lists
// ---------------------------------------------------------------------------

// criteriaBodies serves every adGroupCriteria:mutate with one result per operation (via
// criteriaEcho) and records each request body in order. failOn, when non-zero, answers
// that call (1-based) with a definite 400 instead.
func criteriaBodies(failOn int) (http.HandlerFunc, func() []string) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	h := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == failOn {
			badRequest(w, r)
			return
		}
		_, _ = io.WriteString(w, criteriaEcho(b))
	}
	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// The two ad-group criteria steps stay separate mutates on the one ad group: the
// exclusions first (#473's step, right after the ad group), then the audiences, then
// the placements. Each records its own ids, and the AUDIENCE targetingSetting the
// audiences need sits beside the exclusions, which apply whatever bidOnly says.
func TestCreateDisplayCampaign_ExclusionsAndAudiencesOnOneAdGroup(t *testing.T) {
	rec := &pathRecorder{}
	adGroupH, readAdGroup := capturedMutate(1, displayAdGroupResource)
	criteriaH, bodies := criteriaBodies(0)
	placementH, _ := capturedMutate(2, campaignCriterionName)
	c := displayTargetingServer(t, rec, adGroupH, criteriaH, placementH)

	in := displayTargetingInput()
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	in.ExcludedParentalStatuses = []string{"PARENT"}
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"campaignBudgets:mutate", "campaigns:mutate", "adGroups:mutate", "adGroupCriteria:mutate", "adGroupCriteria:mutate", "campaignCriteria:mutate"}
	if got := rec.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mutate order = %v, want %v", got, want)
	}
	got := bodies()
	sameJSON(t, got[0], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/333","negative":true,"incomeRange":{"type":"INCOME_RANGE_0_50"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","negative":true,"parentalStatus":{"type":"PARENT"}}}
	]}`)
	if strings.Contains(got[1], "incomeRange") || strings.Contains(got[1], "parentalStatus") || !strings.Contains(got[1], `"userList"`) {
		t.Errorf("second adGroupCriteria body = %s, want the audiences alone", got[1])
	}
	if _, ok := mutateCreate(t, readAdGroup())["targetingSetting"]; !ok {
		t.Error("ad group sent no targetingSetting although audiences were asked for")
	}
	if w := []string{"900", "901"}; !reflect.DeepEqual(res.AdGroupDemographicCriteriaIDs, w) {
		t.Errorf("AdGroupDemographicCriteriaIDs = %v, want %v", res.AdGroupDemographicCriteriaIDs, w)
	}
	if w := []string{"900", "901", "902", "903"}; !reflect.DeepEqual(res.AudienceCriteriaIDs, w) {
		t.Errorf("AudienceCriteriaIDs = %v, want %v", res.AudienceCriteriaIDs, w)
	}
	if len(res.ExcludedPlacementCriteriaIDs) != 2 {
		t.Errorf("ExcludedPlacementCriteriaIDs = %v, want 2", res.ExcludedPlacementCriteriaIDs)
	}
}

// A refused exclusion stops the cascade before the audiences and the placements, as it
// stops it before the creative: nothing more is built on the group.
func TestCreateDisplayCampaign_ExclusionFailureSendsNoAudiencesOrPlacements(t *testing.T) {
	rec := &pathRecorder{}
	criteriaH, bodies := criteriaBodies(1)
	c := displayTargetingServer(t, rec, okAdGroup, criteriaH, failHandler(t, "campaignCriteria:mutate"))

	in := displayTargetingInput()
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected the exclusion failure to surface")
	}
	if n := len(bodies()); n != 1 {
		t.Errorf("sent %d adGroupCriteria mutates, want only the failed exclusion one", n)
	}
	if res == nil || res.AdGroupID == "" || len(res.AudienceCriteriaIDs) != 0 || len(res.AdGroupDemographicCriteriaIDs) != 0 {
		t.Errorf("res = %+v, want the ad group kept and no criterion ids", res)
	}
}
