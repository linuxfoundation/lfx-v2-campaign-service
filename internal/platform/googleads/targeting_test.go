// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- validateKeywords -------------------------------------------------------

func TestValidateKeywords(t *testing.T) {
	t.Run("nil input is a no-op", func(t *testing.T) {
		got, err := validateKeywords(nil)
		if err != nil || got != nil {
			t.Errorf("validateKeywords(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("trims, uppercases match type, and validates", func(t *testing.T) {
		got, err := validateKeywords([]Keyword{{Text: "  kubernetes  ", MatchType: "exact"}})
		if err != nil {
			t.Fatalf("validateKeywords: %v", err)
		}
		if len(got) != 1 || got[0].Text != "kubernetes" || got[0].MatchType != MatchTypeExact {
			t.Errorf("got %+v, want [{kubernetes EXACT}]", got)
		}
	})

	t.Run("empty text is an error", func(t *testing.T) {
		if _, err := validateKeywords([]Keyword{{Text: "  ", MatchType: MatchTypeBroad}}); err == nil {
			t.Error("expected an error for an empty keyword text")
		}
	})

	t.Run("over-limit text is an error", func(t *testing.T) {
		if _, err := validateKeywords([]Keyword{{Text: strings.Repeat("a", maxKeywordTextRunes+1), MatchType: MatchTypeBroad}}); err == nil {
			t.Error("expected an error for keyword text over the rune limit")
		}
	})

	t.Run("unsupported match type is an error", func(t *testing.T) {
		if _, err := validateKeywords([]Keyword{{Text: "kubernetes", MatchType: "FUZZY"}}); err == nil {
			t.Error("expected an error for an unsupported match type")
		}
	})

	t.Run("too many keywords is an error", func(t *testing.T) {
		kws := make([]Keyword, maxKeywords+1)
		for i := range kws {
			kws[i] = Keyword{Text: "kw", MatchType: MatchTypeBroad}
		}
		if _, err := validateKeywords(kws); err == nil {
			t.Error("expected an error for exceeding the keyword count cap")
		}
	})

	t.Run("dedupes by matchType+text", func(t *testing.T) {
		got, err := validateKeywords([]Keyword{
			{Text: "kubernetes", MatchType: MatchTypeBroad},
			{Text: "kubernetes", MatchType: MatchTypeBroad},
			{Text: "kubernetes", MatchType: MatchTypeExact},
		})
		if err != nil {
			t.Fatalf("validateKeywords: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %+v, want 2 entries (same text, different match types kept; exact duplicate dropped)", got)
		}
	})
}

// ---- audienceCriterionField / validateAudienceSegments ----------------------

func TestAudienceCriterionField(t *testing.T) {
	cases := []struct {
		name         string
		resourceName string
		wantField    string
		wantOK       bool
	}{
		{"user list", "customers/123/userLists/456", "userList", true},
		{"custom audience", "customers/123/customAudiences/456", "customAudience", true},
		{"unrecognized", "customers/123/userInterests/456", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field, ok := audienceCriterionField(tc.resourceName)
			if field != tc.wantField || ok != tc.wantOK {
				t.Errorf("audienceCriterionField(%q) = (%q, %v), want (%q, %v)", tc.resourceName, field, ok, tc.wantField, tc.wantOK)
			}
		})
	}
}

func TestValidateAudienceSegments(t *testing.T) {
	t.Run("nil input is a no-op", func(t *testing.T) {
		got, err := validateAudienceSegments(nil)
		if err != nil || got != nil {
			t.Errorf("validateAudienceSegments(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("accepts user lists and custom audiences", func(t *testing.T) {
		in := []string{"customers/1/userLists/2", "customers/1/customAudiences/3"}
		got, err := validateAudienceSegments(in)
		if err != nil {
			t.Fatalf("validateAudienceSegments: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %v, want both entries kept", got)
		}
	})

	t.Run("empty entry is an error", func(t *testing.T) {
		if _, err := validateAudienceSegments([]string{"  "}); err == nil {
			t.Error("expected an error for an empty audience segment")
		}
	})

	t.Run("unrecognized resource name is an error", func(t *testing.T) {
		if _, err := validateAudienceSegments([]string{"customers/1/userInterests/2"}); err == nil {
			t.Error("expected an error for an unrecognized audience resource name")
		}
	})

	t.Run("too many segments is an error", func(t *testing.T) {
		segs := make([]string, maxAudienceSegments+1)
		for i := range segs {
			segs[i] = "customers/1/userLists/2"
		}
		if _, err := validateAudienceSegments(segs); err == nil {
			t.Error("expected an error for exceeding the audience segment count cap")
		}
	})

	t.Run("dedupes exact duplicates", func(t *testing.T) {
		got, err := validateAudienceSegments([]string{"customers/1/userLists/2", "customers/1/userLists/2"})
		if err != nil {
			t.Fatalf("validateAudienceSegments: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("got %v, want the duplicate dropped", got)
		}
	})
}

// ---- createAdGroupTargeting integration paths -------------------------------

// newTargetingClient wires a token server + an API server whose
// campaignBudgets/campaigns/adGroups/adGroupAds mutates all succeed, and whose
// adGroupCriteria:mutate handler is supplied per-test.
func newTargetingClient(t *testing.T, criteriaH http.HandlerFunc) *Client {
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
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			criteriaH(w, r)
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

func okAdGroupCriteria(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[`+
		`{"resourceName":"customers/1234567890/adGroupCriteria/333~1"},`+
		`{"resourceName":"customers/1234567890/adGroupCriteria/333~2"}]}`)
}

func TestCreateAdGroupAndAd_TargetingHappyPath(t *testing.T) {
	var mu sync.Mutex
	var criteriaBody map[string]any
	c := newTargetingClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := decode(t, r)
		mu.Lock()
		criteriaBody = body
		mu.Unlock()
		okAdGroupCriteria(w, r)
	})
	in := sampleInput()
	in.Keywords = []Keyword{{Text: "kubernetes", MatchType: MatchTypeBroad}}
	in.AudienceSegments = []string{"customers/1/userLists/2"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(res.KeywordCriteriaIDs) != 1 || res.KeywordCriteriaIDs[0] != "1" {
		t.Errorf("KeywordCriteriaIDs = %v, want [1]", res.KeywordCriteriaIDs)
	}
	if len(res.AudienceCriteriaIDs) != 1 || res.AudienceCriteriaIDs[0] != "2" {
		t.Errorf("AudienceCriteriaIDs = %v, want [2]", res.AudienceCriteriaIDs)
	}

	mu.Lock()
	body := criteriaBody
	mu.Unlock()
	ops, ok := body["operations"].([]any)
	if !ok || len(ops) != 2 {
		t.Fatalf("adGroupCriteria:mutate operations = %v, want 2", body["operations"])
	}
	kwOp := ops[0].(map[string]any)["create"].(map[string]any)
	if kwOp["status"] != StatusEnabled {
		t.Errorf("keyword criterion status = %v, want %s (unlike the PAUSED ad group/ad shell)", kwOp["status"], StatusEnabled)
	}
	kw, ok := kwOp["keyword"].(map[string]any)
	if !ok || kw["text"] != "kubernetes" || kw["matchType"] != MatchTypeBroad {
		t.Errorf("keyword payload = %v, want {text: kubernetes, matchType: BROAD}", kwOp["keyword"])
	}
	audOp := ops[1].(map[string]any)["create"].(map[string]any)
	ul, ok := audOp["userList"].(map[string]any)
	if !ok || ul["userList"] != "customers/1/userLists/2" {
		t.Errorf("userList payload = %v, want {userList: customers/1/userLists/2}", audOp["userList"])
	}
}

func TestCreateAdGroupAndAd_NoTargetingSkipsCriteriaCall(t *testing.T) {
	c := newTargetingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("adGroupCriteria:mutate must not be called when no keywords/audience segments are supplied")
		w.WriteHeader(http.StatusInternalServerError)
	})
	res, err := c.CreateCampaign(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(res.KeywordCriteriaIDs) != 0 || len(res.AudienceCriteriaIDs) != 0 {
		t.Errorf("got non-empty criteria ids %v/%v with no targeting input", res.KeywordCriteriaIDs, res.AudienceCriteriaIDs)
	}
}

func TestCreateAdGroupAndAd_TargetingAmbiguous5xx(t *testing.T) {
	c := newTargetingClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	in := sampleInput()
	in.Keywords = []Keyword{{Text: "kubernetes", MatchType: MatchTypeBroad}}
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error on a 5xx adGroupCriteria mutate")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %v, want it classified as UNCONFIRMED", err)
	}
}

func TestCreateAdGroupAndAd_TargetingDefiniteFailure(t *testing.T) {
	c := newTargetingClient(t, gaqlError(http.StatusBadRequest, "criterionError", "SOME_ERROR"))
	in := sampleInput()
	in.Keywords = []Keyword{{Text: "kubernetes", MatchType: MatchTypeBroad}}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error on a definite 4xx adGroupCriteria mutate")
	}
	if res == nil || res.AdID == "" {
		t.Error("the campaign/ad group/ad must still be reported as created even though targeting failed")
	}
	if strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %v, a definite 4xx must not be reported UNCONFIRMED", err)
	}
}

func TestCreateAdGroupAndAd_TargetingMalformedResponse(t *testing.T) {
	c := newTargetingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333"}]}`)
	})
	in := sampleInput()
	in.Keywords = []Keyword{{Text: "kubernetes", MatchType: MatchTypeBroad}}
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error on a malformed adGroupCriteria resource name")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error = %v, want it classified as UNCONFIRMED", err)
	}
}

func TestCreateAdGroupAndAd_InvalidKeywordAbortsBeforeAnyMutate(t *testing.T) {
	c := newTargetingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("adGroupCriteria:mutate must not be called when keyword validation fails")
	})
	in := sampleInput()
	in.Keywords = []Keyword{{Text: "kubernetes", MatchType: "BOGUS"}}
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for an invalid match type")
	}
	if !strings.Contains(err.Error(), "invalid keyword input") {
		t.Errorf("error = %v, want it to mention invalid keyword input", err)
	}
}

func TestCreateAdGroupAndAd_InvalidAudienceSegmentAbortsBeforeAnyMutate(t *testing.T) {
	c := newTargetingClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("adGroupCriteria:mutate must not be called when audience segment validation fails")
	})
	in := sampleInput()
	in.AudienceSegments = []string{"customers/1/userInterests/2"}
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for an unrecognized audience resource name")
	}
	if !strings.Contains(err.Error(), "invalid audience segment input") {
		t.Errorf("error = %v, want it to mention invalid audience segment input", err)
	}
}

// ---- campaign-level targetingSetting -----------------------------------------

// newTargetingClientCapturingCampaign is newTargetingClient but also captures the
// decoded campaigns:mutate request body, for tests asserting on campaign-level
// fields (targetingSetting) rather than the adGroupCriteria call itself.
// capturedBody guards a decoded request body captured inside an httptest
// handler goroutine and read back from the test goroutine — the handler runs
// on its own goroutine, so an unguarded map read/write here would race.
type capturedBody struct {
	mu   sync.Mutex
	body map[string]any
}

func (c *capturedBody) set(b map[string]any) {
	c.mu.Lock()
	c.body = b
	c.mu.Unlock()
}

func (c *capturedBody) get() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

func newTargetingClientCapturingCampaign(t *testing.T, criteriaH http.HandlerFunc) (*Client, *capturedBody) {
	t.Helper()
	captured := &capturedBody{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			captured.set(decode(t, r))
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			okAdGroupAd(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			criteriaH(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
	return c, captured
}

func okAdGroupCriteriaOne(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~1"}]}`)
}

func TestCreateCampaign_SetsAudienceObservationTargetingSetting(t *testing.T) {
	c, campaignBody := newTargetingClientCapturingCampaign(t, okAdGroupCriteriaOne)
	in := sampleInput()
	in.AudienceSegments = []string{"customers/1/userLists/2"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	op := firstCreate(t, campaignBody.get())
	ts, ok := op["targetingSetting"].(map[string]any)
	if !ok {
		t.Fatalf("campaign create body = %v, want a targetingSetting when audience segments are supplied", op)
	}
	restrictions, ok := ts["targetRestrictions"].([]any)
	if !ok || len(restrictions) != 1 {
		t.Fatalf("targetRestrictions = %v, want exactly one entry", ts["targetRestrictions"])
	}
	r := restrictions[0].(map[string]any)
	if r["targetingDimension"] != "AUDIENCE" || r["bidOnly"] != true {
		t.Errorf("targetRestriction = %v, want {targetingDimension: AUDIENCE, bidOnly: true}", r)
	}
}

func TestCreateCampaign_NoAudienceSegmentsOmitsTargetingSetting(t *testing.T) {
	c, campaignBody := newTargetingClientCapturingCampaign(t, okAdGroupCriteria)
	if _, err := c.CreateCampaign(context.Background(), sampleInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	op := firstCreate(t, campaignBody.get())
	if _, present := op["targetingSetting"]; present {
		t.Errorf("campaign create body = %v, want no targetingSetting when no audience segments are supplied", op)
	}
}
