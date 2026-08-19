// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// keywordRowJSON builds one keyword_view result row.
func keywordRowJSON(criterionID, adGroupID, campaignID, text, matchType, status string, impressions, clicks, cost int) string {
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"status":%q,"keyword":{"text":%q,"matchType":%q}},`+
		`"adGroup":{"id":%q},"campaign":{"id":%q},`+
		`"metrics":{"impressions":"%d","clicks":"%d","costMicros":"%d"}}`,
		criterionID, status, text, matchType, adGroupID, campaignID, impressions, clicks, cost)
}

func TestGetKeywordPerformance_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+
			keywordRowJSON("305729261", "176216228", "21234567890", "kubernetes training", "EXACT", "ENABLED", 1000, 40, 25000000)+
			`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(kp.Rows))
	}
	got := kp.Rows[0]
	if got.CriterionID != "305729261" || got.AdGroupID != "176216228" || got.CampaignID != "21234567890" {
		t.Errorf("ids = %+v", got)
	}
	if got.Text != "kubernetes training" || got.MatchType != "EXACT" || got.Status != "ENABLED" {
		t.Errorf("fields = %+v", got)
	}
	if got.Impressions != 1000 || got.Clicks != 40 || got.CostMicros != 25_000_000 {
		t.Errorf("metrics = %+v", got)
	}
	if want := 0.04; got.Ctr != want {
		t.Errorf("Ctr = %v, want %v", got.Ctr, want)
	}
	if kp.Truncated {
		t.Errorf("Truncated = true, want false for a single row")
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()
	// The window must reach the query as the allow-listed GAQL literal, and REMOVED criteria
	// must be excluded — a removed keyword can never serve, so offering it as actionable is
	// an action the caller cannot meaningfully take.
	if !strings.Contains(body, "DURING LAST_30_DAYS") {
		t.Errorf("query missing window: %s", body)
	}
	// The status predicate itself is asserted by
	// TestGetKeywordPerformance_QueryAllowListsLiveStatuses, which pins the ALLOW-LIST form
	// specifically — an exclusion would admit UNSPECIFIED/UNKNOWN/"" as actionable rows.
	// The LIMIT must be the cap PLUS ONE — that extra row is the only way a full page can be
	// told from a truncated one.
	if !strings.Contains(body, fmt.Sprintf("LIMIT %d", maxKeywordRows+1)) {
		t.Errorf("query LIMIT is not maxKeywordRows+1: %s", body)
	}
	if !strings.Contains(body, "ORDER BY metrics.impressions DESC") {
		t.Errorf("query is not ordered by impressions desc: %s", body)
	}
}

// A cap without a truncation signal is a silent lie: a caller receiving exactly the cap
// cannot tell a small account from a truncated large one and would total the slice as the
// whole account's spend.
func TestGetKeywordPerformance_TruncatesAndReportsIt(t *testing.T) {
	rows := make([]string, 0, maxKeywordRows+1)
	for i := 0; i < maxKeywordRows+1; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "21234567890", "kw", "BROAD", "ENABLED", 10, 1, 100))
	}
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(rows, ",")+`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != maxKeywordRows {
		t.Fatalf("rows = %d, want the cap %d", len(kp.Rows), maxKeywordRows)
	}
	if !kp.Truncated {
		t.Fatalf("Truncated = false, but the account returned more than the cap")
	}
}

// Exactly the cap, with no probe row, is NOT truncated. Guards the off-by-one in the other
// direction: reporting truncation for a complete answer tells a caller their totals are a
// partial slice when they are the whole account.
func TestGetKeywordPerformance_ExactlyCapIsNotTruncated(t *testing.T) {
	rows := make([]string, 0, maxKeywordRows)
	for i := 0; i < maxKeywordRows; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "21234567890", "kw", "BROAD", "ENABLED", 10, 1, 100))
	}
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(rows, ",")+`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != maxKeywordRows {
		t.Fatalf("rows = %d, want %d", len(kp.Rows), maxKeywordRows)
	}
	if kp.Truncated {
		t.Fatalf("Truncated = true for an exactly-full page with no probe row")
	}
}

// A row that cannot be acted on must not be returned: the keyword-actions endpoint needs
// BOTH ids, so a row missing one hands the caller a button that cannot work.
func TestGetKeywordPerformance_MissingIDsIsAnError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"305729261","keyword":{"text":"x","matchType":"EXACT"}},`+
			`"adGroup":{},"campaign":{"id":"21234567890"},"metrics":{"impressions":"5","clicks":"1","costMicros":"10"}}]}`)
	})
	if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
		t.Fatal("expected an error for a row missing its ad group id, got nil")
	}
}

// The window is concatenated into the GAQL string, so an unvalidated value is a query
// injection vector exactly as an unvalidated id is.
func TestGetKeywordPerformance_RejectsUnknownWindowBeforeCalling(t *testing.T) {
	called := false
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	_, err := c.GetKeywordPerformance(context.Background(), MetricsWindow("LAST_30_DAYS'; DROP--"), []string{"555"})
	if err == nil {
		t.Fatal("expected an error for an unknown window, got nil")
	}
	if called {
		t.Fatal("the platform was contacted despite an invalid window")
	}
}

func TestGetKeywordPerformance_EmptyWindowDefaults(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	kp, err := c.GetKeywordPerformance(context.Background(), "", []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if kp.Window != defaultMetricsWindow {
		t.Errorf("Window = %q, want the default %q", kp.Window, defaultMetricsWindow)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if !strings.Contains(body, "DURING "+string(defaultMetricsWindow)) {
		t.Errorf("query did not use the default window: %s", body)
	}
}

// Google omits zero-valued metrics from REST JSON, leaving empty strings. Treating those as
// a parse error would fail every read of a quiet keyword.
func TestGetKeywordPerformance_OmittedZeroMetricsParseAsZero(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"1","status":"ENABLED","keyword":{"text":"x","matchType":"EXACT"}},`+
			`"adGroup":{"id":"2"},"campaign":{"id":"3"},"metrics":{}}]}`)
	})
	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if kp.Rows[0].Impressions != 0 || kp.Rows[0].Clicks != 0 || kp.Rows[0].CostMicros != 0 {
		t.Errorf("omitted metrics did not parse as zero: %+v", kp.Rows[0])
	}
	if kp.Rows[0].Ctr != 0 {
		t.Errorf("Ctr = %v, want 0 (must not divide by zero)", kp.Rows[0].Ctr)
	}
}

// An unparseable metric must name the FIELD and never the value: the value comes from the
// upstream body and is rendered into a log line.
func TestGetKeywordPerformance_ParseErrorNamesFieldNotValue(t *testing.T) {
	const poison = "12\n\ninjected-log-line"
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"results": []any{map[string]any{
			"adGroupCriterion": map[string]any{"criterionId": "1", "status": "ENABLED",
				"keyword": map[string]any{"text": "x", "matchType": "EXACT"}},
			"adGroup":  map[string]any{"id": "2"},
			"campaign": map[string]any{"id": "3"},
			"metrics":  map[string]any{"impressions": poison, "clicks": "1", "costMicros": "1"},
		}}})
		_, _ = w.Write(body)
	})
	_, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if !strings.Contains(err.Error(), "impressions") {
		t.Errorf("error does not name the failing field: %v", err)
	}
	if strings.Contains(err.Error(), "injected-log-line") {
		t.Errorf("error echoed the upstream value: %v", err)
	}
}

// ─── audience ───

func TestGetAudienceInsights_ReadsAllThreeBreakdowns(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		queries = append(queries, string(b))
		n := len(queries)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1: // age
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_25_34"}},"metrics":{"impressions":"100","clicks":"10","costMicros":"500"}}]}`)
		case 2: // gender
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"gender":{"type":"MALE"}},"metrics":{"impressions":"60","clicks":"6","costMicros":"300"}}]}`)
		default: // device
			_, _ = io.WriteString(w, `{"results":[{"segments":{"device":"MOBILE"},"metrics":{"impressions":"80","clicks":"8","costMicros":"400"}}]}`)
		}
	})

	ai, err := c.GetAudienceInsights(context.Background(), WindowLast7Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if len(ai.Buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(ai.Buckets))
	}
	byDim := map[string]AudienceBucket{}
	for _, b := range ai.Buckets {
		byDim[b.Dimension] = b
	}
	if got := byDim[DimensionAge]; got.Value != "AGE_RANGE_25_34" || got.Impressions != 100 {
		t.Errorf("age bucket = %+v", got)
	}
	if got := byDim[DimensionGender]; got.Value != "MALE" || got.Impressions != 60 {
		t.Errorf("gender bucket = %+v", got)
	}
	if got := byDim[DimensionDevice]; got.Value != "MOBILE" || got.Impressions != 80 {
		t.Errorf("device bucket = %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 3 {
		t.Fatalf("issued %d queries, want 3", len(queries))
	}
	// Each breakdown must query its own resource. A device breakdown read FROM a criterion
	// view (or vice versa) returns nothing and would surface as an empty dimension.
	if !strings.Contains(queries[0], "FROM age_range_view") {
		t.Errorf("age query: %s", queries[0])
	}
	if !strings.Contains(queries[1], "FROM gender_view") {
		t.Errorf("gender query: %s", queries[1])
	}
	if !strings.Contains(queries[2], "segments.device") || !strings.Contains(queries[2], "FROM campaign") {
		t.Errorf("device query: %s", queries[2])
	}
}

// The device breakdown segments the CAMPAIGN resource, so it returns one row per
// (campaign, device). Taking the last row per device would report a single campaign's
// numbers as the whole account's.
func TestGetAudienceInsights_AggregatesRepeatedBucketsAcrossCampaigns(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "FROM campaign") {
			_, _ = io.WriteString(w, `{"results":[`+
				`{"segments":{"device":"MOBILE"},"metrics":{"impressions":"100","clicks":"10","costMicros":"1000"}},`+
				`{"segments":{"device":"MOBILE"},"metrics":{"impressions":"300","clicks":"30","costMicros":"3000"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	ai, err := c.GetAudienceInsights(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	var mobile *AudienceBucket
	for i := range ai.Buckets {
		if ai.Buckets[i].Value == "MOBILE" {
			mobile = &ai.Buckets[i]
		}
	}
	if mobile == nil {
		t.Fatal("no MOBILE bucket")
	}
	if mobile.Impressions != 400 || mobile.Clicks != 40 || mobile.CostMicros != 4000 {
		t.Fatalf("MOBILE bucket did not aggregate both campaigns: %+v", mobile)
	}
	// CTR must be computed after aggregation, not averaged per row.
	if want := 0.1; mobile.Ctr != want {
		t.Errorf("Ctr = %v, want %v", mobile.Ctr, want)
	}
}

// A failure in ANY breakdown fails the whole read: a caller shown two of three dimensions
// cannot tell the third is missing rather than empty, and would re-target on a partial picture.
func TestGetAudienceInsights_OneBreakdownFailureFailsTheRead(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "FROM gender_view") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	if _, err := c.GetAudienceInsights(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
		t.Fatal("expected the whole read to fail when one breakdown fails, got nil")
	}
}

// UNDETERMINED is real unattributed traffic. Dropping it makes the buckets under-sum with
// nothing indicating why.
func TestGetAudienceInsights_KeepsUndeterminedBucket(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "FROM age_range_view") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_UNDETERMINED"}},"metrics":{"impressions":"900","clicks":"5","costMicros":"50"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	ai, err := c.GetAudienceInsights(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	found := false
	for _, b := range ai.Buckets {
		if b.Value == "AGE_RANGE_UNDETERMINED" && b.Impressions == 900 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the UNDETERMINED bucket was dropped: %+v", ai.Buckets)
	}
}

// ─── keyword actions ───

func TestValidateKeywordActions_RejectsNonNumericIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action KeywordAction
	}{
		{"ad group not numeric", KeywordAction{AdGroupID: "176216228'; --", CriterionID: "305729261", Action: "PAUSE"}},
		{"criterion not numeric", KeywordAction{AdGroupID: "176216228", CriterionID: "abc", Action: "PAUSE"}},
		{"ad group empty", KeywordAction{AdGroupID: "", CriterionID: "305729261", Action: "PAUSE"}},
		{"criterion empty", KeywordAction{AdGroupID: "176216228", CriterionID: "", Action: "PAUSE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateKeywordActions([]KeywordAction{tc.action}); err == nil {
				t.Fatalf("expected a rejection for %+v, got nil", tc.action)
			}
		})
	}
}

func TestValidateKeywordActions_RejectsUnsupportedAction(t *testing.T) {
	// ENABLE is the notable one: this surface must only ever reduce what serves, so
	// accepting it would let a caller restart spend through an endpoint documented as
	// incapable of doing so.
	for _, action := range []string{"ENABLE", "DELETE", ""} {
		if _, err := ValidateKeywordActions([]KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: action}}); err == nil {
			t.Fatalf("expected a rejection for action %q, got nil", action)
		}
	}
}

func TestValidateKeywordActions_RejectsEmptyAndOverLongBatches(t *testing.T) {
	if _, err := ValidateKeywordActions(nil); err == nil {
		t.Fatal("expected an empty batch to be rejected")
	}
	over := make([]KeywordAction, maxKeywordActions+1)
	for i := range over {
		over[i] = KeywordAction{AdGroupID: "1", CriterionID: fmt.Sprintf("%d", 100+i), Action: "PAUSE"}
	}
	if _, err := ValidateKeywordActions(over); err == nil {
		t.Fatalf("expected a batch of %d to be rejected", len(over))
	}
}

// A duplicate is refused rather than de-duplicated: two entries can carry DIFFERENT actions
// (pause and remove) and there is no defensible way to pick one.
func TestValidateKeywordActions_RejectsDuplicateCriterion(t *testing.T) {
	_, err := ValidateKeywordActions([]KeywordAction{
		{AdGroupID: "1", CriterionID: "2", Action: "PAUSE"},
		{AdGroupID: "1", CriterionID: "2", Action: "REMOVE"},
	})
	if err == nil {
		t.Fatal("expected a duplicate criterion to be rejected")
	}
}

func TestValidateKeywordActions_NormalisesAction(t *testing.T) {
	out, err := ValidateKeywordActions([]KeywordAction{{AdGroupID: " 1 ", CriterionID: " 2 ", Action: " pause "}})
	if err != nil {
		t.Fatalf("ValidateKeywordActions: %v", err)
	}
	if out[0].Action != KeywordActionPause || out[0].AdGroupID != "1" || out[0].CriterionID != "2" {
		t.Fatalf("not normalised: %+v", out[0])
	}
}

func TestApplyKeywordActions_BuildsPauseAndRemoveOperations(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Both criteria resolve as POSITIVE keywords, so the mutate proceeds and this test can
		// go on asserting the operation payload it exists to assert.
		if strings.Contains(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[`+
				criterionRowJSON("176216228", "305729261", false)+`,`+
				criterionRowJSON("176216228", "999999999", false)+`]}`)
			return
		}
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[`+
			`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"},`+
			`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~999999999"}]}`)
	})

	out, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
		{AdGroupID: "176216228", CriterionID: "999999999", Action: KeywordActionRemove},
	})
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(out))
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()

	var req struct {
		Operations []struct {
			Update *struct {
				ResourceName string `json:"resourceName"`
				Status       string `json:"status"`
			} `json:"update"`
			UpdateMask string `json:"updateMask"`
			Remove     string `json:"remove"`
		} `json:"operations"`
		PartialFailure *bool `json:"partialFailure"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v (%s)", err, body)
	}
	if len(req.Operations) != 2 {
		t.Fatalf("operations = %d, want 2: %s", len(req.Operations), body)
	}
	// PAUSE is an update with an explicit status mask — without the mask Google ignores the
	// field and the keyword keeps serving while the call reports success.
	if req.Operations[0].Update == nil || req.Operations[0].Update.Status != StatusPaused {
		t.Errorf("first op is not a status update to PAUSED: %s", body)
	}
	if req.Operations[0].UpdateMask != "status" {
		t.Errorf("updateMask = %q, want status: %s", req.Operations[0].UpdateMask, body)
	}
	if req.Operations[0].Update.ResourceName != "customers/1234567890/adGroupCriteria/176216228~305729261" {
		t.Errorf("pause resource name = %q", req.Operations[0].Update.ResourceName)
	}
	// REMOVE is a remove operation naming the resource, never an update.
	if req.Operations[1].Remove != "customers/1234567890/adGroupCriteria/176216228~999999999" {
		t.Errorf("second op is not a remove: %s", body)
	}
	if req.Operations[1].Update != nil {
		t.Errorf("remove op also carried an update: %s", body)
	}
	// partialFailure must never be enabled: the batch's atomicity is the contract.
	if req.PartialFailure != nil && *req.PartialFailure {
		t.Errorf("partialFailure was enabled, breaking the all-or-nothing contract: %s", body)
	}
}

// A short or malformed mutate response must be UNCONFIRMED, never success: the mutations may
// have been applied, and this path changes spend.
func TestApplyKeywordActions_ShortResponseIsUnconfirmed(t *testing.T) {
	c := keywordCriterionServer(t,
		criterionRowJSON("176216228", "305729261", false)+`,`+criterionRowJSON("176216228", "999999999", false),
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`, nil)
	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
		{AdGroupID: "176216228", CriterionID: "999999999", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("expected UNCONFIRMED for a short mutate response, got nil")
	}
	// Structural, not textual: the service detects ambiguity through Unconfirmed(), so a
	// message-only assertion would agree with an error the service classifies as a definite
	// failure.
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("error is not structurally UNCONFIRMED: %v", err)
	}
}

// A response naming a DIFFERENT criterion than the operation addressed cannot be reported as
// a successful mutation of the requested keyword.
func TestApplyKeywordActions_MismatchedResourceNameIsUnconfirmed(t *testing.T) {
	c := keywordCriterionServer(t,
		criterionRowJSON("176216228", "305729261", false),
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~111111111"}`, nil)
	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	})
	if err == nil {
		t.Fatal("expected UNCONFIRMED for a mismatched resource name, got nil")
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("error is not structurally UNCONFIRMED: %v", err)
	}
}

// A resource name from ANOTHER customer must not be accepted: adGroupCriterionID rejects it,
// so a cross-account response cannot be reported as this project's successful mutation.
func TestApplyKeywordActions_ForeignCustomerResourceNameIsUnconfirmed(t *testing.T) {
	// The criterion resolves as a positive keyword, so the run reaches the resource-name check
	// this test is about rather than stopping at type resolution.
	c := keywordCriterionServer(t,
		criterionRowJSON("176216228", "305729261", false),
		`{"resourceName":"customers/9999999999/adGroupCriteria/176216228~305729261"}`, nil)
	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	})
	if err == nil {
		t.Fatal("expected UNCONFIRMED for a foreign-customer resource name, got nil")
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("error is not structurally UNCONFIRMED: %v", err)
	}
}

// An invalid batch must be refused before the platform is contacted at all.
func TestApplyKeywordActions_InvalidBatchNeverContactsPlatform(t *testing.T) {
	called := false
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	if _, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "bad", CriterionID: "305729261", Action: KeywordActionPause},
	}); err == nil {
		t.Fatal("expected a rejection, got nil")
	}
	if called {
		t.Fatal("the platform was contacted for an invalid batch")
	}
}

// A cancelled context must abort before the request exists, so a caller is never left unable
// to tell whether their keywords were changed.
func TestApplyKeywordActions_CancelledContextAbortsBeforeRequest(t *testing.T) {
	called := false
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ApplyKeywordActions(ctx, []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	})
	if err == nil {
		t.Fatal("expected an error for a cancelled context, got nil")
	}
	if called {
		t.Fatal("the platform was contacted with an already-cancelled context")
	}
	// Assert the GUARD's own diagnostic, not merely that some error came back. The transport
	// layer also refuses a cancelled context during token fetch, so a test checking only
	// "errored and did not call" passes with this guard deleted — it would be pinning the
	// backstop instead of the thing under test. The distinctive promise here is that NO
	// keyword was changed, which is what a caller needs told.
	if !strings.Contains(err.Error(), "no keyword was changed") {
		t.Errorf("error does not carry the pre-request guard's diagnostic: %v", err)
	}
}

// An ACCOUNT-WIDE read returns keywords this service never created, so an upstream value
// outside the API contract's closed enums is reachable no matter what the create path
// restricts itself to. The design declares Enum(...) on the RESPONSE and Goa emits that
// validation in the generated CLIENT — the server never validates its own response body — so
// an un-normalised passthrough makes a client reject the ENTIRE response over one row.
func TestGetKeywordPerformance_NormalisesUnrecognisedEnums(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status, matchType string
		wantStatus        string
		wantMatch         string
	}{
		{"unspecified", "UNSPECIFIED", "UNSPECIFIED", KeywordStatusUnknown, MatchTypeUnknown},
		{"google unknown", "UNKNOWN", "UNKNOWN", KeywordStatusUnknown, MatchTypeUnknown},
		{"absent proto field", "", "", KeywordStatusUnknown, MatchTypeUnknown},
		{"garbage", "WAT", "WAT", KeywordStatusUnknown, MatchTypeUnknown},
		{"recognised passes through", "ENABLED", "EXACT", StatusEnabled, MatchTypeExact},
		{"paused passes through", "PAUSED", "PHRASE", StatusPaused, MatchTypePhrase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[`+
					keywordRowJSON("777", "333", "555", "kw", tc.matchType, tc.status, 10, 1, 100)+`]}`)
			})
			kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
			if err != nil {
				t.Fatalf("GetKeywordPerformance: %v", err)
			}
			if kp.Rows[0].Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q — an out-of-enum value makes the generated client reject the whole response", kp.Rows[0].Status, tc.wantStatus)
			}
			if kp.Rows[0].MatchType != tc.wantMatch {
				t.Errorf("MatchType = %q, want %q", kp.Rows[0].MatchType, tc.wantMatch)
			}
			// The row is normalised, never dropped: a caller must be able to tell "Google said
			// something we don't handle" from "Google said nothing", and the ids/counters are
			// what it acts on.
			if kp.Rows[0].CriterionID != "777" || kp.Rows[0].Impressions != 10 {
				t.Errorf("row was altered beyond its enums: %+v", kp.Rows[0])
			}
		})
	}
}

// The status predicate must be an ALLOW-LIST, not an exclusion. `!= 'REMOVED'` leaves
// UNSPECIFIED, UNKNOWN and the empty string an omitted proto field decodes to being treated
// as live, and offers them as actionable rows.
func TestGetKeywordPerformance_QueryAllowListsLiveStatuses(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if !strings.Contains(body, "ad_group_criterion.status IN ('ENABLED', 'PAUSED')") {
		t.Errorf("query does not allow-list live statuses: %s", body)
	}
	if strings.Contains(body, "!= 'REMOVED'") {
		t.Errorf("query still uses an exclusion, which admits UNSPECIFIED/UNKNOWN/\"\": %s", body)
	}
}

// TestCampaignScopePredicate_EmptyListIsRefused pins the adapter's own refusal to build an
// unscoped query.
//
// The orchestrator already answers an empty scope without calling down here, so this is
// defence in depth — and it is worth having precisely because that guarantee lives in a
// different package. A future caller (another platform, a batch job, a refactor that inlines
// the read) that passes an empty slice must get an error, not an account-wide query. Returning
// an empty predicate string would be concatenated into the GAQL and silently restore the
// cross-project read on the shared customer.
func TestCampaignScopePredicate_EmptyListIsRefused(t *testing.T) {
	for _, ids := range [][]string{nil, {}} {
		if _, err := campaignScopePredicate(ids, "op"); err == nil {
			t.Errorf("campaignScopePredicate(%v) returned no error; an empty scope must never "+
				"produce a query, because an unscoped read returns every project's data", ids)
		}
	}
}

// The ids are concatenated into the GAQL string and GAQL has no parameterized queries, so a
// non-numeric id is an injection vector. Same digits-only rule GetCampaignMetrics applies.
func TestCampaignScopePredicate_RejectsNonNumericIDs(t *testing.T) {
	for _, bad := range []string{"1' OR '1'='1", "abc", "12 34", "", "-1"} {
		if _, err := campaignScopePredicate([]string{"111", bad}, "op"); err == nil {
			t.Errorf("campaignScopePredicate accepted %q; ids reach the query by concatenation", bad)
		}
	}
}

// The ids must render UNQUOTED. campaign.id is an int64 in GAQL, so quoting turns the
// predicate into a string comparison against a numeric column — which Google rejects or
// matches nothing, either way defeating the project scoping this predicate exists to enforce.
// get_campaign_test.go pins the same unquoted form for the same field on the single-id path;
// this is that assertion for the IN list.
func TestCampaignScopePredicate_RendersAnUnquotedINList(t *testing.T) {
	got, err := campaignScopePredicate([]string{" 111 ", "222"}, "op")
	if err != nil {
		t.Fatalf("campaignScopePredicate: %v", err)
	}
	if got != "campaign.id IN (111, 222)" {
		t.Errorf("predicate = %q, want the unquoted int64 form", got)
	}
	if strings.Contains(got, "'") {
		t.Errorf("predicate quotes the int64 campaign.id, making it a string comparison: %s", got)
	}
}

// keywordCriterionServer routes the two calls ApplyKeywordActions now makes: the
// criterion-type resolution search, then the mutate. searchResults is the raw results array
// the keyword_view query answers with; mutateResults is the mutate's.
//
// Routing on the path rather than on call order is deliberate — a test that assumed "first
// request is the search" would keep passing if the guard were dropped and the mutate went first.
func keywordCriterionServer(t *testing.T, searchResults, mutateResults string, mutateCalled *bool) *Client {
	t.Helper()
	var mu sync.Mutex
	return twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[`+searchResults+`]}`)
			return
		}
		if strings.Contains(r.URL.Path, "adGroupCriteria:mutate") {
			mu.Lock()
			if mutateCalled != nil {
				*mutateCalled = true
			}
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[`+mutateResults+`]}`)
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	})
}

// criterionRowJSON builds one keyword_view row for the type-resolution query, rendering
// `negative` as an explicit JSON boolean.
func criterionRowJSON(adGroupID, criterionID string, negative bool) string {
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"negative":%t},"adGroup":{"id":%q}}`,
		criterionID, negative, adGroupID)
}

// A POSITIVE keyword must still be actionable — the guard must refuse the wrong criterion
// types WITHOUT breaking the endpoint's actual job.
func TestApplyKeywordActions_PositiveKeywordSucceeds(t *testing.T) {
	mutated := false
	c := keywordCriterionServer(t,
		criterionRowJSON("176216228", "305729261", false),
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`,
		&mutated)

	out, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	})
	if err != nil {
		t.Fatalf("a positive keyword must be actionable, got: %v", err)
	}
	if len(out) != 1 || out[0].CriterionID != "305729261" {
		t.Fatalf("outcomes = %+v, want the one criterion acted on", out)
	}
	if !mutated {
		t.Error("the mutate was never issued for a valid positive keyword")
	}
}

// A NEGATIVE keyword must be REFUSED. Pausing or removing an exclusion WIDENS delivery and
// spend — the opposite of what this endpoint guarantees — and REMOVE cannot be undone.
//
// The assertion is on the sentinel and on the mutate NEVER being issued, not on the message:
// a test matching error text would pass against a version that refused for some other reason,
// and one asserting only the ad-group id would pass against the bug entirely.
func TestApplyKeywordActions_NegativeKeywordIsRefused(t *testing.T) {
	mutated := false
	c := keywordCriterionServer(t,
		criterionRowJSON("176216228", "305729261", true),
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`,
		&mutated)

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("removing a NEGATIVE keyword was allowed; that widens delivery and spend")
	}
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Errorf("error is not ErrKeywordCriterionNotPositiveKeyword: %v", err)
	}
	if mutated {
		t.Error("the mutate was issued for a negative keyword; the refusal must happen BEFORE any mutation")
	}
}

// A userList (audience) criterion lives in the SAME ad group and the SAME adGroupCriteria
// resource family as a keyword, so the ad-group check alone admits it. keyword_view does not
// return it, so it resolves to nothing and must fail closed.
func TestApplyKeywordActions_UserListCriterionIsRefused(t *testing.T) {
	mutated := false
	// The type-resolution query returns NO row for this criterion — which is exactly what
	// keyword_view does for a userList criterion, since that view holds keywords only.
	c := keywordCriterionServer(t, "",
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~444444444"}`,
		&mutated)

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "444444444", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("removing a userList/audience criterion was allowed through the keyword endpoint")
	}
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Errorf("error is not ErrKeywordCriterionNotPositiveKeyword: %v", err)
	}
	if mutated {
		t.Error("the mutate was issued for a non-keyword criterion")
	}
}

// An UNRESOLVABLE criterion fails CLOSED. This is the documented choice: admitting one risks
// an irreversible REMOVE of an exclusion, while refusing costs the caller a re-read.
func TestApplyKeywordActions_UnresolvableCriterionFailsClosed(t *testing.T) {
	mutated := false
	// One of the two resolves; the other does not. The whole batch must refuse — it is atomic.
	c := keywordCriterionServer(t,
		criterionRowJSON("176216228", "305729261", false),
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"},`+
			`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~999999999"}`,
		&mutated)

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
		{AdGroupID: "176216228", CriterionID: "999999999", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("a batch with an unresolvable criterion was allowed; it must fail closed")
	}
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Errorf("error is not ErrKeywordCriterionNotPositiveKeyword: %v", err)
	}
	if mutated {
		t.Error("the mutate was issued despite an unresolvable criterion")
	}
}

// A row that OMITS `negative` is treated as negative — unknown polarity is refused, not
// assumed benign.
func TestApplyKeywordActions_OmittedNegativeFieldFailsClosed(t *testing.T) {
	mutated := false
	c := keywordCriterionServer(t,
		`{"adGroupCriterion":{"criterionId":"305729261"},"adGroup":{"id":"176216228"}}`,
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`,
		&mutated)

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("a criterion with unknown polarity was allowed; it must fail closed")
	}
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Errorf("error is not ErrKeywordCriterionNotPositiveKeyword: %v", err)
	}
	if mutated {
		t.Error("the mutate was issued for a criterion of unknown polarity")
	}
}

// The type-resolution query must ask keyword_view — the SAME type-scoped resource the read
// path uses. That is what makes the guard's type claim true rather than asserted; a query
// against a broader resource would resolve non-keyword criteria as if they were keywords.
func TestApplyKeywordActions_ResolvesTypeViaKeywordView(t *testing.T) {
	var mu sync.Mutex
	var searchBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "googleAds:search") {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			searchBody = string(b)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[`+criterionRowJSON("176216228", "305729261", false)+`]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}]}`)
	})

	if _, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	}); err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}

	mu.Lock()
	got := searchBody
	mu.Unlock()
	if got == "" {
		t.Fatal("no type-resolution query was issued before the mutate")
	}
	if !strings.Contains(got, "FROM keyword_view") {
		t.Errorf("type resolution does not query keyword_view, the type-scoped resource: %s", got)
	}
	if !strings.Contains(got, "ad_group_criterion.negative") {
		t.Errorf("type resolution does not select ad_group_criterion.negative, so it cannot tell a negative keyword from a positive one: %s", got)
	}
}

// UNCONFIRMED outcomes must be detectable STRUCTURALLY — via IsOutcomeUnconfirmed /
// errors.As on Unconfirmed() — not by their message text. The service classifies ambiguity
// exclusively through that interface, so an arm that only says "UNCONFIRMED" in prose is
// answered as a DEFINITE failure and the caller is told to retry a batch Google may have run.
func TestApplyKeywordActions_UnconfirmedArmsAreStructurallyDetectable(t *testing.T) {
	positive := criterionRowJSON("176216228", "305729261", false) + "," +
		criterionRowJSON("176216228", "999999999", false)

	cases := []struct {
		name    string
		mutate  string
		actions []KeywordAction
	}{
		{
			// 2xx whose result count is SHORT of the operation count.
			name:   "short mutate response",
			mutate: `{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`,
			actions: []KeywordAction{
				{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
				{AdGroupID: "176216228", CriterionID: "999999999", Action: KeywordActionRemove},
			},
		},
		{
			// 2xx naming a criterion the batch never addressed.
			name:   "mismatched resource name",
			mutate: `{"resourceName":"customers/1234567890/adGroupCriteria/176216228~888888888"}`,
			actions: []KeywordAction{
				{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := keywordCriterionServer(t, positive, tc.mutate, nil)
			_, err := c.ApplyKeywordActions(context.Background(), tc.actions)
			if err == nil {
				t.Fatal("expected an UNCONFIRMED error, got nil")
			}
			// The load-bearing assertion: detected by BEHAVIOUR, the way the service detects it.
			if !IsOutcomeUnconfirmed(err) {
				t.Errorf("IsOutcomeUnconfirmed = false; the service will report this ambiguous outcome as a DEFINITE failure and invite a retry: %v", err)
			}
			var u interface{ Unconfirmed() bool }
			if !errors.As(err, &u) || !u.Unconfirmed() {
				t.Errorf("errors.As found no Unconfirmed() marker; classifyKeywordActionError matches on exactly this: %v", err)
			}
		})
	}
}
