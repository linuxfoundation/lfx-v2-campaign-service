// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// keywordRowJSON builds one POSITIVE keyword_view result row.
//
// It OMITS `negative` entirely, because that is what Google actually sends: `negative` is a
// proto bool and protobuf JSON does not serialise a field at its default value, so
// `negative: false` never appears on the wire for an ordinary positive keyword. Rendering it
// explicitly would produce a body no conformant serialiser emits, and every positive-path test
// built on it would agree with the decoder instead of checking it. See criterionRowJSON, which
// makes the same choice for the type-resolution query.
func keywordRowJSON(criterionID, adGroupID, campaignID, text, matchType, status string, impressions, clicks, cost int) string {
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"status":%q,"keyword":{"text":%q,"matchType":%q}},`+
		`"adGroup":{"id":%q},"campaign":{"id":%q},`+
		`"metrics":{"impressions":"%d","clicks":"%d","costMicros":"%d"}}`,
		criterionID, status, text, matchType, adGroupID, campaignID, impressions, clicks, cost)
}

// negativeKeywordRowJSON builds one NEGATIVE (exclusion) keyword_view row, carrying the
// explicit `negative: true` Google sends for an exclusion. `keyword_view` returns both
// polarities, so this is a row the read must not publish.
func negativeKeywordRowJSON(criterionID, adGroupID, campaignID, text, matchType, status string, impressions, clicks, cost int) string {
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"status":%q,"negative":true,"keyword":{"text":%q,"matchType":%q}},`+
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
			keywordRowJSON("305729261", "176216228", "555", "kubernetes training", "EXACT", "ENABLED", 1000, 40, 25000000)+
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
	if got.CriterionID != "305729261" || got.AdGroupID != "176216228" || got.CampaignID != "555" {
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
// cannot tell a project with few keywords from a truncated large one and would total the
// slice as the project's whole spend. (Not the ACCOUNT's — the read is confined to the
// project's own campaigns by campaignScopePredicate, so neither figure covers the shared
// customer.)
func TestGetKeywordPerformance_TruncatesAndReportsIt(t *testing.T) {
	rows := make([]string, 0, maxKeywordRows+1)
	for i := 0; i < maxKeywordRows+1; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "555", "kw", "BROAD", "ENABLED", 10, 1, 100))
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
		t.Fatalf("Truncated = false, but the scoped campaigns returned more than the cap")
	}
}

// Exactly the cap, with no probe row, is NOT truncated. Guards the off-by-one in the other
// direction: reporting truncation for a complete answer tells a caller their totals are a
// partial slice when they cover every keyword on the project's own campaigns — which is the
// whole of what this read is scoped to, and is never the whole shared account.
func TestGetKeywordPerformance_ExactlyCapIsNotTruncated(t *testing.T) {
	rows := make([]string, 0, maxKeywordRows)
	for i := 0; i < maxKeywordRows; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "555", "kw", "BROAD", "ENABLED", 10, 1, 100))
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

// campaign_id is Required on the google-ads-keyword design type and is selected by the same
// GAQL query as the other two ids, so an absent campaign.id is the SAME SELECT/decoder drift
// the guard above already fails closed on. Letting it through emits campaign_id:"" — a row a
// caller cannot associate with any campaign, on a read whose entire scope is "this project's
// OWN campaigns". The assertion is that the call ERRORS; it deliberately does not match the
// message text, which would move with the wording.
func TestGetKeywordPerformance_MissingCampaignIDIsAnError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Both ids the old guard checks ARE present; only campaign.id is absent.
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"305729261","keyword":{"text":"x","matchType":"EXACT"}},`+
			`"adGroup":{"id":"176216228"},"campaign":{},"metrics":{"impressions":"5","clicks":"1","costMicros":"10"}}]}`)
	})
	if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
		t.Fatal("a keyword row with no campaign id must not be returned as campaign_id:\"\", which is unusable to the caller and violates the Required() on the design type")
	}
}

// A campaign id present but WHITESPACE-ONLY is the same defect: TrimSpace is what the sibling
// guards use, so " " must not survive as a usable id either.
func TestGetKeywordPerformance_WhitespaceCampaignIDIsAnError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"305729261","keyword":{"text":"x","matchType":"EXACT"}},`+
			`"adGroup":{"id":"176216228"},"campaign":{"id":"   "},"metrics":{"impressions":"5","clicks":"1","costMicros":"10"}}]}`)
	})
	if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
		t.Fatal("a whitespace-only campaign id must be rejected exactly as the criterion and ad group ids are")
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
			`"adGroup":{"id":"2"},"campaign":{"id":"555"},"metrics":{}}]}`)
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
			"adGroup": map[string]any{"id": "2"},
			// IN SCOPE, deliberately. The metrics parse now runs after the scope check, so a
			// fixture naming an out-of-scope campaign would fail on the scope error and prove
			// nothing about how a parse failure is reported. It passed that way only while the
			// parse happened to run first.
			"campaign": map[string]any{"id": "555"},
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
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_25_34"}},"campaign":{"id":"555"},"metrics":{"impressions":"100","clicks":"10","costMicros":"500"}}]}`)
		case 2: // gender
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"gender":{"type":"MALE"}},"campaign":{"id":"555"},"metrics":{"impressions":"60","clicks":"6","costMicros":"300"}}]}`)
		default: // device
			_, _ = io.WriteString(w, `{"results":[{"segments":{"device":"MOBILE"},"campaign":{"id":"555"},"metrics":{"impressions":"80","clicks":"8","costMicros":"400"}}]}`)
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
// numbers as the whole scope's — every campaign the project owns, which is what this
// read covers (not the shared account).
func TestGetAudienceInsights_AggregatesRepeatedBucketsAcrossCampaigns(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "FROM campaign") {
			_, _ = io.WriteString(w, `{"results":[`+
				`{"segments":{"device":"MOBILE"},"campaign":{"id":"555"},"metrics":{"impressions":"100","clicks":"10","costMicros":"1000"}},`+
				`{"segments":{"device":"MOBILE"},"campaign":{"id":"555"},"metrics":{"impressions":"300","clicks":"30","costMicros":"3000"}}]}`)
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
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_UNDETERMINED"}},"campaign":{"id":"555"},"metrics":{"impressions":"900","clicks":"5","costMicros":"50"}}]}`)
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

// The GAQL predicate is the filter REQUESTED; these two tests pin the filter ENFORCED.
//
// Google Ads is ONE customer shared across every foundation, so a response carrying a campaign
// the caller did not ask for is another project's data. Presence of campaign.id is NOT membership
// in the requested set: the guard these replace admitted any non-empty id, so a response that did
// not honour the WHERE clause was returned to the caller as a successful read.
//
// Both assert on the ERROR rather than on a filtered-down row set: an unhonoured filter
// invalidates the whole response, and silently dropping the foreign rows would report another
// project's read as "this project has little data".
func TestGetKeywordPerformance_RejectsCampaignOutsideRequestedScope(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Asked for 555; the response names 999 — a campaign belonging to another project.
		_, _ = io.WriteString(w, `{"results":[`+
			keywordRowJSON("305729261", "176216228", "999", "rival keyword", "EXACT", "ENABLED", 1000, 40, 25000000)+
			`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err == nil {
		t.Fatalf("a row for campaign 999 was accepted on a read scoped to [555]; rows = %+v", kp.Rows)
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "outside the requested campaign scope") {
		t.Errorf("error does not name the out-of-scope campaign: %v", err)
	}
}

// A non-canonical spelling of an in-scope id must not pass by string equality, and an id that
// names no campaign at all must not pass either. "0555" is the dangerous one: it is campaign 555
// to the server and a different string here, so admitting it would make the boundary depend on
// how the upstream chose to spell the number.
func TestGetKeywordPerformance_RejectsNonCanonicalCampaignID(t *testing.T) {
	for _, bad := range []string{"", "0555", "abc", "0"} {
		t.Run(bad, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[`+
					keywordRowJSON("305729261", "176216228", bad, "kw", "EXACT", "ENABLED", 10, 1, 100)+
					`]}`)
			})
			if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
				t.Fatalf("campaign id %q was accepted on a read scoped to [555]", bad)
			}
		})
	}
}

// The sibling read on the same boundary. Its buckets never REPORT a campaign id, which is
// exactly why the leak is quiet here: a foreign campaign's impressions and spend are summed
// into a bucket and become indistinguishable from the project's own. campaign.id is selected
// solely so this check is possible.
func TestGetAudienceInsights_RejectsCampaignOutsideRequestedScope(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "FROM age_range_view") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_25_34"}},`+
				`"campaign":{"id":"999"},"metrics":{"impressions":"5000","clicks":"250","costMicros":"77000000"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	ai, err := c.GetAudienceInsights(context.Background(), WindowLast7Days, []string{"555"})
	if err == nil {
		t.Fatalf("a bucket from campaign 999 was accepted on a read scoped to [555]; buckets = %+v", ai.Buckets)
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "outside the requested campaign scope") {
		t.Errorf("error does not name the out-of-scope campaign: %v", err)
	}
}

// The membership check can only run if the query ASKS for campaign.id. The stub server answers
// with whatever fixture the test wrote regardless of the SELECT, so nothing else in this file
// would notice the field being dropped from the query — against the real API every audience read
// would then fail as "missing field campaign", or, if the guard were also relaxed, silently stop
// enforcing scope. This pins the SELECT itself for every dimension.
func TestGetAudienceInsights_QuerySelectsCampaignIDForEveryDimension(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		queries = append(queries, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	if _, err := c.GetAudienceInsights(context.Background(), WindowLast7Days, []string{"555"}); err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != len(audienceQueries) {
		t.Fatalf("sent %d queries, want one per dimension (%d)", len(queries), len(audienceQueries))
	}
	// Asserted against the SELECT list specifically, NOT the whole query: the scope predicate
	// puts the literal "campaign.id" in the WHERE clause of every one of these queries, so a
	// substring check over the full text passes even when the projection omits the field
	// entirely — it would be matching the filter it is not testing.
	for i, q := range queries {
		sel := q[strings.Index(q, "SELECT"):strings.Index(q, " FROM ")]
		if !strings.Contains(sel, "campaign.id") {
			t.Errorf("query %d (%s) does not SELECT campaign.id, so the scope check cannot run; select list = %q",
				i, audienceQueries[i].dimension, sel)
		}
		// metrics.conversions for the same reason and with the same blind spot: the audience
		// fixtures return a conversions value regardless of what was asked for, so the
		// aggregation tests stay green if this field is dropped from the projection — every
		// bucket would simply report a measured zero. Only asserting the SELECT can catch it.
		if !strings.Contains(sel, "metrics.conversions") {
			t.Errorf("query %d (%s) does not SELECT metrics.conversions, so every bucket would report zero; select list = %q",
				i, audienceQueries[i].dimension, sel)
		}
	}
}

// A conversions value that is not a usable count fails the WHOLE response rather than being
// folded into a total.
//
// NaN and ±Inf survive JSON decoding of a bare number in some encoders, and a negative count is
// upstream corruption rather than a small number. Unchecked, each reaches a keyword row as a
// rendered measurement — and on the audience path it is SUMMED into a bucket, where one bad row
// corrupts every figure in that bucket rather than just its own.
//
// This mirrors the guard GetCampaignMetrics already applies; the shared helper had bypassed it.
func TestParseRowMetrics_RejectsUnusableConversionCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"negative", `{"results":[{"adGroupCriterion":{"criterionId":"1","status":"ENABLED","keyword":{"text":"x","matchType":"EXACT"}},` +
			`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},` +
			`"metrics":{"impressions":"10","clicks":"1","costMicros":"100","conversions":-5}}]}`},
		{"beyond int64", `{"results":[{"adGroupCriterion":{"criterionId":"1","status":"ENABLED","keyword":{"text":"x","matchType":"EXACT"}},` +
			`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},` +
			`"metrics":{"impressions":"10","clicks":"1","costMicros":"100","conversions":1e19}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})
			// The whole call fails — asserted rather than checking the row was dropped, because
			// a silently dropped row is its own defect: the caller would total a set missing a
			// keyword and never know.
			if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
				t.Fatal("an unusable conversion count was accepted")
			}
		})
	}
}

// The NON-FINITE arms, driven directly rather than over HTTP.
//
// They cannot be reached through the transport: `encoding/json` rejects the bare literals `NaN`,
// `Infinity` and `-Infinity` before any handler runs, so an HTTP-level case would test the
// DECODER and stay green with the guard deleted — which is exactly what an earlier version of
// this test did. A non-finite value can still arrive from a different decoder or a future direct
// caller, so the guard is real; calling the helper directly is the only way to bind it.
func TestParseRowMetrics_RejectsNonFiniteConversions(t *testing.T) {
	for _, tc := range []struct {
		name string
		conv float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := gaqlMetricRowMetrics{Impressions: "10", Clicks: "1", CostMicros: "100", Conversions: tc.conv}
			if _, _, _, _, err := parseRowMetrics(m, "test"); err == nil {
				t.Fatalf("a %s conversion count was accepted", tc.name)
			}
		})
	}
}

// A fractional count is NOT rejected. Google credits fractional conversions under data-driven
// and position-based attribution, so 0.5 is an ordinary measurement — a guard that treated
// "not a whole number" as corruption would refuse the common case.
func TestParseRowMetrics_AcceptsFractionalConversions(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"1","status":"ENABLED","keyword":{"text":"x","matchType":"EXACT"}},`+
			`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},`+
			`"metrics":{"impressions":"10","clicks":"1","costMicros":"100","conversions":0.5}}]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("a fractional conversion count was rejected: %v", err)
	}
	if got := kp.Rows[0].Conversions; got != 0.5 {
		t.Errorf("Conversions = %v, want 0.5", got)
	}
}

// The membership check must be reachable for every dimension, not only the first one queried.
// Device is the one that segments the `campaign` resource rather than a criterion view, so a
// check written against the *_view shape only would leave it behind.
func TestGetAudienceInsights_ScopeEnforcedOnEveryDimension(t *testing.T) {
	for _, from := range []string{"age_range_view", "gender_view", "FROM campaign "} {
		t.Run(from, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				if !strings.Contains(string(b), from) {
					_, _ = io.WriteString(w, `{"results":[]}`)
					return
				}
				switch {
				case strings.Contains(from, "age_range"):
					_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_25_34"}},`+
						`"campaign":{"id":"999"},"metrics":{"impressions":"10","clicks":"1","costMicros":"5"}}]}`)
				case strings.Contains(from, "gender"):
					_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"gender":{"type":"MALE"}},`+
						`"campaign":{"id":"999"},"metrics":{"impressions":"10","clicks":"1","costMicros":"5"}}]}`)
				default:
					_, _ = io.WriteString(w, `{"results":[{"segments":{"device":"MOBILE"},`+
						`"campaign":{"id":"999"},"metrics":{"impressions":"10","clicks":"1","costMicros":"5"}}]}`)
				}
			})
			if _, err := c.GetAudienceInsights(context.Background(), WindowLast7Days, []string{"555"}); err == nil {
				t.Fatalf("campaign 999 was accepted from %s on a read scoped to [555]", from)
			}
		})
	}
}

// The scope SET and the predicate STRING must be built from one pass, so the filter sent and the
// membership enforced cannot drift. A set holding a spelling the predicate did not send (or vice
// versa) would enforce a different boundary than the one requested.
func TestCampaignScopePredicate_SetMatchesPredicate(t *testing.T) {
	pred, scope, err := campaignScopePredicate([]string{"111", "222"}, "op")
	if err != nil {
		t.Fatalf("campaignScopePredicate: %v", err)
	}
	if len(scope) != 2 {
		t.Fatalf("scope set = %v, want 2 entries", scope)
	}
	for _, id := range []string{"111", "222"} {
		if _, ok := scope[id]; !ok {
			t.Errorf("scope set is missing %s, but the predicate sent it: %s", id, pred)
		}
		if !strings.Contains(pred, id) {
			t.Errorf("predicate %q does not carry %s, but the scope set enforces it", pred, id)
		}
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

// TestValidateKeywordActions_RejectsOverLongIDs pins the runtime half of the design's
// MaxLength(20) on both ids. Goa enforces that bound for HTTP callers; a DIRECT service
// caller never passes through the generated decoder, so without this check the two entry
// points disagreed about what a valid request is.
//
// Digits-only alone bounds the CHARACTER CLASS, not the VALUE, and neither does a digit
// count. Google Ads ids are positive int64s, and math.MaxInt64 has nineteen digits, so a
// twenty-digit id — and "9999999999999999999", which is nineteen — is injection-safe and
// still incapable of naming a real criterion. Left unchecked it reached the type-resolution
// GAQL request, where Google's PERMANENT rejection was classified onto the retryable 503 path
// — telling the caller to retry a request that can never succeed.
//
// The boundary is asserted on BOTH sides: math.MaxInt64 itself must still be ACCEPTED, or the
// check would be refusing ids that name real criteria. The rejected cases are chosen so a
// length-only check cannot pass this test: "0" and "0305729261" are both well within any
// twenty-digit cap.
// designKeywordIDMaxLength reads the MaxLength the design declares on keyword-action-input's
// two id attributes, so the client's maxKeywordIDLen can be pinned to it mechanically rather
// than by a comment that a later edit to design/brief.go would silently falsify. Both
// attributes must agree; an id is an id whichever field carries it.
func designKeywordIDMaxLength(t *testing.T) int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "design", "brief.go"))
	if err != nil {
		t.Fatalf("read design/brief.go: %v", err)
	}
	block := regexp.MustCompile(`(?s)var KeywordActionInput = Type\(.*?\n\}\)`).Find(src)
	if block == nil {
		t.Fatal("could not locate KeywordActionInput in design/brief.go; this test pins the " +
			"design's cap to the client's and cannot do so if the type was renamed")
	}
	found := regexp.MustCompile(`MaxLength\((\d+)\)`).FindAllSubmatch(block, -1)
	if len(found) != 2 {
		t.Fatalf("expected a MaxLength on both keyword-action id attributes, found %d", len(found))
	}
	first, err := strconv.Atoi(string(found[0][1]))
	if err != nil {
		t.Fatalf("parse MaxLength: %v", err)
	}
	second, err := strconv.Atoi(string(found[1][1]))
	if err != nil {
		t.Fatalf("parse MaxLength: %v", err)
	}
	if first != second {
		t.Fatalf("ad_group_id and criterion_id declare different MaxLengths (%d and %d); an id "+
			"is an id whichever field carries it", first, second)
	}
	return first
}

func TestValidateKeywordActions_RejectsIDsThatCannotNameACriterion(t *testing.T) {
	maxInt64 := strconv.FormatInt(math.MaxInt64, 10) // 19 digits, the largest valid id
	overflow19 := "9999999999999999999"              // 19 digits, but > math.MaxInt64
	twentyDigits := "99999999999999999999"           // 20 digits: within the OLD cap of 20

	if len(maxInt64) != maxKeywordIDLen {
		t.Fatalf("math.MaxInt64 has %d digits but maxKeywordIDLen is %d — the design's cap no "+
			"longer matches the widest id Google can issue", len(maxInt64), maxKeywordIDLen)
	}
	// maxKeywordIDLen is the client's copy of design/brief.go's MaxLength on
	// keyword-action-input. Nothing in the source reads it, so if the two drift apart only this
	// assertion notices: a design raised back to 20 would let Goa's decoder admit, for HTTP
	// callers, exactly the ids the cases below prove cannot name a criterion.
	if got := designKeywordIDMaxLength(t); got != maxKeywordIDLen {
		t.Fatalf("design/brief.go declares MaxLength(%d) on the keyword-action ids but "+
			"maxKeywordIDLen is %d — the design and the client disagree about what a valid "+
			"request is, and the generated decoder follows the design", got, maxKeywordIDLen)
	}

	for _, tc := range []struct {
		name    string
		action  KeywordAction
		wantErr bool
	}{
		{"criterion of 20 digits", KeywordAction{AdGroupID: "176216228", CriterionID: twentyDigits, Action: "PAUSE"}, true},
		{"ad group of 20 digits", KeywordAction{AdGroupID: twentyDigits, CriterionID: "305729261", Action: "PAUSE"}, true},
		{"criterion overflowing int64 at 19 digits", KeywordAction{AdGroupID: "176216228", CriterionID: overflow19, Action: "PAUSE"}, true},
		{"criterion of zero", KeywordAction{AdGroupID: "176216228", CriterionID: "0", Action: "PAUSE"}, true},
		{"ad group of zero", KeywordAction{AdGroupID: "0", CriterionID: "305729261", Action: "PAUSE"}, true},
		{"criterion in a leading-zero spelling", KeywordAction{AdGroupID: "176216228", CriterionID: "0305729261", Action: "PAUSE"}, true},
		{"both at math.MaxInt64", KeywordAction{AdGroupID: maxInt64, CriterionID: maxInt64, Action: "PAUSE"}, false},
		{"an ordinary pair", KeywordAction{AdGroupID: "176216228", CriterionID: "305729261", Action: "PAUSE"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateKeywordActions([]KeywordAction{tc.action})
			if tc.wantErr && err == nil {
				t.Fatalf("expected a local rejection for an id that cannot name a positive int64 " +
					"criterion; admitting it sends a GAQL request whose permanent rejection is " +
					"mapped onto the retryable 503 path")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("an id that names a real criterion must be accepted, got: %v", err)
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

// The ACTION is still normalised (trimmed and upper-cased): it is a closed enum this package
// defines, so " pause " and "PAUSE" name the same member and there is no other campaign a
// mis-normalisation could reach. The IDS are deliberately NOT normalised — they are opaque
// upstream handles whose padding the Goa pattern already rejects, and trimming them would let
// a non-HTTP caller in past that contract. So this case passes canonical ids and asserts they
// survive unchanged alongside the action that does get rewritten.
func TestValidateKeywordActions_NormalisesAction(t *testing.T) {
	out, err := ValidateKeywordActions([]KeywordAction{{AdGroupID: "1", CriterionID: "2", Action: " pause "}})
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

// This read returns keywords this service never created — a campaign ADOPTED into the
// project, or one of its own campaigns edited in the Google UI — so an upstream value
// outside the API contract's closed enums is reachable no matter what the create path
// restricts itself to. (The premise used to be stated as "an ACCOUNT-WIDE read". The read
// is campaign-scoped, so that reason is false; the conclusion survives on the two above,
// which is why the test stands unchanged.) The design declares Enum(...) on the RESPONSE and Goa emits that
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
		if _, _, err := campaignScopePredicate(ids, "op"); err == nil {
			t.Errorf("campaignScopePredicate(%v) returned no error; an empty scope must never "+
				"produce a query, because an unscoped read returns every project's data", ids)
		}
	}
}

// The ids are concatenated into the GAQL string and GAQL has no parameterized queries, so a
// non-numeric id is an injection vector. Same digits-only rule GetCampaignMetrics applies.
func TestCampaignScopePredicate_RejectsNonNumericIDs(t *testing.T) {
	for _, bad := range []string{"1' OR '1'='1", "abc", "12 34", "", "-1"} {
		if _, _, err := campaignScopePredicate([]string{"111", bad}, "op"); err == nil {
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
	// Both ids are canonical here. This case previously passed " 111 " to pin the old
	// TrimSpace behaviour; that padding is now REJECTED (see
	// TestCampaignScopePredicate_RejectsIDsThatCannotNameACampaign), because normalising a
	// malformed id inside the tenant-boundary predicate can substitute a different real
	// campaign. This test asserts rendering, so it uses ids that are valid on both sides.
	got, _, err := campaignScopePredicate([]string{"111", "222"}, "op")
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

// criterionRowJSON builds one keyword_view row for the type-resolution query.
//
// A POSITIVE row OMITS `negative` entirely, because that is what Google actually sends:
// protobuf JSON does not serialise a field at its default value, so `negative: false` never
// appears on the wire. Rendering it explicitly — as this helper used to — produces a body no
// real response has, and a positive-path test built on it agrees with the decoder instead of
// checking it. A NEGATIVE row carries the explicit `negative: true`, which IS what Google
// sends for an exclusion.
func criterionRowJSON(adGroupID, criterionID string, negative bool) string {
	if negative {
		return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"negative":true},"adGroup":{"id":%q}}`,
			criterionID, adGroupID)
	}
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q},"adGroup":{"id":%q}}`,
		criterionID, adGroupID)
}

// criterionRowJSONExplicitNegativeFalse renders `negative: false` explicitly. A conformant
// protobuf-JSON serialiser never emits this, but a proxy or a future non-default serialiser
// may, and it must decode to the same POSITIVE verdict as the omitted form.
func criterionRowJSONExplicitNegativeFalse(adGroupID, criterionID string) string {
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"negative":false},"adGroup":{"id":%q}}`,
		criterionID, adGroupID)
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

// A row that OMITS `negative` is POSITIVE and MUST be actionable. This is the realistic wire
// shape, not an edge case: protobuf JSON omits a field at its default value, so an ordinary
// positive keyword — the very row `GetKeywordPerformance` just handed the caller — arrives
// with no `negative` key at all.
//
// Treating that omission as "unknown polarity" refuses every ordinary keyword and makes the
// endpoint unusable, and re-reading cannot repair it because the second read omits the field
// identically. The body here is written inline rather than through the helper so the shape
// under test is visible at the assertion, and so a future change to the helper cannot quietly
// reintroduce an explicit `negative: false` and make this test agree with a broken decoder.
func TestApplyKeywordActions_OmittedNegativeFieldIsPositiveAndActionable(t *testing.T) {
	mutated := false
	c := keywordCriterionServer(t,
		`{"adGroupCriterion":{"criterionId":"305729261"},"adGroup":{"id":"176216228"}}`,
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`,
		&mutated)

	out, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	})
	if err != nil {
		t.Fatalf("a positive keyword whose `negative` field is OMITTED — the shape Google actually sends — was refused, which breaks the endpoint's normal path: %v", err)
	}
	if len(out) != 1 || out[0].CriterionID != "305729261" {
		t.Fatalf("outcomes = %+v, want the one criterion acted on", out)
	}
	if !mutated {
		t.Error("the mutate was never issued for a positive keyword sent in the realistic omitted-field form")
	}
}

// An EXPLICIT `negative: false` must reach the same POSITIVE verdict as the omitted form.
// Conformant protobuf JSON never emits it, but the two spellings mean one thing and the
// decoder must not distinguish them.
func TestApplyKeywordActions_ExplicitNegativeFalseIsPositive(t *testing.T) {
	mutated := false
	c := keywordCriterionServer(t,
		criterionRowJSONExplicitNegativeFalse("176216228", "305729261"),
		`{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`,
		&mutated)

	if _, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionPause},
	}); err != nil {
		t.Fatalf("an explicit `negative: false` must be actionable, got: %v", err)
	}
	if !mutated {
		t.Error("the mutate was never issued for an explicitly-positive keyword")
	}
}

// A MISSING FIELD and a MISSING ROW are different facts, and conflating them is what broke the
// happy path. This pins BOTH verdicts in one test so a future change cannot collapse them
// again: the same batch shape that is ADMITTED when the row exists with `negative` omitted is
// REFUSED when `keyword_view` returns no row for the id at all — a userList criterion, the
// case the guard exists to catch.
func TestApplyKeywordActions_MissingFieldAdmittedMissingRowRefused(t *testing.T) {
	omittedFieldRow := `{"adGroupCriterion":{"criterionId":"305729261"},"adGroup":{"id":"176216228"}}`
	mutateResult := `{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}`
	action := []KeywordAction{{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove}}

	admitted := false
	if _, err := keywordCriterionServer(t, omittedFieldRow, mutateResult, &admitted).
		ApplyKeywordActions(context.Background(), action); err != nil {
		t.Fatalf("missing FIELD must be admitted as positive: %v", err)
	}
	if !admitted {
		t.Error("the mutate was not issued for a row whose `negative` field is merely absent")
	}

	refused := false
	_, err := keywordCriterionServer(t, "", mutateResult, &refused).
		ApplyKeywordActions(context.Background(), action)
	if err == nil {
		t.Fatal("missing ROW must FAIL CLOSED — that is a userList/absent criterion, not a positive keyword")
	}
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Errorf("error is not ErrKeywordCriterionNotPositiveKeyword: %v", err)
	}
	if refused {
		t.Error("the mutate was issued for a criterion keyword_view returned no row for")
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
	// The status predicate must be an ALLOW-LIST, exactly as the read path's is. Asserting
	// only that REMOVED is absent from the query text would pass against a predicate-free
	// query, which is the very defect this pins — the same trap the settings readback's
	// REMOVED test fell into.
	for _, want := range []string{StatusEnabled, StatusPaused} {
		if !strings.Contains(got, "'"+want+"'") {
			t.Errorf("type resolution must name %s in an ad_group_criterion.status allow-list; "+
				"without one keyword_view returns REMOVED criteria, whose mutation Google "+
				"rejects PERMANENTLY and which this path then reports as a retryable 503: %s", want, got)
		}
	}
	if !strings.Contains(got, "ad_group_criterion.status") {
		t.Errorf("type resolution carries no ad_group_criterion.status predicate at all: %s", got)
	}
}

// TestApplyKeywordActions_RemovedCriterionFailsLocally is the behavioural half of the query
// assertion above, and the one that states the CONSEQUENCE rather than the query text.
//
// A criterion already REMOVED upstream is a permanently stale handle: Google rejects a pause
// or removal of it as unmutable, however many times it is retried. With no status predicate,
// keyword_view returned the removed row, the guard admitted it as a positive keyword, the
// mutate was sent, and that permanent rejection came back through the transport-error path as
// a retryable upstream 503. Excluded by the allow-list, the row is simply absent, so the
// existing fail-closed `!ok` arm answers with the permanent sentinel BEFORE anything is
// mutated — and the mutate endpoint is never called at all.
func TestApplyKeywordActions_RemovedCriterionFailsLocally(t *testing.T) {
	var mu sync.Mutex
	mutateCalled := false
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "googleAds:search") {
			// The server honours the allow-list the query asks for: a REMOVED criterion is
			// filtered out server-side, so the response carries no row for it. This is what
			// Google does with the predicate present, and what it does NOT do without it.
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		mu.Lock()
		mutateCalled = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/176216228~305729261"}]}`)
	})

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove},
	})
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Fatalf("err = %v, want ErrKeywordCriterionNotPositiveKeyword: a removed criterion is a "+
			"permanently stale handle and must fail LOCALLY as an invalid action, not as a "+
			"retryable upstream failure", err)
	}
	mu.Lock()
	called := mutateCalled
	mu.Unlock()
	if called {
		t.Error("a removed criterion must be refused BEFORE the mutate is issued; sending it " +
			"turns a permanent rejection into the retryable 503 path")
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

// campaignScopePredicate builds the TENANT BOUNDARY for the two account-wide keyword reads,
// so what it accepts decides which project's rows come back. It normalised each id with
// strings.TrimSpace BEFORE the digits-only class check, which admitted three shapes that
// cannot name a campaign: whitespace padding, a leading-zero spelling, and a digit string
// past math.MaxInt64. canonicalCampaignID's own doc names the reason trimming is wrong here —
// it converts "this value is malformed" into "this value is campaign 123" — and that
// substitution is made against the predicate that keeps one project from reading another's.
func TestCampaignScopePredicate_RejectsIDsThatCannotNameACampaign(t *testing.T) {
	for _, bad := range []string{" 123 ", "123 ", " 123", "000123", "0", "99999999999999999999", "9999999999999999999"} {
		if _, _, err := campaignScopePredicate([]string{"111", bad}, "op"); err == nil {
			t.Errorf("campaignScopePredicate accepted %q for the tenant-boundary predicate; "+
				"an id that cannot name a positive int64 campaign must fail closed rather than "+
				"be normalised into a different real campaign", bad)
		}
	}
	// The canonical spellings must still pass, or the fix has closed the endpoint entirely.
	if _, _, err := campaignScopePredicate([]string{"111", strconv.FormatInt(math.MaxInt64, 10)}, "op"); err != nil {
		t.Errorf("campaignScopePredicate rejected canonical ids: %v", err)
	}
}

// The Goa contract pins Pattern(`^[0-9]+$`) on ad_group_id and criterion_id (design/brief.go),
// so an HTTP caller sending " 333 " is rejected by the generated decoder before any handler
// runs. ValidateKeywordActions trimmed first and validated second, so the SAME value was
// accepted from a non-HTTP caller and silently rewritten to "333" — the published input
// contract and the runtime backstop disagreeing about what a valid request is.
func TestValidateKeywordActions_RejectsWhitespacePaddedIDs(t *testing.T) {
	for _, tc := range []struct{ name, adGroup, criterion string }{
		{"padded criterion", "176216228", " 305729261 "},
		{"padded ad group", " 176216228 ", "305729261"},
		{"trailing newline on criterion", "176216228", "305729261\n"},
		{"tab-padded ad group", "\t176216228", "305729261"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateKeywordActions([]KeywordAction{
				{AdGroupID: tc.adGroup, CriterionID: tc.criterion, Action: "PAUSE"},
			}); err == nil {
				t.Fatalf("ValidateKeywordActions accepted adGroup=%q criterion=%q; the Goa "+
					"pattern ^[0-9]+$ rejects these, so trimming lets a non-HTTP caller bypass "+
					"the published ID validation", tc.adGroup, tc.criterion)
			}
		})
	}
}

// The truncation PROBE row is a returned GAQL row, so it is subject to the same tenant
// check as every other returned row. Slicing the probe off before the scope loop would let
// the one row that proves the filter was not honoured escape assertCampaignInScope: the
// caller would receive a clean, capped, `truncated: true` answer built from a response that
// carried another project's campaign. The probe is checked and then discarded, never
// discarded and then unchecked.
func TestGetKeywordPerformance_OutOfScopeProbeRowFailsTheRead(t *testing.T) {
	rows := make([]string, 0, maxKeywordRows+1)
	for i := 0; i < maxKeywordRows; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "555", "kw", "BROAD", "ENABLED", 10, 1, 100))
	}
	// ONLY the probe row (the maxKeywordRows+1'th) names a campaign outside the scope.
	rows = append(rows, keywordRowJSON("9999", "176216228", "999", "kw", "BROAD", "ENABLED", 10, 1, 100))

	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(rows, ",")+`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err == nil {
		t.Fatalf("an out-of-scope probe row was accepted on a read scoped to [555]: "+
			"rows = %d, truncated = %v", len(kp.Rows), kp.Truncated)
	}
	if kp != nil {
		t.Errorf("a result was returned alongside the scope error: %+v", kp)
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "outside the requested campaign scope") {
		t.Errorf("error does not name the out-of-scope probe campaign: %v", err)
	}
}

// TestGetKeywordPerformance_NegativeKeywordIsNotReturned pins the polarity contract on the
// READ path. Every row this endpoint publishes is advertised as the criterion_id + ad_group_id
// handle `keyword-actions` takes, and that endpoint refuses a negative criterion outright,
// so a returned exclusion is a handle whose only advertised use cannot succeed.
//
// The positive row OMITS `negative` (the wire shape of the happy path); the negative row
// carries the explicit `negative: true` Google sends. Asserting on the returned VALUES rather
// than on the query text is deliberate: a test that only grepped the GAQL string would pass
// against a build that requested the filter and then published whatever came back.
func TestGetKeywordPerformance_NegativeKeywordIsNotReturned(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+
			keywordRowJSON("111", "176216228", "555", "kubernetes training", "EXACT", "ENABLED", 1000, 40, 25000000)+`,`+
			negativeKeywordRowJSON("222", "176216228", "555", "free", "BROAD", "ENABLED", 10, 0, 0)+
			`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the negative keyword must not be published)", len(kp.Rows))
	}
	if kp.Rows[0].CriterionID != "111" {
		t.Errorf("criterion id = %q, want 111", kp.Rows[0].CriterionID)
	}
	if kp.Rows[0].Text != "kubernetes training" {
		t.Errorf("text = %q, want the positive keyword", kp.Rows[0].Text)
	}
	for _, r := range kp.Rows {
		if r.CriterionID == "222" || r.Text == "free" {
			t.Errorf("NEGATIVE criterion %s (%q) was published as an actionable keyword handle", r.CriterionID, r.Text)
		}
	}
}

// TestGetKeywordPerformance_OmittedNegativeIsPositive is the companion guard to the test
// above, and the one that catches the failure mode a polarity check invites: reading an
// ABSENT `negative` as "unknown" and dropping the row. Absence already means false in
// protobuf JSON, so treating it as unknown would empty this endpoint of every ordinary
// keyword — the same defect that shipped once on the mutate path.
func TestGetKeywordPerformance_OmittedNegativeIsPositive(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Note: keywordRowJSON omits `negative` entirely, as Google does.
		_, _ = io.WriteString(w, `{"results":[`+
			keywordRowJSON("111", "176216228", "555", "kubernetes training", "EXACT", "ENABLED", 1000, 40, 25000000)+`,`+
			keywordRowJSON("112", "176216228", "555", "linux foundation", "PHRASE", "PAUSED", 500, 10, 5000000)+
			`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != 2 {
		t.Fatalf("got %d rows, want 2 — a row with `negative` OMITTED is POSITIVE and must be kept", len(kp.Rows))
	}
}

// TestGetKeywordPerformance_ExplicitNegativeFalseIsPositive covers the non-default
// serialisation. A conformant protobuf-JSON serialiser never emits `negative: false`, but a
// proxy or a future serialiser may, and it must reach the same POSITIVE verdict as omission.
func TestGetKeywordPerformance_ExplicitNegativeFalseIsPositive(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+
			`{"adGroupCriterion":{"criterionId":"111","status":"ENABLED","negative":false,`+
			`"keyword":{"text":"kubernetes training","matchType":"EXACT"}},`+
			`"adGroup":{"id":"176216228"},"campaign":{"id":"555"},`+
			`"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000"}}`+
			`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1 — explicit `negative: false` is POSITIVE", len(kp.Rows))
	}
}

// TestGetKeywordPerformance_QueryRequestsPositiveKeywordsOnly pins the REQUESTED filter, the
// half the response-side check cannot prove. Both matter: the predicate keeps exclusions from
// consuming the row cap upstream, and the enforcement keeps them out of the result if the
// predicate is ever dropped or not honoured.
func TestGetKeywordPerformance_QueryRequestsPositiveKeywordsOnly(t *testing.T) {
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
	defer mu.Unlock()
	if !strings.Contains(gotBody, "ad_group_criterion.negative = FALSE") {
		t.Errorf("query does not restrict to positive keywords; body = %s", gotBody)
	}
	if !strings.Contains(gotBody, "ad_group_criterion.negative,") {
		t.Errorf("query does not SELECT the polarity field, so the response cannot be checked; body = %s", gotBody)
	}
}

// TestApplyKeywordActions_PreMutationReadFailureIsNotUnconfirmed pins the boundary between a
// failed pre-mutation READ and an ambiguous MUTATION outcome.
//
// resolveKeywordCriteria's GAQL read fails with the same shapes a mutate does (transportError,
// 5xx, exhausted 429), and createOutcomeAmbiguous reads those as "may have committed". That is
// the right default for a mutate and the wrong answer here: adGroupCriteria:mutate is never
// built, so nothing can have landed. Reporting it as unconfirmed tells the caller to go and
// verify a batch that was never sent, and withholds the retry that is actually safe.
//
// The assertion is on the CLASSIFIER's verdict and on the mutate never being reached, not on
// the error text — the arm already SAID "no keyword was changed" while classifying as
// ambiguous, which is exactly how the contradiction survived.
func TestApplyKeywordActions_PreMutationReadFailureIsNotUnconfirmed(t *testing.T) {
	var mu sync.Mutex
	var mutateCalled bool
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate") {
			mu.Lock()
			mutateCalled = true
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		// The type-resolution googleAds:search read fails with a 503.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"backend unavailable"}}`)
	})

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("expected the read failure to surface as an error")
	}
	mu.Lock()
	called := mutateCalled
	mu.Unlock()
	if called {
		t.Fatal("adGroupCriteria:mutate was sent despite the type-resolution read failing")
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a pre-mutation READ failure classified as an UNCONFIRMED mutation outcome; "+
			"the mutate was never sent, so nothing can have been applied: %v", err)
	}
}

// TestApplyKeywordActions_MutateFailureStaysUnconfirmed is the counterpart, and the one that
// stops the fix above from being over-applied. A 5xx from adGroupCriteria:mutate ITSELF is
// genuinely ambiguous — Google may have committed the batch — and must still be reported that
// way. Without this, marking the read arm could be widened until the real ambiguity is lost,
// which for an irreversible REMOVE is the dangerous direction.
func TestApplyKeywordActions_MutateFailureStaysUnconfirmed(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"backend unavailable"}}`)
			return
		}
		// The type-resolution read succeeds and reports a POSITIVE keyword (negative omitted).
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+criterionRowJSON("176216228", "305729261", false)+`]}`)
	})

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove},
	})
	if err == nil {
		t.Fatal("expected the mutate failure to surface as an error")
	}
	if !IsOutcomeUnconfirmed(err) {
		t.Errorf("a 5xx from adGroupCriteria:mutate ITSELF must stay UNCONFIRMED — Google may "+
			"have applied the batch: %v", err)
	}

	// The check above is satisfied by the structural unconfirmedKeywordError wrapper alone, so
	// it cannot tell whether the SHAPE-BASED inference still works. Assert that separately on
	// the unwrapped cause: createOutcomeAmbiguous is what catches any future client arm that
	// reports a genuinely ambiguous mutate outcome WITHOUT remembering to wrap it, and a
	// not-attempted marker must not have suppressed it.
	unwrapped := errors.Unwrap(err)
	if unwrapped == nil {
		t.Fatalf("expected the unconfirmed wrapper to carry the underlying mutate error: %v", err)
	}
	if !IsOutcomeUnconfirmed(unwrapped) {
		t.Errorf("the shape-based ambiguity inference no longer classifies a bare mutate 5xx as "+
			"UNCONFIRMED; an unwrapped ambiguous outcome would be reported as a definite failure: %v", unwrapped)
	}
}

// TestApplyKeywordActions_NegativeCriterionStaysPermanentFault guards the unwrap chain through
// the new not-attempted wrapper. The dispatcher folds
// ErrKeywordCriterionNotPositiveKeyword onto a 400, and it matches with errors.Is — so a
// wrapper that failed to Unwrap would silently turn a permanent input fault into a retryable
// 503 inviting a retry that can never succeed.
func TestApplyKeywordActions_NegativeCriterionStaysPermanentFault(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+criterionRowJSON("176216228", "305729261", true)+`]}`)
	})

	_, err := c.ApplyKeywordActions(context.Background(), []KeywordAction{
		{AdGroupID: "176216228", CriterionID: "305729261", Action: KeywordActionRemove},
	})
	if !errors.Is(err, ErrKeywordCriterionNotPositiveKeyword) {
		t.Fatalf("negative criterion must stay a permanent input fault: %v", err)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a refused-before-mutating batch must not be UNCONFIRMED: %v", err)
	}
}

// TestGetKeywordPerformance_NegativeRowDoesNotConsumeACapSlot is the reproduction of the
// reviewer's case on PR #153: ONE negative row followed by exactly maxKeywordRows positive
// rows. The response carries maxKeywordRows+1 rows, but only maxKeywordRows of them are
// keywords this endpoint publishes — so the complete answer is the full cap, NOT truncated.
func TestGetKeywordPerformance_NegativeRowDoesNotConsumeACapSlot(t *testing.T) {
	rows := []string{negativeKeywordRowJSON("999", "176216228", "555", "free", "BROAD", "ENABLED", 10, 0, 0)}
	for i := 0; i < maxKeywordRows; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "555", "kw", "BROAD", "ENABLED", 10, 1, 100))
	}
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(rows, ",")+`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	t.Logf("OBSERVED: rows = %d, truncated = %v", len(kp.Rows), kp.Truncated)
	if len(kp.Rows) != maxKeywordRows || kp.Truncated {
		t.Fatalf("rows = %d, truncated = %v; want %d rows and truncated = false",
			len(kp.Rows), kp.Truncated, maxKeywordRows)
	}
}

// TestGetKeywordPerformance_TruncationCountsMatchingRowsOnly sweeps the cap/`truncated`
// pair across the polarity boundary instead of reasoning about it. Each case states the
// rows the server returns and the pair the caller must see; either half can be right while
// the pair is wrong, so both are asserted together.
//
// The reviewer's case on PR #153 is `one negative then a full page`. The cases that keep the
// fix honest are the ones where truncation is still TRUE: a fix that made `truncated` count
// publishable rows could just as easily have hardcoded it false.
func TestGetKeywordPerformance_TruncationCountsMatchingRowsOnly(t *testing.T) {
	positives := func(n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "555", "kw", "BROAD", "ENABLED", 10, 1, 100))
		}
		return out
	}
	negative := func(id string) string {
		return negativeKeywordRowJSON(id, "176216228", "555", "free", "BROAD", "ENABLED", 10, 0, 0)
	}

	cases := []struct {
		name      string
		rows      []string
		wantRows  int
		wantTrunc bool
		why       string
	}{
		{
			name:      "one negative then a full page of positives",
			rows:      append([]string{negative("999")}, positives(maxKeywordRows)...),
			wantRows:  maxKeywordRows,
			wantTrunc: false,
			why:       "the response held exactly a full page of publishable keywords and nothing beyond it",
		},
		{
			name:      "one negative then a full page plus a positive probe",
			rows:      append([]string{negative("999")}, positives(maxKeywordRows+1)...),
			wantRows:  maxKeywordRows,
			wantTrunc: true,
			why:       "a positive row past the cap is a keyword the caller has not been shown",
		},
		{
			name:      "a negative in the middle of a full page",
			rows:      append(append(positives(10), negative("999")), positives(maxKeywordRows-10)...),
			wantRows:  maxKeywordRows,
			wantTrunc: false,
			why:       "position must not change the verdict; the negative still consumes no slot",
		},
		{
			name:      "a negative as the last row of an over-full response",
			rows:      append(positives(maxKeywordRows), negative("999")),
			wantRows:  maxKeywordRows,
			wantTrunc: false,
			why:       "the only row past the cap is one this endpoint does not publish",
		},
		{
			name:      "every row negative",
			rows:      []string{negative("991"), negative("992"), negative("993")},
			wantRows:  0,
			wantTrunc: false,
			why:       "no publishable keywords, and none withheld — an empty answer is a complete one",
		},
		{
			name:      "positives only, one past the cap",
			rows:      positives(maxKeywordRows + 1),
			wantRows:  maxKeywordRows,
			wantTrunc: true,
			why:       "the unchanged all-positive probe case must keep reporting truncation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[`+strings.Join(tc.rows, ",")+`]}`)
			})
			kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
			if err != nil {
				t.Fatalf("GetKeywordPerformance: %v", err)
			}
			if len(kp.Rows) != tc.wantRows || kp.Truncated != tc.wantTrunc {
				t.Fatalf("(rows, truncated) = (%d, %v), want (%d, %v): %s",
					len(kp.Rows), kp.Truncated, tc.wantRows, tc.wantTrunc, tc.why)
			}
			for _, r := range kp.Rows {
				if r.Text == "free" {
					t.Errorf("negative criterion %s was published as an actionable handle", r.CriterionID)
				}
			}
		})
	}
}

// TestGetKeywordPerformance_NegativeProbeRowIsStillScopeChecked keeps the two fixes already
// on this PR from being traded against each other. The cap now counts MATCHED rows, so a
// negative row no longer advances it — that must not become a route by which a row escapes
// assertCampaignInScope. The out-of-scope row here is BOTH negative AND past the cap: the
// two conditions that each, on their own, cause a row to be dropped from the result.
func TestGetKeywordPerformance_NegativeProbeRowIsStillScopeChecked(t *testing.T) {
	rows := make([]string, 0, maxKeywordRows+1)
	for i := 0; i < maxKeywordRows; i++ {
		rows = append(rows, keywordRowJSON(fmt.Sprintf("%d", 1000+i), "176216228", "555", "kw", "BROAD", "ENABLED", 10, 1, 100))
	}
	rows = append(rows, negativeKeywordRowJSON("9999", "176216228", "999", "free", "BROAD", "ENABLED", 10, 0, 0))

	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(rows, ",")+`]}`)
	})

	kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
	if err == nil {
		t.Fatalf("an out-of-scope NEGATIVE probe row was accepted on a read scoped to [555]: "+
			"rows = %d, truncated = %v", len(kp.Rows), kp.Truncated)
	}
	if kp != nil {
		t.Errorf("a result was returned alongside the scope error: %+v", kp)
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "outside the requested campaign scope") {
		t.Errorf("error does not name the out-of-scope campaign: %v", err)
	}
}

// enrichedKeywordRowJSON builds a keyword_view row carrying the fields added for the UI
// cutover: the two display names, the quality score, and conversions.
//
// It renders conversions as a bare JSON NUMBER and the quality score as a nested number,
// because that is what Google sends: int64-valued fields are string-encoded to protect them
// from float64 precision loss, while metrics.conversions is DOUBLE upstream and
// quality_info.quality_score is int32, so neither gets the string treatment. A fixture that
// quoted them would agree with a decoder that expected strings instead of checking it.
func enrichedKeywordRowJSON(criterionID, adGroupID, adGroupName, campaignID, campaignName string, qualityScore int, conversions float64) string {
	return fmt.Sprintf(`{"adGroupCriterion":{"criterionId":%q,"status":"ENABLED",`+
		`"keyword":{"text":"kubernetes training","matchType":"EXACT"},`+
		`"qualityInfo":{"qualityScore":%d}},`+
		`"adGroup":{"id":%q,"name":%q},"campaign":{"id":%q,"name":%q},`+
		`"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000","conversions":%v}}`,
		criterionID, qualityScore, adGroupID, adGroupName, campaignID, campaignName, conversions)
}

// The four fields the UI needs must survive decoding with their VALUES intact, not merely
// be present on the struct. Asserting the values is what makes this test able to fail if a
// later change maps the wrong source field onto one of them — a presence check would pass
// against a decoder that assigned the campaign's name to the ad group.
func TestGetKeywordPerformance_CarriesNamesQualityScoreAndConversions(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[`+
			enrichedKeywordRowJSON("305729261", "176216228", "Registration - Exact", "555", "KubeCon NA 2026 - Search", 7, 12.5)+
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
	// Distinct expected values per field: the ad group and campaign names differ, so a
	// decoder that crossed them fails here rather than passing on a shared placeholder.
	if got.AdGroupName != "Registration - Exact" {
		t.Errorf("AdGroupName = %q, want %q", got.AdGroupName, "Registration - Exact")
	}
	if got.CampaignName != "KubeCon NA 2026 - Search" {
		t.Errorf("CampaignName = %q, want %q", got.CampaignName, "KubeCon NA 2026 - Search")
	}
	if got.QualityScore == nil {
		t.Fatalf("QualityScore = nil, want 7")
	}
	if *got.QualityScore != 7 {
		t.Errorf("QualityScore = %d, want 7", *got.QualityScore)
	}
	// The FRACTION must survive. Google credits fractional conversions under data-driven
	// attribution, so a decode into an integer type would silently truncate 12.5 to 12.
	if got.Conversions != 12.5 {
		t.Errorf("Conversions = %v, want 12.5", got.Conversions)
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()
	// The query must ASK for every field the struct decodes. A decoder that can parse a
	// field Google was never asked for returns a zero value on every real response, which
	// is exactly the failure this half of the test exists to catch: the struct assertions
	// above pass against a hand-written fixture regardless of what the SELECT contains.
	for _, field := range []string{
		"ad_group_criterion.quality_info.quality_score",
		"ad_group.name",
		"campaign.name",
		"metrics.conversions",
	} {
		if !strings.Contains(body, field) {
			t.Errorf("query does not select %s: %s", field, body)
		}
	}
}

// An unrated keyword must arrive as nil, NOT as a score of 0. Google withholds the rating
// until a keyword has accrued enough impressions, so this is the ordinary state of a new
// keyword rather than an edge case — and 0 is off the 1-10 scale, so a caller shown zero
// reads it as the worst possible rating.
//
// Absence is asserted at BOTH levels because Google produces both: `qualityInfo` is omitted
// entirely for an unrated keyword, and the score within it can be omitted when the block is
// present. A guard covering only one level leaves the other returning a fabricated zero.
func TestGetKeywordPerformance_UnratedKeywordHasNoQualityScore(t *testing.T) {
	cases := []struct {
		name string
		row  string
	}{
		{
			// The whole qualityInfo block absent — an ordinary new keyword.
			name: "quality info block omitted",
			row: `{"adGroupCriterion":{"criterionId":"1","status":"ENABLED",` +
				`"keyword":{"text":"x","matchType":"EXACT"}},` +
				`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},` +
				`"metrics":{"impressions":"10","clicks":"1","costMicros":"100"}}`,
		},
		{
			// The block present but carrying no score.
			name: "quality score omitted within the block",
			row: `{"adGroupCriterion":{"criterionId":"1","status":"ENABLED",` +
				`"keyword":{"text":"x","matchType":"EXACT"},"qualityInfo":{}},` +
				`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},` +
				`"metrics":{"impressions":"10","clicks":"1","costMicros":"100"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[`+tc.row+`]}`)
			})
			kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
			if err != nil {
				t.Fatalf("GetKeywordPerformance: %v", err)
			}
			if len(kp.Rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(kp.Rows))
			}
			if got := kp.Rows[0].QualityScore; got != nil {
				t.Errorf("QualityScore = %d, want nil — an unrated keyword must not report a score", *got)
			}
			// An omitted conversions field is a MEASURED zero, unlike the quality score:
			// Google always measures conversions for a served keyword, so absence here
			// carries no ambiguity and is correctly reported as 0.
			if got := kp.Rows[0].Conversions; got != 0 {
				t.Errorf("Conversions = %v, want 0 for an omitted field", got)
			}
		})
	}
}

// The device breakdown returns one row per (campaign, device) pair, so a bucket's
// conversions arrive spread across rows. They must be SUMMED like the other counters —
// taking any single row's figure reports one campaign's conversions as the whole device's.
func TestGetAudienceInsights_SumsConversionsAcrossRows(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Only the device query is given two rows for one bucket; the other two
		// breakdowns answer empty so this asserts the aggregation in isolation.
		if strings.Contains(string(b), "FROM campaign") {
			_, _ = io.WriteString(w, `{"results":[`+
				`{"segments":{"device":"MOBILE"},"campaign":{"id":"555"},`+
				`"metrics":{"impressions":"100","clicks":"10","costMicros":"1000","conversions":1.5}},`+
				`{"segments":{"device":"MOBILE"},"campaign":{"id":"555"},`+
				`"metrics":{"impressions":"200","clicks":"20","costMicros":"2000","conversions":2.25}}`+
				`]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	ai, err := c.GetAudienceInsights(context.Background(), WindowLast30Days, []string{"555"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if len(ai.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(ai.Buckets))
	}
	got := ai.Buckets[0]
	// 1.5 + 2.25: a sum that is neither row's value and is not an integer, so neither
	// "took the first row" nor "took the last row" nor a truncating decode can pass.
	if want := 3.75; got.Conversions != want {
		t.Errorf("Conversions = %v, want %v", got.Conversions, want)
	}
	// The counters it is summed alongside, to pin that this row really is the aggregate.
	if got.Impressions != 300 || got.Clicks != 30 {
		t.Errorf("aggregated counters = %+v", got)
	}
}

// A quality score OUTSIDE the 1-10 scale must not be published as a score.
//
// This is a response-validation hazard, not a cosmetic one. The design declares
// quality_score with Minimum(1)/Maximum(10), and Goa emits response validation in the
// generated CLIENT — so a single out-of-range row would make the client reject the ENTIRE
// keywords response, taking down a whole report over one keyword. That is the same failure
// the match_type enum comment warns about, reached from the other direction.
//
// Google should never send 0 on the 1-10 scale, so this is defensive: the point is that the
// blast radius of being wrong is the whole response, which is worth one guard.
func TestGetKeywordPerformance_OutOfRangeQualityScoreIsNotPublished(t *testing.T) {
	for _, score := range []int{0, 11, -1} {
		t.Run(fmt.Sprintf("score %d", score), func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fmt.Sprintf(`{"results":[{"adGroupCriterion":{"criterionId":"1","status":"ENABLED",`+
					`"keyword":{"text":"x","matchType":"EXACT"},"qualityInfo":{"qualityScore":%d}},`+
					`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},`+
					`"metrics":{"impressions":"10","clicks":"1","costMicros":"100"}}]}`, score))
			})
			kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
			if err != nil {
				t.Fatalf("GetKeywordPerformance: %v", err)
			}
			if len(kp.Rows) != 1 {
				t.Fatalf("rows = %d, want 1 — an out-of-range score must drop the SCORE, not the row", len(kp.Rows))
			}
			if got := kp.Rows[0].QualityScore; got != nil {
				t.Errorf("QualityScore = %d, want nil for an out-of-scale score", *got)
			}
			// The rest of the row must survive. Dropping the whole row over an unusable
			// score would lose real spend and impressions from the report.
			if kp.Rows[0].Impressions != 10 {
				t.Errorf("row metrics lost: %+v", kp.Rows[0])
			}
		})
	}
}

// The guard's bounds must equal the bounds the DESIGN declares, because the generated
// client validates the response against the design and rejects the whole body on a
// violation. Nothing else ties them together: widening Maximum(10) to 11 in design/brief.go
// while these constants stay at 10 leaves every test green and merely under-reports, but
// NARROWING the design below these constants publishes a value the client refuses — and the
// failure appears at a consumer, on a real Google response, not here.
//
// Asserted by reading the design source rather than by restating the numbers, so the two
// cannot drift apart in the direction that breaks the response.
func TestKeywordQualityScoreBoundsMatchTheDesign(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "design", "brief.go"))
	if err != nil {
		t.Fatalf("read design: %v", err)
	}
	// Scoped to the quality_score attribute's own func literal: the file declares Minimum
	// and Maximum on other attributes too, so an unscoped search would match whichever
	// happened to come first and pass regardless of what quality_score says.
	block := regexp.MustCompile(`(?s)Attribute\("quality_score".*?\n\t\}\)`).Find(src)
	if block == nil {
		t.Fatal("design no longer declares a quality_score attribute — this guard is now unpinned")
	}
	for _, tc := range []struct {
		name  string
		re    *regexp.Regexp
		guard int64
	}{
		{"Minimum", regexp.MustCompile(`Minimum\((\d+)\)`), minQualityScore},
		{"Maximum", regexp.MustCompile(`Maximum\((\d+)\)`), maxQualityScore},
	} {
		m := tc.re.FindSubmatch(block)
		if m == nil {
			t.Errorf("design's quality_score declares no %s — the guard is unpinned in that direction", tc.name)
			continue
		}
		want, convErr := strconv.ParseInt(string(m[1]), 10, 64)
		if convErr != nil {
			t.Fatalf("parse design %s: %v", tc.name, convErr)
		}
		if want != tc.guard {
			t.Errorf("design %s(%d) != guard constant %d — an out-of-range score would fail the whole response", tc.name, want, tc.guard)
		}
	}
}

// A NEGATIVE keyword carrying an unusable conversions value must not fail the whole report.
//
// The row is dropped either way — this endpoint publishes only positive keywords — so
// validating its counters can only ever turn a droppable row into a total failure. That is
// exactly the outcome the polarity-drop comment says dropping exists to avoid: "erroring would
// let one exclusion in an ad group take down the whole keyword report."
//
// The fixture pairs the poisoned exclusion with a HEALTHY positive row, so the assertion is not
// merely "no error" — it is that the good row still comes back. A version that dropped both
// would satisfy a bare error check while silently losing real keywords.
func TestGetKeywordPerformance_CorruptNegativeRowDoesNotFailTheReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		conv string
	}{
		{"negative count", "-5"},
		{"beyond int64", "1e19"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[`+
					// An exclusion whose conversions cannot be parsed. In scope, so the scope
					// check passes and the polarity drop is what decides its fate.
					`{"adGroupCriterion":{"criterionId":"1","status":"ENABLED","negative":true,`+
					`"keyword":{"text":"free","matchType":"BROAD"}},`+
					`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},`+
					`"metrics":{"impressions":"10","clicks":"1","costMicros":"100","conversions":`+tc.conv+`}},`+
					// A healthy positive row that must survive.
					`{"adGroupCriterion":{"criterionId":"9","status":"ENABLED",`+
					`"keyword":{"text":"kubernetes training","matchType":"EXACT"}},`+
					`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},`+
					`"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000","conversions":12.5}}`+
					`]}`)
			})

			kp, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"})
			if err != nil {
				t.Fatalf("a corrupt EXCLUSION failed the whole report: %v", err)
			}
			if len(kp.Rows) != 1 {
				t.Fatalf("rows = %d, want the one positive keyword to survive", len(kp.Rows))
			}
			if kp.Rows[0].CriterionID != "9" {
				t.Errorf("wrong row survived: %+v", kp.Rows[0])
			}
		})
	}
}

// The same corruption on a POSITIVE row still fails the whole response. The two cases are
// different facts: an exclusion is a row this endpoint does not publish, while a published row
// with an unusable counter would be rendered as a measurement.
func TestGetKeywordPerformance_CorruptPositiveRowStillFailsTheReport(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"1","status":"ENABLED",`+
			`"keyword":{"text":"x","matchType":"EXACT"}},`+
			`"adGroup":{"id":"2","name":"AG"},"campaign":{"id":"555","name":"C"},`+
			`"metrics":{"impressions":"10","clicks":"1","costMicros":"100","conversions":-5}}]}`)
	})

	if _, err := c.GetKeywordPerformance(context.Background(), WindowLast30Days, []string{"555"}); err == nil {
		t.Fatal("a corrupt POSITIVE row was published rather than failing the response")
	}
}
