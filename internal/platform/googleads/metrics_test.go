// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func metricsRange(t *testing.T) (time.Time, time.Time) {
	t.Helper()
	from, err := time.Parse(metricsDateLayout, "2026-07-01")
	if err != nil {
		t.Fatalf("parse from: %v", err)
	}
	to, err := time.Parse(metricsDateLayout, "2026-07-03")
	if err != nil {
		t.Fatalf("parse to: %v", err)
	}
	return from, to
}

// TestFetchCampaignMetrics_DecodesGoogleWireTypes pins the response contract. Google
// encodes int64 metrics as quoted JSON STRINGS (protobuf int64 encoding) and doubles
// as bare numbers; decoding either into the wrong Go type silently yields zeros.
func TestFetchCampaignMetrics_DecodesGoogleWireTypes(t *testing.T) {
	var gotQuery string
	c := twoServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req searchRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		gotQuery = req.Query
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[
			{"segments":{"date":"2026-07-01"},
			 "metrics":{"impressions":"15000","clicks":"321","costMicros":"1234567","conversions":2.5},
			 "customer":{"currencyCode":"USD"}},
			{"segments":{"date":"2026-07-02"},
			 "metrics":{"impressions":"20000","clicks":"400","costMicros":"7654321","conversions":0.25},
			 "customer":{"currencyCode":"USD"}}
		]}`)
	})

	from, to := metricsRange(t)
	rows, err := c.FetchCampaignMetrics(context.Background(), "555", from, to)
	if err != nil {
		t.Fatalf("FetchCampaignMetrics: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	// int64-as-string must decode to the real number, not 0.
	if rows[0].Impressions != 15000 || rows[0].Clicks != 321 {
		t.Errorf("row0 impressions/clicks = %d/%d, want 15000/321", rows[0].Impressions, rows[0].Clicks)
	}
	// cost_micros -> whole currency units, EXACTLY.
	if rows[0].Spend != "1.234567" {
		t.Errorf("row0 Spend = %s, want 1.234567 (1234567 micros)", rows[0].Spend)
	}
	if rows[1].Spend != "7.654321" {
		t.Errorf("row1 Spend = %s, want 7.654321", rows[1].Spend)
	}
	// Fractional conversions must survive; truncating to an integer would lose data.
	if rows[0].Conversions != "2.500000" {
		t.Errorf("row0 Conversions = %s, want 2.500000", rows[0].Conversions)
	}
	if rows[1].Conversions != "0.250000" {
		t.Errorf("row1 Conversions = %s, want 0.250000 (a sub-1 fractional conversion must not floor to 0)", rows[1].Conversions)
	}
	if rows[0].Currency != "USD" {
		t.Errorf("row0 Currency = %q, want USD", rows[0].Currency)
	}
	if !rows[0].Date.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("row0 Date = %v, want 2026-07-01", rows[0].Date)
	}
	// The raw platform response must be retained for auditability.
	if len(rows[0].Raw) == 0 || !strings.Contains(string(rows[0].Raw), "costMicros") {
		t.Errorf("row0 Raw = %q, want the verbatim platform row", rows[0].Raw)
	}

	// The query must select segments.date (one row per day = the storage grain) and
	// must NOT select campaign.start_date/end_date, which v23 rejects.
	for _, want := range []string{"segments.date", "metrics.cost_micros", "metrics.conversions", "customer.currency_code", "FROM campaign", "campaign.id = 555"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q; got: %s", want, gotQuery)
		}
	}
	if strings.Contains(gotQuery, "campaign.start_date") || strings.Contains(gotQuery, "campaign.end_date") {
		t.Errorf("query selects campaign.start_date/end_date, which Google Ads v23 REJECTS as unrecognized: %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "'2026-07-01' AND '2026-07-03'") {
		t.Errorf("query date window wrong; got: %s", gotQuery)
	}
}

// TestFetchCampaignMetrics_RejectsNonNumericCampaignID is the injection guard. GAQL
// has no bind parameters, so the id is interpolated into query TEXT and must be
// proven digits-only rather than escaped.
func TestFetchCampaignMetrics_RejectsNonNumericCampaignID(t *testing.T) {
	var called bool
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})

	from, to := metricsRange(t)
	for _, bad := range []string{
		"123 OR 1=1",
		"1' OR '1'='1",
		"123; SELECT campaign.id FROM campaign",
		"abc",
		"",
		"12-34",
	} {
		if _, err := c.FetchCampaignMetrics(context.Background(), bad, from, to); err == nil {
			t.Errorf("campaign id %q was ACCEPTED; a non-numeric id interpolated into GAQL text could alter the WHERE clause and read another campaign's rows", bad)
		}
	}
	if called {
		t.Error("a rejected campaign id still reached the API; validation must happen before any request is sent")
	}
}

// TestFetchCampaignMetrics_RejectsInvertedRange guards against a silently-empty
// result: Google returns no rows for an inverted BETWEEN, which is indistinguishable
// from "this campaign had no activity".
func TestFetchCampaignMetrics_RejectsInvertedRange(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	from, to := metricsRange(t)
	if _, err := c.FetchCampaignMetrics(context.Background(), "555", to, from); err == nil {
		t.Error("an inverted date range was accepted; it yields zero rows, which reads as 'no activity' rather than 'bad request'")
	}
}

// TestFetchCampaignMetrics_MalformedRowIsAnErrorNotASkip: dropping a row that fails
// to decode would UNDER-REPORT spend, making a campaign look cheaper than it was.
func TestFetchCampaignMetrics_MalformedRowIsAnErrorNotASkip(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Second row has a date the layout cannot parse.
		_, _ = io.WriteString(w, `{"results":[
			{"segments":{"date":"2026-07-01"},"metrics":{"impressions":"1","clicks":"1","costMicros":"1000000","conversions":1},"customer":{"currencyCode":"USD"}},
			{"segments":{"date":"not-a-date"},"metrics":{"impressions":"9","clicks":"9","costMicros":"9000000","conversions":9},"customer":{"currencyCode":"USD"}}
		]}`)
	})
	from, to := metricsRange(t)
	rows, err := c.FetchCampaignMetrics(context.Background(), "555", from, to)
	if err == nil {
		t.Fatalf("an unparseable date was tolerated and %d rows returned; silently dropping a row under-reports spend", len(rows))
	}
	if !strings.Contains(err.Error(), "segments.date") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestFetchCampaignMetrics_PaginatesViaSearchTransport confirms the fetcher inherits
// the existing cursor pagination rather than reading only the first page.
func TestFetchCampaignMetrics_PaginatesViaSearchTransport(t *testing.T) {
	var page int
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			_, _ = io.WriteString(w, `{"results":[{"segments":{"date":"2026-07-01"},"metrics":{"impressions":"1","clicks":"1","costMicros":"1000000","conversions":1},"customer":{"currencyCode":"USD"}}],"nextPageToken":"tok2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"segments":{"date":"2026-07-02"},"metrics":{"impressions":"2","clicks":"2","costMicros":"2000000","conversions":2},"customer":{"currencyCode":"USD"}}]}`)
	})
	from, to := metricsRange(t)
	rows, err := c.FetchCampaignMetrics(context.Background(), "555", from, to)
	if err != nil {
		t.Fatalf("FetchCampaignMetrics: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows across 2 pages, want 2 — pagination was not followed", len(rows))
	}
}

// TestFetchCampaignMetrics_EmptyResultIsNotAnError: a campaign that genuinely did not
// serve returns no rows, which is valid data, not a failure.
func TestFetchCampaignMetrics_EmptyResultIsNotAnError(t *testing.T) {
	c := twoServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	from, to := metricsRange(t)
	rows, err := c.FetchCampaignMetrics(context.Background(), "555", from, to)
	if err != nil {
		t.Fatalf("empty result should not be an error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestMicrosToDecimalIsExact(t *testing.T) {
	cases := []struct{ micros, want string }{
		{"1234567", "1.234567"},
		{"1", "0.000001"}, // the smallest unit must not vanish
		{"0", "0.000000"},
		{"1000000", "1.000000"},
		{"", "0.000000"},
		{"not-a-number", "0.000000"},
	}
	for _, tc := range cases {
		if got := microsToDecimal(tc.micros); got != tc.want {
			t.Errorf("microsToDecimal(%q) = %s, want %s", tc.micros, got, tc.want)
		}
	}
}

// TestMicrosToDecimalRejectsFloatConversion is the guard that makes the big.Rat
// implementation load-bearing: if someone rewrites microsToDecimal as
// ParseFloat(micros)/1e6, these cases MUST fail.
//
// Small values do not discriminate — 1234567/1e6 is exact in float64, so a test
// built only from those would pass against a float64 implementation and prove
// nothing. The values below are large-but-legitimate: the spend column is
// NUMERIC(18,6), i.e. up to ~1e12 currency units = ~1e18 micros, and float64 carries
// only ~15-16 significant digits. Past 2^53 micros the integer itself is no longer
// exactly representable, so the conversion loses real money.
func TestMicrosToDecimalRejectsFloatConversion(t *testing.T) {
	cases := []struct {
		name, micros, want, floatWould string
	}{
		{
			name:   "2^53+1 micros: first integer float64 cannot represent",
			micros: "9007199254740993", want: "9007199254.740993", floatWould: "9007199254.740992",
		},
		{
			name:   "high-spend account loses its last digit",
			micros: "12345678901234567", want: "12345678901.234567", floatWould: "12345678901.234568",
		},
		{
			name:   "column-max spend rounds up to a whole extra unit",
			micros: "999999999999999999", want: "999999999999.999999", floatWould: "1000000000000.000000",
		},
		{
			name:   "smallest unit on a large balance is swallowed",
			micros: "100000000000000001", want: "100000000000.000001", floatWould: "100000000000.000000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := microsToDecimal(tc.micros); got != tc.want {
				t.Errorf("microsToDecimal(%q) = %s, want %s", tc.micros, got, tc.want)
			}

			// Prove the case discriminates: a float64 implementation must differ.
			f, err := strconv.ParseFloat(tc.micros, 64)
			if err != nil {
				t.Fatalf("test constant %q is not a number: %v", tc.micros, err)
			}
			got := strconv.FormatFloat(f/1e6, 'f', 6, 64)
			if got == tc.want {
				t.Errorf("case does not discriminate: float64 also yields %s, so this would pass against a float64 implementation", got)
			}
			if got != tc.floatWould {
				t.Errorf("float64 yields %s, expected %s — the documented failure mode has changed", got, tc.floatWould)
			}
		})
	}
}

func TestNormalizeDecimalHandlesExponentNotation(t *testing.T) {
	// A JSON number may legitimately arrive as 1e-3; naive string handling would
	// store the literal "1e-3" or read it as 1.
	if got := normalizeDecimal("1e-3"); got != "0.001000" {
		t.Errorf("normalizeDecimal(1e-3) = %s, want 0.001000", got)
	}
	if got := normalizeDecimal("2.5"); got != "2.500000" {
		t.Errorf("normalizeDecimal(2.5) = %s, want 2.500000", got)
	}
	if got := normalizeDecimal(""); got != "0.000000" {
		t.Errorf("normalizeDecimal(empty) = %s, want 0.000000", got)
	}
}
