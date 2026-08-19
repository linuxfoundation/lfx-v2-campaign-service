// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

// WHAT THESE FIXTURES ARE, AND WHAT THEY CANNOT PROVE
//
// The response bodies below are modelled on Reddit's official public OpenAPI spec
// (https://ads-api.reddit.com/api/v3/openapi.json, operation POST
// /ad_accounts/{ad_account_id}/reports, schemas Report and ReportMetric), verified on
// LFXV2-3282. They are NOT captures from a live Reddit ad account — this repository has
// no Reddit credentials, and no request has ever been made against the real endpoint.
//
// So these tests prove that the client agrees with the PUBLISHED SCHEMA. They cannot
// prove that the published schema is what a live account returns, and no assertion here
// should be read as evidence of that. The behaviours a schema cannot express are called
// out where they matter: whether a campaign with no activity yields an empty metrics
// array or an explicit zero row, and whether ends_at is inclusive of its final hour, are
// both unconfirmed. Where the tests encode an assumption rather than a documented fact,
// the test comment says so.
//
// The guards in metrics.go are written so that a wrong assumption FAILS rather than
// returning a plausible number, and the tests below exercise those refusals directly —
// see TestGetCampaignMetrics_UnrecognisedRowShapeIsRefusedNotZero, which is the
// regression test for the failure class this whole file exists to prevent.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func newMetricsTestClient(t *testing.T, apiHandler http.HandlerFunc) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(apiHandler)
	t.Cleanup(apiSrv.Close)

	return NewClient(testCreds, testAccount,
		WithBaseURL(apiSrv.URL+"/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()),
	)
}

// metricsHandler serves a fixed report body and records nothing.
func metricsHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotPath string
	var gotBody map[string]any

	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// Shape per the spec's Report schema: data is an OBJECT with a metrics array, and
		// spend is an int64 in microcurrency (25.50 of the account's currency = 25500000).
		_, _ = w.Write([]byte(`{"data":{"metrics":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":25500000}]},"pagination":{}}`))
	})

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 1000 {
		t.Errorf("expected 1000 impressions, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 50 {
		t.Errorf("expected 50 clicks, got %d", metrics.Clicks)
	}
	// Spend passes through unscaled: the spec documents it as microcurrency already.
	// Multiplying by 1e6 here (the pre-LFXV2-3282 behaviour) would give 2.55e13.
	if metrics.CostMicros != 25_500_000 {
		t.Errorf("expected 25500000 costMicros passed through unscaled, got %d", metrics.CostMicros)
	}
	if metrics.Ctr != 0.05 {
		t.Errorf("expected CTR 0.05, got %f", metrics.Ctr)
	}
	if metrics.CampaignID != "camp_123" {
		t.Errorf("expected CampaignID camp_123, got %q", metrics.CampaignID)
	}
	if metrics.Window != model.MetricsWindowLast7Days {
		t.Errorf("expected Window last_7_days, got %q", metrics.Window)
	}

	mu.Lock()
	method, path, body := gotMethod, gotPath, gotBody
	mu.Unlock()
	if method != http.MethodPost {
		t.Errorf("expected POST, got %s", method)
	}
	if path != "/api/v3/ad_accounts/t2_test/reports" {
		t.Errorf("expected path /api/v3/ad_accounts/t2_test/reports, got %s", path)
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatal("expected a data envelope in the request body")
	}
	// The whole request body is asserted, not just one key. The window translation and the
	// field/filter selection decide WHICH numbers come back, so a wrong date range or a
	// dropped field yields a well-formed response for the wrong period or with a missing
	// metric — nothing downstream can detect that.
	//
	// The campaign is scoped by the spec's `filter` string DSL. The schema sets
	// additionalProperties:false, so the pre-LFXV2-3282 `campaign_ids` array would have
	// been rejected by Reddit outright.
	if got := data["filter"]; got != "campaign:id==camp_123" {
		t.Errorf("expected filter campaign:id==camp_123, got %v", got)
	}
	if _, present := data["campaign_ids"]; present {
		t.Error("campaign_ids is not in the spec's request schema (additionalProperties:false) and must not be sent")
	}
	// The clock is pinned at 2026-07-01 (fixedRedditClock), so last_7_days is the
	// inclusive 7-day span ending today, rendered in the spec's required
	// YYYY-MM-DDTHH:00:00Z hourly form.
	if got := data["starts_at"]; got != "2026-06-25T00:00:00Z" {
		t.Errorf("expected starts_at 2026-06-25T00:00:00Z, got %v", got)
	}
	if got := data["ends_at"]; got != "2026-07-01T23:00:00Z" {
		t.Errorf("expected ends_at 2026-07-01T23:00:00Z, got %v", got)
	}
	// Fields are UPPERCASE enum members per the spec. CAMPAIGN_ID must be requested as a
	// FIELD, not merely a breakdown, or no row would carry the id the provenance check
	// below verifies against.
	fields, _ := data["fields"].([]any)
	wantFields := []string{"CAMPAIGN_ID", "IMPRESSIONS", "CLICKS", "SPEND"}
	if len(fields) != len(wantFields) {
		t.Fatalf("expected fields %v, got %v", wantFields, data["fields"])
	}
	for i, want := range wantFields {
		if fields[i] != want {
			t.Errorf("fields[%d]: expected %q, got %v", i, want, fields[i])
		}
	}
}

// TestGetCampaignMetrics_RequestTimestampsAreHourlyZulu pins the exact rendering the spec
// requires ("Must follow the `YYYY-MM-DDTHH:00:00Z` format"). It is separate from the
// happy path because this is the guess whose failure is QUIETEST: a rejected format is
// loud, but a format Reddit accepts and reads as a different instant returns a
// well-formed report for the wrong period. Both prior renderings are ruled out
// explicitly — a bare date, and this client's own toRedditTimestamp "+00:00" offset.
func TestGetCampaignMetrics_RequestTimestampsAreHourlyZulu(t *testing.T) {
	for _, window := range []model.MetricsWindow{
		model.MetricsWindowToday,
		model.MetricsWindowLast7Days,
		model.MetricsWindowLast30Days,
		model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth,
	} {
		t.Run(string(window), func(t *testing.T) {
			var mu sync.Mutex
			var body map[string]any
			client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"metrics":[]}}`))
			})
			if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", window); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			mu.Lock()
			data, _ := body["data"].(map[string]any)
			mu.Unlock()
			for _, key := range []string{"starts_at", "ends_at"} {
				got, _ := data[key].(string)
				if _, err := time.Parse("2006-01-02T15:00:00Z", got); err != nil {
					t.Errorf("%s = %q is not the spec's YYYY-MM-DDTHH:00:00Z form", key, got)
				}
				if strings.Contains(got, "+00:00") {
					t.Errorf("%s = %q uses the write path's +00:00 offset; reporting requires a literal Z", key, got)
				}
				if !strings.HasSuffix(got, "Z") {
					t.Errorf("%s = %q must end in Z", key, got)
				}
			}
		})
	}
}

// TestGetCampaignMetrics_UnrecognisedRowShapeIsRefusedNotZero is the regression test for
// the failure class this file exists to prevent, and the single highest-value test here.
//
// Before LFXV2-3282 the row struct used VALUE fields. A response whose keys did not match
// the guessed tags decoded every metric to Go's zero value, so the client returned
// impressions=0, clicks=0, cost=0, ctr=0 with a nil error — a measurement indistinguishable
// from a campaign that genuinely served nothing, published with no indication that the
// decode had matched nothing at all. That was verified by probe, not assumed: two of five
// wrong-shape bodies returned exactly that silent zero.
//
// Each case below is a shape the response could plausibly take if an assumption is wrong.
// Every one must produce an ERROR. A returned CampaignMetrics — even an all-zero one — is
// the bug.
func TestGetCampaignMetrics_UnrecognisedRowShapeIsRefusedNotZero(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// The exact shape that silently returned zero before this change.
			name: "row uses entirely different metric names",
			body: `{"data":{"metrics":[{"campaign_id":"camp_123","impression_count":1000,"click_count":50,"amount_spent":25500000}]}}`,
		},
		{
			// The spec types every metric as ["integer","null"], so an explicit null is a
			// response Reddit is permitted to send. It must not read as a zero.
			name: "explicit nulls for every metric",
			body: `{"data":{"metrics":[{"campaign_id":"camp_123","impressions":null,"clicks":null,"spend":null}]}}`,
		},
		{
			name: "one metric null, the rest present",
			body: `{"data":{"metrics":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":null}]}}`,
		},
		{
			name: "row omits the requested spend field",
			body: `{"data":{"metrics":[{"campaign_id":"camp_123","impressions":1000,"clicks":50}]}}`,
		},
		{
			name: "row omits campaign_id so provenance cannot be checked",
			body: `{"data":{"metrics":[{"impressions":1000,"clicks":50,"spend":25500000}]}}`,
		},
		{
			// Rows nested one level deeper than the spec's Report schema.
			name: "metrics array is not where the schema puts it",
			body: `{"data":{"report":{"metrics":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":25500000}]}}}`,
		},
		{
			name: "data is a bare array (the pre-3282 assumption)",
			body: `{"data":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":25500000}]}`,
		},
		{
			name: "metrics is null rather than an empty array",
			body: `{"data":{"metrics":null}}`,
		},
		{
			name: "data object carries no metrics key at all",
			body: `{"data":{"pagination":{}}}`,
		},
		{
			name: "data field is null",
			body: `{"data":null}`,
		},
		{
			name: "response has no data field",
			body: `{"pagination":{}}`,
		},
		{
			// If spend ever arrives as the decimal string the old code assumed, that is a
			// contract change worth failing on — not something to coerce, since reading a
			// decimal as microcurrency (or the reverse) is a factor-of-1e6 error.
			name: "spend as a decimal string",
			body: `{"data":{"metrics":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":"25.50"}]}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newMetricsTestClient(t, metricsHandler(tc.body))
			got, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
			if err == nil {
				t.Fatalf("expected a refusal, got metrics %+v — an unreadable response must never be reported as a measurement", got)
			}
			if got != nil {
				t.Errorf("expected nil metrics alongside the error, got %+v", got)
			}
		})
	}
}

// TestGetCampaignMetrics_MissingFieldErrorNamesTheFields pins that the refusal is
// ACTIONABLE. Whoever first flips REDDIT_METRICS_ENABLED against a live account should
// learn from one error which requested fields the report did not carry, rather than
// debugging a decoder. It also pins that no upstream VALUE is echoed.
func TestGetCampaignMetrics_MissingFieldErrorNamesTheFields(t *testing.T) {
	client := newMetricsTestClient(t, metricsHandler(
		`{"data":{"metrics":[{"campaign_id":"camp_123","impressions":1000}]}}`))

	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected a refusal for a row missing clicks and spend")
	}
	msg := err.Error()
	for _, want := range []string{"clicks", "spend"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should name the missing field %q so the cause is one read away; got: %s", want, msg)
		}
	}
	// impressions WAS present, so naming it would misdirect the reader.
	if strings.Contains(msg, "missing or null field(s) impressions") {
		t.Errorf("error must not name a field that was present; got: %s", msg)
	}
}

// TestGetCampaignMetrics_ErrorsLeakNoUpstreamValues pins the repo rule that no credential,
// token, id, or unvalidated upstream content reaches an error string — these errors are
// rendered into the service's warning log.
func TestGetCampaignMetrics_ErrorsLeakNoUpstreamValues(t *testing.T) {
	cases := []struct {
		name, body string
		forbidden  []string
	}{
		{
			name:      "mismatched campaign id is not echoed",
			body:      `{"data":{"metrics":[{"campaign_id":"camp_999_SECRET","impressions":1,"clicks":0,"spend":0}]}}`,
			forbidden: []string{"camp_999_SECRET"},
		},
		{
			name:      "malformed data body is not echoed",
			body:      `{"data":{"metrics":"tok_LEAKED_VALUE"}}`,
			forbidden: []string{"tok_LEAKED_VALUE"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newMetricsTestClient(t, metricsHandler(tc.body))
			_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, bad := range tc.forbidden {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error leaked upstream content %q: %s", bad, err.Error())
				}
			}
			// The account id is interpolated into the real request path; the error reports
			// the bare "reports" path instead.
			if strings.Contains(err.Error(), testAccount.AccountID) {
				t.Errorf("error leaked the account id: %s", err.Error())
			}
		})
	}
}

// TestGetCampaignMetrics_NonSuccessErrorsHideTheAccountID covers the arms the decode-side
// invariant never reached. reportDecodeError pins the literal "reports" path only for a 2xx
// whose body cannot be decoded; a 4xx/5xx builds an apiError, and a transport failure a
// transportError, both carrying the REAL request path with the ad account id interpolated
// into it — and both stringify into the service's warning log.
//
// These are the arms that actually fire during an outage, so the invariant has to hold here
// or it does not hold at all.
func TestGetCampaignMetrics_NonSuccessErrorsHideTheAccountID(t *testing.T) {
	t.Run("non-2xx response", func(t *testing.T) {
		client := newMetricsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))
		})
		_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testAccount.AccountID) {
			t.Errorf("a non-2xx error leaked the account id: %s", err.Error())
		}
		// The diagnostic that matters must survive.
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("the status code was lost from the error: %s", err.Error())
		}
		if !strings.Contains(err.Error(), "reports") {
			t.Errorf("the error no longer identifies the reports call: %s", err.Error())
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		// A handler that hijacks and closes the connection produces a Do-time failure.
		client := newMetricsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Skip("ResponseWriter is not a Hijacker")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Skip("hijack failed")
			}
			_ = conn.Close()
		})
		_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testAccount.AccountID) {
			t.Errorf("a transport error leaked the account id: %s", err.Error())
		}
	})
}

// TestGetCampaignMetrics_NoActivity covers an empty metrics array.
//
// ASSUMPTION, NOT A DOCUMENTED FACT: this test encodes that a campaign with no activity
// yields an empty metrics array. The spec does not say whether Reddit instead returns one
// row of explicit zeros, and that cannot be settled without a live account. Both readings
// produce zero metrics here — the row-of-zeros case flows through the accumulation loop
// instead — so the client is correct either way, but this test is not evidence for which
// one Reddit actually does.
func TestGetCampaignMetrics_NoActivity(t *testing.T) {
	client := newMetricsTestClient(t, metricsHandler(`{"data":{"metrics":[]},"pagination":{}}`))

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected zero metrics, got %+v", metrics)
	}
	if metrics.CampaignID != "camp_123" {
		t.Errorf("expected CampaignID camp_123, got %q", metrics.CampaignID)
	}
	if metrics.Window != model.MetricsWindowToday {
		t.Errorf("expected Window today, got %q", metrics.Window)
	}
}

// TestGetCampaignMetrics_ExplicitZeroRowIsAccepted covers the other reading of the same
// unknown: a row present with genuine zeros must NOT be refused. The missing-field guards
// key on absence, never on the value being zero, and this pins that distinction.
func TestGetCampaignMetrics_ExplicitZeroRowIsAccepted(t *testing.T) {
	client := newMetricsTestClient(t, metricsHandler(
		`{"data":{"metrics":[{"campaign_id":"camp_123","impressions":0,"clicks":0,"spend":0}]}}`))

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("a row of genuine zeros is real data and must be accepted: %v", err)
	}
	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 || metrics.Ctr != 0 {
		t.Errorf("expected zero metrics, got %+v", metrics)
	}
}

func TestGetCampaignMetrics_EmptyCampaignID(t *testing.T) {
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made for an empty campaign id")
	})
	_, err := client.GetCampaignMetrics(context.Background(), "  ", model.MetricsWindowToday)
	if !errors.Is(err, ErrInvalidCampaignID) {
		t.Fatalf("expected ErrInvalidCampaignID, got %v", err)
	}
}

// TestGetCampaignMetrics_InvalidCampaignIDCharacters covers ids that must never reach the
// wire. The charset guard now protects two interpolation sites, not one: the URL path AND
// the `filter` DSL, where a comma would split one filter term into two and silently widen
// the report's scope to another campaign.
func TestGetCampaignMetrics_InvalidCampaignIDCharacters(t *testing.T) {
	for _, id := range []string{
		"camp/123",
		"camp?123",
		"camp#123",
		"camp_123,campaign:id==camp_999", // filter-DSL injection
		"camp 123",
		"camp==123",
	} {
		t.Run(id, func(t *testing.T) {
			client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request should be made for invalid campaign id %q", id)
			})
			_, err := client.GetCampaignMetrics(context.Background(), id, model.MetricsWindowToday)
			if !errors.Is(err, ErrInvalidCampaignID) {
				t.Fatalf("expected ErrInvalidCampaignID for %q, got %v", id, err)
			}
		})
	}
}

// TestGetCampaignMetrics_UnsupportedWindowIsRejectedBeforeAnyRequest pins BOTH the
// sentinel and the ordering. The window is validated before the account is resolved so an
// unsupported window stays a permanent 400 rather than being masked as a retryable
// connection failure by a misconfigured account.
func TestGetCampaignMetrics_UnsupportedWindowIsRejectedBeforeAnyRequest(t *testing.T) {
	var mu sync.Mutex
	handlerCalled := false
	client := newMetricsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handlerCalled = true
		mu.Unlock()
	})
	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindow("last_year"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("expected ErrUnsupportedWindow, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if handlerCalled {
		t.Error("request should not reach the server for an unsupported window")
	}
}

// TestGetCampaignMetrics_UnsupportedWindowBeatsMissingAccount pins the ORDER directly: with
// both an unsupported window and an unusable account, the window error must win, because
// only it is permanent.
func TestGetCampaignMetrics_UnsupportedWindowBeatsMissingAccount(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)
	client := NewClient(testCreds, AccountConfig{AccountID: ""},
		WithBaseURL("http://127.0.0.1:1/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()))

	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindow("last_year"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("the permanent window error must be reported ahead of the account error, got %v", err)
	}
}

func TestGetCampaignMetrics_MalformedResponseIsDecodeError(t *testing.T) {
	client := newMetricsTestClient(t, metricsHandler(`not json`))
	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected a decode error for a malformed response")
	}
}

// TestGetCampaignMetrics_RowGuardsAreRefusals covers the per-row guards that reject a row
// which cannot describe reality, plus the checked additions that stop a running total
// from wrapping past MaxInt64.
func TestGetCampaignMetrics_RowGuardsAreRefusals(t *testing.T) {
	tests := []struct {
		name string
		rows string
	}{
		{
			name: "mismatched campaign id",
			rows: `{"campaign_id":"camp_999","impressions":10,"clicks":1,"spend":100}`,
		},
		{
			name: "blank campaign id",
			rows: `{"campaign_id":"","impressions":10,"clicks":1,"spend":100}`,
		},
		{
			name: "negative impressions",
			rows: `{"campaign_id":"camp_123","impressions":-1,"clicks":5,"spend":100}`,
		},
		{
			name: "negative clicks",
			rows: `{"campaign_id":"camp_123","impressions":10,"clicks":-1,"spend":100}`,
		},
		{
			name: "negative spend",
			rows: `{"campaign_id":"camp_123","impressions":10,"clicks":1,"spend":-100}`,
		},
		{
			// Impossible rather than merely low: every click is preceded by its impression.
			// Reporting it would publish Ctr=0 beside a non-zero click count.
			name: "clicks with zero impressions",
			rows: `{"campaign_id":"camp_123","impressions":0,"clicks":5,"spend":100}`,
		},
		{
			// One row cannot trip an overflow guard: the running total starts at zero, so
			// the branch is only reachable with a second row.
			name: "impressions total overflows",
			rows: `{"campaign_id":"camp_123","impressions":9223372036854775807,"clicks":1,"spend":1},` +
				`{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":1}`,
		},
		{
			name: "clicks total overflows",
			rows: `{"campaign_id":"camp_123","impressions":1,"clicks":9223372036854775807,"spend":1},` +
				`{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":1}`,
		},
		{
			name: "spend total overflows",
			rows: `{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":9223372036854775807},` +
				`{"campaign_id":"camp_123","impressions":1,"clicks":1,"spend":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMetricsTestClient(t, metricsHandler(`{"data":{"metrics":[`+tt.rows+`]}}`))
			got, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
			if err == nil {
				t.Fatalf("expected a refusal for %s, got %+v", tt.name, got)
			}
		})
	}
}

// TestGetCampaignMetrics_MultipleRowsAccumulate exercises the decode loop with more than
// one row. Without a breakdown the report should aggregate over the whole window, but the
// client does not depend on that: it sums whatever rows arrive, so a multi-row response
// still totals correctly.
func TestGetCampaignMetrics_MultipleRowsAccumulate(t *testing.T) {
	client := newMetricsTestClient(t, metricsHandler(`{"data":{"metrics":[`+
		`{"campaign_id":"camp_123","impressions":1000,"clicks":40,"spend":10000000},`+
		`{"campaign_id":"camp_123","impressions":600,"clicks":20,"spend":5250000},`+
		`{"campaign_id":"camp_123","impressions":400,"clicks":20,"spend":750000}`+
		`]}}`))

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Impressions != 2000 {
		t.Errorf("expected 2000 impressions summed across rows, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 80 {
		t.Errorf("expected 80 clicks summed across rows, got %d", metrics.Clicks)
	}
	if metrics.CostMicros != 16_000_000 {
		t.Errorf("expected 16000000 costMicros summed across rows, got %d", metrics.CostMicros)
	}
	// CTR is recomputed from the TOTALS (80/2000), not averaged per row — a per-row mean
	// would give 0.0333, which is what this assertion rules out.
	if metrics.Ctr != 0.04 {
		t.Errorf("expected CTR recomputed from totals (0.04), got %f", metrics.Ctr)
	}
}

// TestGetCampaignMetrics_ExtraRowFieldsAreIgnored pins that the client tolerates the
// hundreds of other properties the spec's ReportMetric schema defines. Rejecting unknown
// fields would break on any field Reddit adds.
func TestGetCampaignMetrics_ExtraRowFieldsAreIgnored(t *testing.T) {
	client := newMetricsTestClient(t, metricsHandler(
		`{"data":{"metrics":[{"campaign_id":"camp_123","impressions":1000,"clicks":50,"spend":25500000,`+
			`"ctr":0.999,"cpc":0.75,"ecpm":0.5,"date":"2026-07-01","video_started":7}]},"pagination":{"total_count":1}}`))

	metrics, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// CTR is derived from the totals, NOT read from the row's own ctr field — the fixture
	// sets ctr to 0.999 precisely so a client that trusted it would fail here.
	if metrics.Ctr != 0.05 {
		t.Errorf("expected CTR derived from totals (0.05), got %f", metrics.Ctr)
	}
}

// TestValidateMetricsWindowMatchesDateRangeForWindow pins that the two can never disagree
// about which windows are supported.
//
// EVERY domain constant is listed by NAME, including the two Reddit does not support. Naming
// them via model.* rather than a string literal is what makes the test load-bearing: adding
// only MetricsWindowLast14Days to supportedMetricsWindows without teaching dateRangeForWindow
// about it would otherwise slip through, since a literal "yesterday" tests nothing about the
// constant set. The unsupported ones must fail BOTH functions, exactly like the junk values.
func TestValidateMetricsWindowMatchesDateRangeForWindow(t *testing.T) {
	all := []model.MetricsWindow{
		model.MetricsWindowToday,
		model.MetricsWindowLast7Days,
		model.MetricsWindowLast30Days,
		model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth,
		// Domain constants Reddit does not map — named, not spelled.
		model.MetricsWindowYesterday,
		model.MetricsWindowLast14Days,
		model.MetricsWindow(""),
		model.MetricsWindow("last_year"),
	}
	for _, w := range all {
		validErr := ValidateMetricsWindow(w)
		_, _, rangeErr := dateRangeForWindow(w, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
		if (validErr == nil) != (rangeErr == nil) {
			t.Errorf("window %q: ValidateMetricsWindow err=%v but dateRangeForWindow err=%v", w, validErr, rangeErr)
		}
	}
}

func TestDateRangeForWindow(t *testing.T) {
	// Pinned clock: 2026-07-01 (a month start, so last_month exercises the boundary).
	now := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		window             model.MetricsWindow
		wantStart, wantEnd string
	}{
		{model.MetricsWindowToday, "2026-07-01T00:00:00Z", "2026-07-01T23:00:00Z"},
		{model.MetricsWindowLast7Days, "2026-06-25T00:00:00Z", "2026-07-01T23:00:00Z"},
		{model.MetricsWindowLast30Days, "2026-06-02T00:00:00Z", "2026-07-01T23:00:00Z"},
		{model.MetricsWindowThisMonth, "2026-07-01T00:00:00Z", "2026-07-01T23:00:00Z"},
		// June has 30 days: the first-of-month anchor must not normalize into July.
		{model.MetricsWindowLastMonth, "2026-06-01T00:00:00Z", "2026-06-30T23:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(string(tt.window), func(t *testing.T) {
			start, end, err := dateRangeForWindow(tt.window, now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tt.wantStart {
				t.Errorf("start: want %s, got %s", tt.wantStart, start)
			}
			if end != tt.wantEnd {
				t.Errorf("end: want %s, got %s", tt.wantEnd, end)
			}
		})
	}
}

// TestDateRangeForWindow_MonthEndBoundaries pins the AddDate normalization hazard the
// first-of-month anchor exists to avoid. On the 31st, a naive AddDate(0,-1,0) yields
// March 3rd (normalizing Feb 31), which would silently shift both month windows.
func TestDateRangeForWindow_MonthEndBoundaries(t *testing.T) {
	tests := []struct {
		name               string
		now                time.Time
		wantStart, wantEnd string
	}{
		{
			name:      "31st of March looks back at February",
			now:       time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC),
			wantStart: "2026-02-01T00:00:00Z",
			wantEnd:   "2026-02-28T23:00:00Z",
		},
		{
			name:      "leap February",
			now:       time.Date(2024, 3, 31, 9, 0, 0, 0, time.UTC),
			wantStart: "2024-02-01T00:00:00Z",
			wantEnd:   "2024-02-29T23:00:00Z",
		},
		{
			name:      "January looks back across the year boundary",
			now:       time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC),
			wantStart: "2025-12-01T00:00:00Z",
			wantEnd:   "2025-12-31T23:00:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := dateRangeForWindow(model.MetricsWindowLastMonth, tt.now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("want [%s, %s], got [%s, %s]", tt.wantStart, tt.wantEnd, start, end)
			}
		})
	}
}

// TestDateRangeForWindow_AnchorsToUTC pins that the range is derived from the UTC calendar
// date regardless of the clock's zone — matching the UTC default the spec documents for
// time_zone_id. A client in Asia/Tokyo at 06:00 on Jul 2 local is at 21:00 on Jul 1 UTC,
// so "today" must query Jul 1.
func TestDateRangeForWindow_AnchorsToUTC(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)
	start, end, err := dateRangeForWindow(model.MetricsWindowToday, time.Date(2026, 7, 2, 6, 0, 0, 0, tokyo))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-07-01T00:00:00Z" || end != "2026-07-01T23:00:00Z" {
		t.Errorf("expected the UTC calendar day 2026-07-01, got [%s, %s]", start, end)
	}
}

func TestGetCampaignMetrics_MissingAccountID(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)
	client := NewClient(testCreds, AccountConfig{AccountID: "  "},
		WithBaseURL("http://127.0.0.1:1/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()))

	if _, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday); err == nil {
		t.Fatal("expected an error when the account id is not configured")
	}
}

// TestGetCampaignMetrics_InvalidAccountIDIsNotEchoed pins that a malformed account id is
// rejected without reproducing it in the error, which reaches the server log.
func TestGetCampaignMetrics_InvalidAccountIDIsNotEchoed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)
	client := NewClient(testCreds, AccountConfig{AccountID: "t2_bad/../secret"},
		WithBaseURL("http://127.0.0.1:1/api/v3"),
		WithTokenURL(tokenSrv.URL),
		WithNowFunc(fixedRedditClock()))

	_, err := client.GetCampaignMetrics(context.Background(), "camp_123", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error for a malformed account id")
	}
	if strings.Contains(err.Error(), "t2_bad") {
		t.Errorf("error must not echo the account id: %s", err.Error())
	}
}
