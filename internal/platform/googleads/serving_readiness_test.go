// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- validateNegativeKeywords ----------------------------------------------

func TestValidateNegativeKeywords(t *testing.T) {
	t.Run("nil input is a no-op", func(t *testing.T) {
		got, err := validateNegativeKeywords(nil)
		if err != nil || got != nil {
			t.Errorf("validateNegativeKeywords(nil) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("applies the same trim/uppercase/dedupe rules as positives", func(t *testing.T) {
		got, err := validateNegativeKeywords([]Keyword{
			{Text: "  free  ", MatchType: "broad"},
			{Text: "FREE", MatchType: MatchTypeBroad},
		})
		if err != nil {
			t.Fatalf("validateNegativeKeywords: %v", err)
		}
		if len(got) != 1 || got[0].Text != "free" || got[0].MatchType != MatchTypeBroad {
			t.Errorf("got %+v, want one {free BROAD} after case-insensitive dedupe", got)
		}
	})

	// The whole reason validateNegativeKeywords is not a direct call to
	// validateKeywords: an operator reading the rejection must be told WHICH list
	// was refused, or they go looking at the wrong half of the config.
	t.Run("errors name the negative list, not the positive one", func(t *testing.T) {
		cases := map[string][]Keyword{
			"empty text":         {{Text: "  ", MatchType: MatchTypeBroad}},
			"over-limit text":    {{Text: strings.Repeat("a", maxKeywordTextRunes+1), MatchType: MatchTypeBroad}},
			"bad match type":     {{Text: "free", MatchType: "FUZZY"}},
			"too many negatives": make([]Keyword, maxNegativeKeywords+1),
		}
		for name, kws := range cases {
			_, err := validateNegativeKeywords(kws)
			if err == nil {
				t.Errorf("%s: expected an error", name)
				continue
			}
			if !strings.Contains(err.Error(), "negative keyword") {
				t.Errorf("%s: error %q does not say which list was refused", name, err)
			}
		}
	})

	// Byte-for-byte preservation of the positive path's messages is the contract
	// that makes the shared implementation safe to introduce.
	t.Run("positive-keyword errors still say plain keyword", func(t *testing.T) {
		_, err := validateKeywords([]Keyword{{Text: "  ", MatchType: MatchTypeBroad}})
		if err == nil || err.Error() != "google-ads: keyword text must not be empty" {
			t.Errorf("positive error = %v, want the original unchanged wording", err)
		}
	})

	// A term legal as both a positive and a negative must not be refused: that is
	// how a broad positive is narrowed, and the two criteria live at different
	// levels upstream.
	t.Run("dedupe is per list, not across lists", func(t *testing.T) {
		if _, err := validateKeywords([]Keyword{{Text: "kubernetes", MatchType: MatchTypeBroad}}); err != nil {
			t.Fatalf("positive: %v", err)
		}
		if _, err := validateNegativeKeywords([]Keyword{{Text: "kubernetes", MatchType: MatchTypeExact}}); err != nil {
			t.Fatalf("negative with the same text must be accepted, got %v", err)
		}
	})

	t.Run("exactly at the cap is accepted", func(t *testing.T) {
		kws := make([]Keyword, maxNegativeKeywords)
		for i := range kws {
			kws[i] = Keyword{Text: strings.Repeat("a", i+1), MatchType: MatchTypeBroad}
		}
		got, err := validateNegativeKeywords(kws)
		if err != nil {
			t.Fatalf("a list exactly at the cap must be accepted, got %v", err)
		}
		if len(got) != maxNegativeKeywords {
			t.Errorf("got %d keywords, want %d", len(got), maxNegativeKeywords)
		}
	})
}

// campaignNegativeKeywordCreate must emit `negative` explicitly. A campaign
// keyword criterion with negative=false is a POSITIVE campaign-level keyword,
// which this client never means to create, so the field must be impossible to
// omit by accident.
func TestCampaignNegativeKeywordCreateAlwaysEmitsNegative(t *testing.T) {
	b, err := json.Marshal(campaignNegativeKeywordCreate{
		Campaign: "customers/1/campaigns/2",
		Negative: true,
		Keyword:  &keywordInfo{Text: "free", MatchType: MatchTypeBroad},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"negative":true`) {
		t.Errorf("payload %s is missing an explicit negative field", b)
	}

	// Even the zero value must appear on the wire rather than vanish.
	b, err = json.Marshal(campaignNegativeKeywordCreate{Campaign: "customers/1/campaigns/2"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"negative":false`) {
		t.Errorf("payload %s omits negative when false; it must always be explicit", b)
	}
}

// ---- validateCPCBid ---------------------------------------------------------

func TestValidateCPCBid(t *testing.T) {
	t.Run("zero means unset and invents no default", func(t *testing.T) {
		micros, set, err := validateCPCBid(0)
		if err != nil || set || micros != 0 {
			t.Errorf("validateCPCBid(0) = (%d, %v, %v), want (0, false, nil)", micros, set, err)
		}
	})

	t.Run("converts to micros with rounding, not truncation", func(t *testing.T) {
		micros, set, err := validateCPCBid(2.01)
		if err != nil || !set {
			t.Fatalf("validateCPCBid(2.01) = (%d, %v, %v)", micros, set, err)
		}
		if micros != 2_010_000 {
			t.Errorf("got %d micros, want 2010000 (truncation would give 2009999)", micros)
		}
	})

	t.Run("bounds are inclusive", func(t *testing.T) {
		for _, bid := range []float64{minCPCBid, maxCPCBid} {
			if _, set, err := validateCPCBid(bid); err != nil || !set {
				t.Errorf("validateCPCBid(%v) must be accepted, got set=%v err=%v", bid, set, err)
			}
		}
	})

	t.Run("rejects out-of-range and non-finite bids", func(t *testing.T) {
		for _, bid := range []float64{
			minCPCBid / 2,
			maxCPCBid + 1,
			-1,
			math.NaN(),
			math.Inf(1),
			math.Inf(-1),
		} {
			if _, _, err := validateCPCBid(bid); err == nil {
				t.Errorf("validateCPCBid(%v) must be an error", bid)
			}
		}
	})
}

// cpcBidMicros must disappear from the ad group create when unset — an explicit
// 0 is a different request (a zero bid) from no bid at all.
func TestAdGroupCreateOmitsUnsetCPCBid(t *testing.T) {
	b, err := json.Marshal(adGroupCreate{Name: "ag", Campaign: "customers/1/campaigns/2", Status: StatusPaused, Type: adGroupTypeSearchStandard})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "cpcBidMicros") {
		t.Errorf("payload %s sends cpcBidMicros when no bid was supplied", b)
	}

	b, err = json.Marshal(adGroupCreate{Name: "ag", CpcBidMicros: 2_500_000})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"cpcBidMicros":2500000`) {
		t.Errorf("payload %s is missing the supplied bid", b)
	}
}

// ---- validateFlightWindow ---------------------------------------------------

func TestValidateFlightWindow(t *testing.T) {
	t.Run("both empty yields no fields", func(t *testing.T) {
		start, end, err := validateFlightWindow("", "")
		if err != nil || start != "" || end != "" {
			t.Errorf("got (%q, %q, %v), want two empty strings and no error", start, end, err)
		}
	})

	t.Run("renders the account-timezone day boundaries", func(t *testing.T) {
		start, end, err := validateFlightWindow("2026-08-01", "2026-08-31")
		if err != nil {
			t.Fatalf("validateFlightWindow: %v", err)
		}
		if start != "2026-08-01 00:00:00" {
			t.Errorf("start = %q, want the start-of-day boundary", start)
		}
		if end != "2026-08-31 23:59:59" {
			t.Errorf("end = %q, want the end-of-day boundary", end)
		}
	})

	t.Run("each date is independently optional", func(t *testing.T) {
		start, end, err := validateFlightWindow("2026-08-01", "")
		if err != nil || start == "" || end != "" {
			t.Errorf("start only: got (%q, %q, %v)", start, end, err)
		}
		start, end, err = validateFlightWindow("", "2026-08-31")
		if err != nil || start != "" || end == "" {
			t.Errorf("end only: got (%q, %q, %v)", start, end, err)
		}
	})

	// time.Parse("2006-01-02", …) accepts single-digit months/days on its own,
	// which is exactly what the format regex exists to catch.
	t.Run("rejects formats time.Parse would otherwise accept", func(t *testing.T) {
		for _, d := range []string{"2026-1-05", "2026-01-5", "26-01-05", "2026/01/05", " 2026-01-05"} {
			if _, _, err := validateFlightWindow(d, ""); err == nil {
				t.Errorf("start date %q must be refused as malformed", d)
			}
			if _, _, err := validateFlightWindow("", d); err == nil {
				t.Errorf("end date %q must be refused as malformed", d)
			}
		}
	})

	t.Run("rejects impossible calendar dates", func(t *testing.T) {
		if _, _, err := validateFlightWindow("2026-02-30", ""); err == nil {
			t.Error("2026-02-30 must be refused as not a real date")
		}
	})

	t.Run("end must be after start when both are present", func(t *testing.T) {
		if _, _, err := validateFlightWindow("2026-08-31", "2026-08-01"); err == nil {
			t.Error("an end before the start must be refused")
		}
		if _, _, err := validateFlightWindow("2026-08-01", "2026-08-01"); err == nil {
			t.Error("an end equal to the start must be refused")
		}
	})

	// This client does not know the ad account's timezone, so a UTC "today" would
	// refuse a start date Google accepts for an account hours behind. Over-refusal
	// is the failure that matters here.
	t.Run("a past start date is NOT refused", func(t *testing.T) {
		if _, _, err := validateFlightWindow("2020-01-01", ""); err != nil {
			t.Errorf("a past start date must be accepted, got %v", err)
		}
	})
}

// The v23 field names, and their absence when unset. Sending the pre-v23
// start_date/end_date is rejected as an unrecognized field AFTER the budget
// mutate has committed, so the spelling is load-bearing.
func TestCampaignCreateFlightDateFields(t *testing.T) {
	b, err := json.Marshal(campaignCreate{Name: "c"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, f := range []string{"startDateTime", "endDateTime", "startDate", "endDate"} {
		if strings.Contains(string(b), f) {
			t.Errorf("payload %s emits %q when no flight window was supplied", b, f)
		}
	}

	b, err = json.Marshal(campaignCreate{Name: "c", StartDateTime: "2026-08-01 00:00:00", EndDateTime: "2026-08-31 23:59:59"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"startDateTime":"2026-08-01 00:00:00"`) ||
		!strings.Contains(string(b), `"endDateTime":"2026-08-31 23:59:59"`) {
		t.Errorf("payload %s is missing the v23 date-time fields", b)
	}
}

func TestDemandGenCampaignCreateFlightDateFields(t *testing.T) {
	b, err := json.Marshal(demandGenCampaignCreate{Name: "c"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "DateTime") {
		t.Errorf("payload %s emits a date field when no flight window was supplied", b)
	}

	b, err = json.Marshal(demandGenCampaignCreate{Name: "c", StartDateTime: "2026-08-01 00:00:00"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"startDateTime":"2026-08-01 00:00:00"`) {
		t.Errorf("payload %s is missing the v23 start field", b)
	}
}

func TestFlightWindowStep(t *testing.T) {
	// An omitted date is reported as the default it leaves in place, never as a blank.
	got := flightWindowStep("", "")
	if !strings.Contains(got, "no start date set") || !strings.Contains(got, "no end date") {
		t.Errorf("flightWindowStep(\"\", \"\") = %q, want both defaults named", got)
	}
	got = flightWindowStep("2026-08-01 00:00:00", "2026-08-31 23:59:59")
	if !strings.Contains(got, "2026-08-01 00:00:00") || !strings.Contains(got, "2026-08-31 23:59:59") {
		t.Errorf("flightWindowStep = %q, want the exact strings sent upstream", got)
	}
	if !strings.Contains(got, "account timezone") {
		t.Errorf("flightWindowStep = %q, want the timezone basis stated", got)
	}
}

// ---- the create cascade end to end -----------------------------------------

// TestCreateCampaign_ServingReadinessCascade pins what actually goes on the wire
// when all three serving-readiness inputs are supplied together: the flight
// window on the campaign create, the negatives as their own campaign-level
// mutate between the campaign and the ad group, and the bid on the ad group.
func TestCreateCampaign_ServingReadinessCascade(t *testing.T) {
	var campaignBody, criteriaBody, adGroupBody map[string]any
	var order []string

	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			order = append(order, "budget")
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			order = append(order, "campaign")
			campaignBody = decode(t, r)
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			order = append(order, "campaignCriteria")
			criteriaBody = decode(t, r)
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/222~555"},`+
				`{"resourceName":"customers/1234567890/campaignCriteria/222~556"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			order = append(order, "adGroups")
			adGroupBody = decode(t, r)
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			order = append(order, "adGroupCriteria")
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			order = append(order, "adGroupAds")
			okAdGroupAd(w, r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))

	in := sampleInput()
	in.Keywords = []Keyword{{Text: "kubecon", MatchType: MatchTypeBroad}}
	in.NegativeKeywords = []Keyword{
		{Text: "free", MatchType: MatchTypeBroad},
		{Text: "jobs", MatchType: MatchTypePhrase},
	}
	in.CPCBid = 2.50
	in.StartDate = "2026-08-01"
	in.EndDate = "2026-08-31"

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	// The negatives are campaign-level and go on BEFORE the ad group, so a failure
	// has as little built on top of it as possible.
	wantOrder := []string{"budget", "campaign", "campaignCriteria", "adGroups", "adGroupAds", "adGroupCriteria"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("call order = %v, want %v", order, wantOrder)
	}

	cop := firstCreate(t, campaignBody)
	if cop["startDateTime"] != "2026-08-01 00:00:00" || cop["endDateTime"] != "2026-08-31 23:59:59" {
		t.Errorf("flight window = %v / %v", cop["startDateTime"], cop["endDateTime"])
	}

	ops, _ := criteriaBody["operations"].([]any)
	if len(ops) != 2 {
		t.Fatalf("campaignCriteria operations = %d, want 2", len(ops))
	}
	for i, raw := range ops {
		op, _ := raw.(map[string]any)
		create, _ := op["create"].(map[string]any)
		if create["negative"] != true {
			t.Errorf("operation %d: negative = %v, want true", i, create["negative"])
		}
		if create["campaign"] != "customers/1234567890/campaigns/222" {
			t.Errorf("operation %d: campaign = %v", i, create["campaign"])
		}
		if _, ok := create["keyword"].(map[string]any); !ok {
			t.Errorf("operation %d: missing keyword payload", i)
		}
	}
	if len(res.NegativeKeywordCriteriaIDs) != 2 ||
		res.NegativeKeywordCriteriaIDs[0] != "555" || res.NegativeKeywordCriteriaIDs[1] != "556" {
		t.Errorf("NegativeKeywordCriteriaIDs = %v", res.NegativeKeywordCriteriaIDs)
	}

	agop := firstCreate(t, adGroupBody)
	if agop["cpcBidMicros"] != "2500000" && agop["cpcBidMicros"] != float64(2_500_000) {
		t.Errorf("cpcBidMicros = %#v, want 2500000", agop["cpcBidMicros"])
	}

	joined := strings.Join(res.Steps, "\n")
	for _, want := range []string{"2026-08-01 00:00:00", "Negative keywords applied: 2", "CPC bid 2.50"} {
		if !strings.Contains(joined, want) {
			t.Errorf("steps are missing %q:\n%s", want, joined)
		}
	}
}

// An invalid serving-readiness input must be refused BEFORE the budget mutate —
// the orphan-avoidance contract every other preflight field already holds to.
func TestCreateCampaign_ServingReadinessFailsBeforeAnyMutate(t *testing.T) {
	for name, mutate := range map[string]func(*CampaignInput){
		"bad negative keyword": func(in *CampaignInput) {
			in.NegativeKeywords = []Keyword{{Text: "free", MatchType: "FUZZY"}}
		},
		"out-of-range bid":  func(in *CampaignInput) { in.CPCBid = maxCPCBid + 1 },
		"malformed date":    func(in *CampaignInput) { in.StartDate = "2026-8-1" },
		"end before start":  func(in *CampaignInput) { in.StartDate, in.EndDate = "2026-08-31", "2026-08-01" },
		"impossible date":   func(in *CampaignInput) { in.EndDate = "2026-02-30" },
		"non-finite bid":    func(in *CampaignInput) { in.CPCBid = math.NaN() },
		"too many negative": func(in *CampaignInput) { in.NegativeKeywords = make([]Keyword, maxNegativeKeywords+1) },
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			fail := func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusInternalServerError)
			}
			c := newCampaignClient(t, fail, fail)
			in := sampleInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if res != nil {
				t.Errorf("a pre-mutate failure must return a nil result, got %+v", res)
			}
			if called {
				t.Error("a local input error must not reach any mutate")
			}
			// The same input must be refused identically on the adoption path.
			if err := c.ValidateCampaignInput(in); err == nil {
				t.Error("ValidateCampaignInput must refuse what CreateCampaign refuses")
			}
		})
	}
}
