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
	"sync"
	"sync/atomic"
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
		micros, err := validateCPCBid(0)
		if err != nil || micros != 0 {
			t.Errorf("validateCPCBid(0) = (%d, %v), want (0, nil)", micros, err)
		}
	})

	t.Run("converts to micros with rounding, not truncation", func(t *testing.T) {
		micros, err := validateCPCBid(2.01)
		if err != nil {
			t.Fatalf("validateCPCBid(2.01) = (%d, %v)", micros, err)
		}
		if micros != 2_010_000 {
			t.Errorf("got %d micros, want 2010000 (truncation would give 2009999)", micros)
		}
	})

	t.Run("bounds are inclusive", func(t *testing.T) {
		for _, bid := range []float64{minCPCBid, maxCPCBid} {
			if _, err := validateCPCBid(bid); err != nil {
				t.Errorf("validateCPCBid(%v) must be accepted, got err=%v", bid, err)
			}
		}
	})

	// The whole reason validateCPCBid can report "unset" as a plain 0 is that an
	// accepted bid can never round to it: omitempty is what omits the field, so a
	// legal bid rounding to 0 micros would be silently dropped instead of sent.
	t.Run("the smallest accepted bid is far from the unset zero", func(t *testing.T) {
		micros, err := validateCPCBid(minCPCBid)
		if err != nil {
			t.Fatalf("validateCPCBid(minCPCBid) = %v", err)
		}
		if micros <= 0 {
			t.Errorf("minCPCBid rounds to %d micros — omitempty would drop it as if unset", micros)
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
			if _, err := validateCPCBid(bid); err == nil {
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

	t.Run("end must not be before start when both are present", func(t *testing.T) {
		if _, _, err := validateFlightWindow("2026-08-31", "2026-08-01"); err == nil {
			t.Error("an end before the start must be refused")
		}
	})

	// A one-day flight is an ordinary ad buy (an event promo), and the explicit day
	// boundaries make it a real 24-hour window rather than a zero-length instant.
	// Refusing it would be refusing a create Google accepts.
	t.Run("a same-day window is accepted and covers the whole day", func(t *testing.T) {
		start, end, err := validateFlightWindow("2026-08-01", "2026-08-01")
		if err != nil {
			t.Fatalf("a same-day flight window must be accepted, got %v", err)
		}
		if start != "2026-08-01 00:00:00" || end != "2026-08-01 23:59:59" {
			t.Errorf("same-day window = %q / %q, want the full-day boundaries", start, end)
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
	// httptest runs every handler on its own goroutine, so each of these is written
	// on one goroutine and read by the test on another. The mutex is the
	// happens-before edge; without it `make test`'s -race flags the handoff, and on
	// a loaded runner the assertions can read a half-written map.
	var mu sync.Mutex
	var campaignBody, criteriaBody, adGroupBody map[string]any
	var order []string
	// Decoding inside the handler must not call t.Fatalf — FailNow is only valid on
	// the test goroutine. decodeRequest is the handler-safe decoder the package
	// already provides for exactly this; failures are reported with t.Errorf and the
	// assertions below then fail on the nil body.
	record := func(step string, r *http.Request) map[string]any {
		m, err := decodeRequest(r)
		if err != nil {
			t.Errorf("decode %s request body: %v", step, err)
		}
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
		return m
	}

	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			mu.Lock()
			order = append(order, "budget")
			mu.Unlock()
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			body := record("campaign", r)
			mu.Lock()
			campaignBody = body
			mu.Unlock()
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			body := record("campaignCriteria", r)
			mu.Lock()
			criteriaBody = body
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/222~555"},`+
				`{"resourceName":"customers/1234567890/campaignCriteria/222~556"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			body := record("adGroups", r)
			mu.Lock()
			adGroupBody = body
			mu.Unlock()
			okAdGroup(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			mu.Lock()
			order = append(order, "adGroupCriteria")
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			mu.Lock()
			order = append(order, "adGroupAds")
			mu.Unlock()
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

	// CreateCampaign has returned, so every handler has finished — but the race
	// detector reasons about the lock, not about that ordering, and so should a
	// reader. Everything below is read under it.
	mu.Lock()
	defer mu.Unlock()

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
			// atomic, not a plain bool: this is written on a handler goroutine and
			// read on the test goroutine below.
			var called atomic.Bool
			fail := func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
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
			if called.Load() {
				t.Error("a local input error must not reach any mutate")
			}
			// The same input must be refused identically on the adoption path.
			if err := c.ValidateCampaignInput(in); err == nil {
				t.Error("ValidateCampaignInput must refuse what CreateCampaign refuses")
			}
		})
	}
}

// ---- the negative-keyword failure branches ----------------------------------
//
// These four mirror the geo path's coverage (geo_test.go:297, :459, :482, :542),
// which is the function createCampaignNegativeKeywords was modelled on line for
// line. The property they pin is the riskiest one in this feature: the campaign
// already exists and already costs money by the time the negatives mutate runs,
// so a failure must come back ALONGSIDE the result and never as (nil, err). With
// nothing pinning it, a refactor to `return nil, negErr` passes the whole suite.

// negativeKeywordInput is a Search input whose ONLY campaignCriteria traffic is the
// negatives — no GeoTargets, so the handler under test is never shared with the geo
// mutate and a failure can only have come from the negatives.
func negativeKeywordInput() CampaignInput {
	in := sampleInput()
	in.NegativeKeywords = []Keyword{
		{Text: "free", MatchType: MatchTypeBroad},
		{Text: "jobs", MatchType: MatchTypePhrase},
	}
	return in
}

func TestCreateCampaign_NegativeKeywordFailureKeepsCampaignPartial(t *testing.T) {
	// A definite 4xx: the campaign is created and PAUSED, with no negatives on it.
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
		},
		failHandler(t, "adGroupCriteria:mutate (the cascade must stop at the negatives)"))

	res, err := c.CreateCampaign(context.Background(), negativeKeywordInput())
	if err == nil {
		t.Fatal("expected an error")
	}
	if res == nil {
		t.Fatal("expected a non-nil partial result: the campaign exists upstream and must stay reconcilable")
	}
	if res.CampaignID != "222" {
		t.Errorf("CampaignID = %q, want the created campaign id", res.CampaignID)
	}
	if len(res.NegativeKeywordCriteriaIDs) != 0 {
		t.Errorf("NegativeKeywordCriteriaIDs = %v, want none recorded for a failed mutate", res.NegativeKeywordCriteriaIDs)
	}
	// The sentence an operator acts on: it says what the campaign is missing and
	// what that means if someone un-pauses it.
	if !strings.Contains(err.Error(), "NO negative keywords") {
		t.Errorf("error does not state the consequence: %v", err)
	}
}

// A 2xx carrying fewer results than operations is UNCONFIRMED, not success: an
// unknown number of the exclusions may have committed, so no ids may be persisted.
func TestCreateCampaign_ShortNegativeKeywordResponseIsUnconfirmed(t *testing.T) {
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			// Two operations were sent; one result comes back.
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/222~555"}]}`)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	res, err := c.CreateCampaign(context.Background(), negativeKeywordInput())
	if err == nil {
		t.Fatal("a short mutate response must not be treated as success")
	}
	if res == nil || res.CampaignID != "222" {
		t.Fatalf("expected the partial campaign result, got %+v", res)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a short response must be reported as UNCONFIRMED, got %v", err)
	}
	if len(res.NegativeKeywordCriteriaIDs) != 0 {
		t.Errorf("no ids may be persisted from an unconfirmed mutate, got %v", res.NegativeKeywordCriteriaIDs)
	}
}

// firstResourceName-style parsing only extracts a trailing id; without the kind and
// account checks a 2xx naming someone else's resource would be persisted as ours.
func TestCreateCampaign_RejectsNegativeKeywordNamesThatAreNotOurs(t *testing.T) {
	for name, results := range map[string]string{
		"malformed resource name": `{"results":[{"resourceName":"nonsense"},{"resourceName":"nonsense"}]}`,
		"wrong resource kind":     `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/222~555"},{"resourceName":"customers/1234567890/adGroupCriteria/222~556"}]}`,
		"a different campaign id": `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/999~555"},{"resourceName":"customers/1234567890/campaignCriteria/999~556"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := results
			c := newGeoClient(t,
				func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) },
				failHandler(t, "adGroupCriteria:mutate"))

			res, err := c.CreateCampaign(context.Background(), negativeKeywordInput())
			if err == nil {
				t.Fatal("a criterion resource name that is not ours must not be accepted as confirmed")
			}
			if res == nil || res.CampaignID != "222" {
				t.Fatalf("expected the partial campaign result, got %+v", res)
			}
			if !strings.Contains(err.Error(), "UNCONFIRMED") {
				t.Errorf("want UNCONFIRMED, got %v", err)
			}
			if len(res.NegativeKeywordCriteriaIDs) != 0 {
				t.Errorf("ids parsed from an untrusted response must not be persisted, got %v", res.NegativeKeywordCriteriaIDs)
			}
		})
	}
}

// 429 on a criteria mutate is NOT retried: the request is not idempotent, and a
// retry could double-create the exclusions. One call, then the ambiguous report.
func TestCreateCampaign_NegativeKeyword429IsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	res, err := c.CreateCampaign(context.Background(), negativeKeywordInput())
	if err == nil {
		t.Fatal("expected an error")
	}
	if res == nil || res.CampaignID != "222" {
		t.Fatalf("expected the partial campaign result, got %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("campaignCriteria:mutate called %d times, want exactly 1 — a retry could double-create the exclusions", got)
	}
}
