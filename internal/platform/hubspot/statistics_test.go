// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fixedClock pins now() so window boundaries are assertable. 2026-03-15T12:00:00Z sits
// mid-month in a 31-day month whose predecessor (February 2026) has 28 days — the shape
// that catches an AddDate(0,-1,0) month-arithmetic bug.
func fixedClock(t *testing.T, c *Client) *Client {
	t.Helper()
	withClock(func() time.Time { return time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC) })(c)
	return c
}

// statsBody renders a statistics response with the given emails list and counters.
func statsBody(t *testing.T, emails string, counters string) string {
	t.Helper()
	return `{"emails":` + emails + `,"campaignAggregations":{},"aggregate":{"counters":` + counters +
		`,"ratios":{"openratio":0.5},"deviceBreakdown":{},"qualifierStats":{}}}`
}

const fullCounters = `{"sent":1000,"delivered":950,"open":400,"click":80,"bounce":50,"unsubscribed":7,"notsent":0}`

func TestGetEmailMetrics_MapsCountersAndComputesCtr(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if m.Impressions != 400 || m.Clicks != 80 {
		t.Errorf("impressions/clicks = %d/%d, want 400/80", m.Impressions, m.Clicks)
	}
	// 80/400. Computed here, NOT taken from the response's own openratio/clickratio, so
	// every platform's ctr means the same thing.
	if m.Ctr != 0.2 {
		t.Errorf("ctr = %v, want 0.2", m.Ctr)
	}
	if m.CostMicros != 0 {
		t.Errorf("cost_micros = %d, want 0 (email is not billed per send)", m.CostMicros)
	}
	if m.Window != model.MetricsWindowLast30Days {
		t.Errorf("window = %q", m.Window)
	}
	if m.Email == nil {
		t.Fatal("Email counters are nil; the email channel must populate them")
	}
	want := model.EmailMetrics{Sent: 1000, Delivered: 950, Opens: 400, Clicks: 80, Bounces: 50, Unsubscribes: 7}
	if *m.Email != want {
		t.Errorf("email = %+v, want %+v", *m.Email, want)
	}
}

func TestGetEmailMetrics_SendsTheWindowAsAnInclusiveUTCRange(t *testing.T) {
	// Every window, so a mis-mapped boundary is caught for all seven rather than the one
	// that happens to be exercised elsewhere. end is always the final millisecond of the
	// last day — see timeRangeForWindow on why not next-midnight.
	cases := []struct{ window, start, end string }{
		{"today", "2026-03-15T00:00:00Z", "2026-03-15T23:59:59Z"},
		{"yesterday", "2026-03-14T00:00:00Z", "2026-03-14T23:59:59Z"},
		{"last_7_days", "2026-03-09T00:00:00Z", "2026-03-15T23:59:59Z"},
		{"last_14_days", "2026-03-02T00:00:00Z", "2026-03-15T23:59:59Z"},
		{"last_30_days", "2026-02-14T00:00:00Z", "2026-03-15T23:59:59Z"},
		{"this_month", "2026-03-01T00:00:00Z", "2026-03-15T23:59:59Z"},
		// February 2026 has 28 days. A month computed by subtracting one month from the
		// 15th would still land here, so the assertion that matters is the 28th end date.
		{"last_month", "2026-02-01T00:00:00Z", "2026-02-28T23:59:59Z"},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			var gotStart, gotEnd, gotIDs, gotPath string
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				q := r.URL.Query()
				gotStart, gotEnd, gotIDs = q.Get("startTimestamp"), q.Get("endTimestamp"), q.Get("emailIds")
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindow(tc.window)); err != nil {
				t.Fatalf("GetEmailMetrics: %v", err)
			}
			if gotPath != statisticsPath {
				t.Errorf("path = %q, want %q", gotPath, statisticsPath)
			}
			if gotStart != tc.start || gotEnd != tc.end {
				t.Errorf("range = %s..%s, want %s..%s", gotStart, gotEnd, tc.start, tc.end)
			}
			if gotIDs != "4242" {
				t.Errorf("emailIds = %q, want 4242", gotIDs)
			}
		})
	}
}

// The response is unverifiable when its email list names something other than what we
// asked for: the filter was not honoured, so nothing in it describes this campaign.
// Reporting its counters would attribute a stranger's sends to this email.
func TestGetEmailMetrics_RejectsAResponseForADifferentEmail(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[9999]`, fullCounters))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrStatisticsFilterNotHonored) {
		t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
	}
}

// An email present alongside others is still covered — the guard rejects ABSENCE from the
// list, not the presence of company. Without this the guard would be an equality check.
func TestGetEmailMetrics_AcceptsAResponseListingTheEmailAmongOthers(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[1,4242,9999]`, fullCounters))
	})
	if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday); err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
}

// An EMPTY emails list is the API's way of saying the email had no activity in the window.
// That is an ordinary result and must read as zeros, not as a filter failure — the
// difference between "this campaign did nothing" and "we could not tell".
func TestGetEmailMetrics_EmptyEmailsListIsZerosNotAnError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[]`, `{}`))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Clicks != 0 || m.Ctr != 0 {
		t.Errorf("want all zeros, got %+v", m)
	}
	if m.Email == nil || *m.Email != (model.EmailMetrics{}) {
		t.Errorf("email = %+v, want zero value", m.Email)
	}
}

// The fail-closed guard. `counters` is an OPEN map in HubSpot's v3 schema, so a renamed
// key set decodes cleanly and every lookup returns 0 — an email that really sent would
// report as having sent nothing. That is indistinguishable from a dead campaign, so it
// must be an error instead.
func TestGetEmailMetrics_UnrecognizedCounterVocabularyIsAnErrorNotZeros(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, `{"emailsSent":1000,"emailsOpened":400}`))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrUnrecognizedCounters) {
		t.Fatalf("err = %v, want ErrUnrecognizedCounters", err)
	}
}

// The companion to the guard above, and the reason the probe set is wider than the six
// mapped keys: an email created but never sent can come back carrying only counters this
// client does not map. That is a real zero, not a vocabulary change.
func TestGetEmailMetrics_UnmappedButKnownCountersAreARealZero(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, `{"notsent":12,"pending":3}`))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if m.Impressions != 0 || m.Email.Sent != 0 {
		t.Errorf("want zeros, got %+v / %+v", m, m.Email)
	}
}

// The counter names are not enumerated in HubSpot's v3 schema, so they are pinned here:
// an upstream rename should fail this test rather than silently zero a dashboard.
func TestCounterKeysAreTheDocumentedVocabulary(t *testing.T) {
	for name, got := range map[string]string{
		"sent": counterSent, "delivered": counterDelivered, "open": counterOpen,
		"click": counterClick, "bounce": counterBounce, "unsubscribed": counterUnsubscribed,
	} {
		if got != name {
			t.Errorf("counter constant for %q = %q", name, got)
		}
		if _, ok := knownCounterVocabulary[got]; !ok {
			t.Errorf("mapped counter %q is missing from knownCounterVocabulary, so a response carrying only it would be rejected", got)
		}
	}
}

// A malformed stored id is reported as malformed rather than answered with "no metrics":
// emailIds is an integer list upstream, so a non-integer would either be rejected there or
// ignored — and an ignored filter widens the query to the whole portal.
func TestGetEmailMetrics_RejectsAMalformedEmailIDBeforeAnyRequest(t *testing.T) {
	for _, id := range []string{"", "abc", "0", "-5", "+42", "042", " 42", "42 ", "42.0", "4242abc"} {
		t.Run(id, func(t *testing.T) {
			var called bool
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), id, model.MetricsWindowToday); err == nil {
				t.Fatalf("id %q was accepted", id)
			}
			if called {
				t.Error("a request was sent for a malformed id")
			}
		})
	}
}

// Pins that the validator and the range builder cannot disagree about which windows are
// supported — a window one accepts and the other rejects would surface as a 503 on a
// request the API surface advertises.
func TestValidateMetricsWindowMatchesTimeRangeForWindow(t *testing.T) {
	c := fixedClock(t, NewClient(testCreds(), testAccount()))
	for _, w := range []model.MetricsWindow{
		model.MetricsWindowToday, model.MetricsWindowYesterday, model.MetricsWindowLast7Days,
		model.MetricsWindowLast14Days, model.MetricsWindowLast30Days, model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth, model.MetricsWindow("not_a_window"), model.MetricsWindow(""),
	} {
		validErr := ValidateMetricsWindow(w)
		_, _, rangeErr := c.timeRangeForWindow(w)
		if (validErr == nil) != (rangeErr == nil) {
			t.Errorf("window %q: ValidateMetricsWindow=%v but timeRangeForWindow=%v", w, validErr, rangeErr)
		}
	}
}

// The whole shared vocabulary is supported, because HubSpot takes arbitrary instants. A
// future window added to model without a case here must fail, not fall through.
func TestEveryMetricsWindowIsSupported(t *testing.T) {
	c := fixedClock(t, NewClient(testCreds(), testAccount()))
	for _, w := range []model.MetricsWindow{
		model.MetricsWindowToday, model.MetricsWindowYesterday, model.MetricsWindowLast7Days,
		model.MetricsWindowLast14Days, model.MetricsWindowLast30Days, model.MetricsWindowThisMonth,
		model.MetricsWindowLastMonth,
	} {
		start, end, err := c.timeRangeForWindow(w)
		if err != nil {
			t.Errorf("window %q: %v", w, err)
			continue
		}
		if !start.Before(end) {
			t.Errorf("window %q: start %s is not before end %s", w, start, end)
		}
	}
}

// A 429 on this read is retried (idempotent=true), which is what distinguishes it from the
// clone path. Asserted through the public method so a future change to the idempotent flag
// at the call site is caught.
func TestGetEmailMetrics_RetriesA429(t *testing.T) {
	var attempts int
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
	})
	if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday); err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}
