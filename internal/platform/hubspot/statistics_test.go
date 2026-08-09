// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fixedClock pins now() so window boundaries are assertable. 2026-03-15T12:00:00Z sits
// mid-month in a 31-day month whose predecessor (February 2026) has 28 days — the shape
// that catches an AddDate(0,-1,0) month-arithmetic bug.
func fixedClock(t *testing.T, c *Client) *Client {
	t.Helper()
	return clockAt(t, c, time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
}

// clockAt pins now() to an arbitrary instant, for the cases whose whole point is the date.
func clockAt(t *testing.T, c *Client, at time.Time) *Client {
	t.Helper()
	withClock(func() time.Time { return at })(c)
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
		{"today", "2026-03-15T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"yesterday", "2026-03-14T00:00:00Z", "2026-03-14T23:59:59.999Z"},
		{"last_7_days", "2026-03-09T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"last_14_days", "2026-03-02T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"last_30_days", "2026-02-14T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		{"this_month", "2026-03-01T00:00:00Z", "2026-03-15T23:59:59.999Z"},
		// February 2026 has 28 days. A month computed by subtracting one month from the
		// 15th would still land here, so the assertion that matters is the 28th end date.
		{"last_month", "2026-02-01T00:00:00Z", "2026-02-28T23:59:59.999Z"},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			// Guarded: httptest runs the handler on its own goroutine, so these writes and
			// the reads below are a data race even though the request has returned by then
			// — a happens-before the race detector cannot see.
			var mu sync.Mutex
			var gotStart, gotEnd, gotIDs, gotPath string
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotPath = r.URL.Path
				q := r.URL.Query()
				gotStart, gotEnd, gotIDs = q.Get("startTimestamp"), q.Get("endTimestamp"), q.Get("emailIds")
				mu.Unlock()
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindow(tc.window)); err != nil {
				t.Fatalf("GetEmailMetrics: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
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

// Company is a filter failure too, and this is the case a presence check waves through.
// The request names exactly one email and `aggregate` is the aggregation over the emails
// the response covers, so a list of three means the counters include two strangers'
// sends. Accepting it reports those sends as this campaign's — the same misattribution as
// accepting a response for the wrong email outright, just harder to notice.
func TestGetEmailMetrics_RejectsAResponseListingTheEmailAmongOthers(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[1,4242,9999]`, fullCounters))
	})
	_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrStatisticsFilterNotHonored) {
		t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
	}
}

// The absent-field half of the vocabulary guard. A renamed or dropped `counters` field
// decodes to a nil map, so `len(counters) > 0` never fires and every lookup returns 0 —
// an email HubSpot has just told us it covers reports as having sent nothing. The empty
// object and the missing key are the same break and both must be caught. Nothing licenses
// zeros here: an empty `emails` list is its own error, which
// TestGetEmailMetrics_EmptyEmailsListMeansTheWindowMissedTheSend pins from the other side.
func TestGetEmailMetrics_MissingCountersForACoveredEmailIsAnErrorNotZeros(t *testing.T) {
	for _, body := range []string{
		`{"emails":[4242],"aggregate":{"counters":{}}}`,
		`{"emails":[4242],"aggregate":{}}`,
		`{"emails":[4242]}`,
	} {
		t.Run(body, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if !errors.Is(err, ErrUnrecognizedCounters) {
				t.Fatalf("err = %v, want ErrUnrecognizedCounters", err)
			}
		})
	}
}

// An EMPTY emails list means HubSpot did not include this email in the span, and the
// contract says why: the span selects emails by SEND time, so a window that does not
// contain the send date matches nothing.
//
// Zeros are the wrong answer, and this test exists because the obvious reading — "empty
// means no activity" — gets it backwards. The email that really had no activity comes back
// PRESENT (see TestGetEmailMetrics_UnmappedButKnownCountersAreARealZero, where `[4242]`
// arrives carrying only `notsent`). So zeroing the empty case would make "you picked a
// window that predates the send" and "nobody opened it" the same answer, which is exactly
// the case where a live campaign reads as a dead one.
func TestGetEmailMetrics_EmptyEmailsListMeansTheWindowMissedTheSend(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, statsBody(t, `[]`, `{}`))
	})
	m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
	if !errors.Is(err, ErrEmailNotSentInWindow) {
		t.Fatalf("err = %v, want ErrEmailNotSentInWindow", err)
	}
	if m != nil {
		t.Errorf("metrics = %+v, want nil: a partial result here would be read as real zeros", m)
	}
}

// The other half of that boundary. A response with NO `emails` field at all decodes to the
// same nil as `[]` if the field is a value slice, and the empty-list branch would then
// report a schema break as "wrong window" — sending the caller off to retry other windows
// against a response that can never carry what it needs. The field is the only evidence
// the filter was honoured, so its absence belongs with the filter guard.
func TestGetEmailMetrics_AbsentEmailsFieldIsNotAnEmptyList(t *testing.T) {
	for name, body := range map[string]string{
		"omitted": `{"aggregate":{"counters":{"sent":1000}}}`,
		"null":    `{"emails":null,"aggregate":{"counters":{"sent":1000}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if !errors.Is(err, ErrStatisticsFilterNotHonored) {
				t.Fatalf("err = %v, want ErrStatisticsFilterNotHonored", err)
			}
			if errors.Is(err, ErrEmailNotSentInWindow) {
				t.Errorf("err = %v, must not read as a window miss: the field is absent, not empty", err)
			}
		})
	}
}

// The PARTIAL rename, which the whole-vocabulary guard below cannot see: `sent` survives,
// so at least one known key is present and that guard passes, while `open` has become
// `emailsOpened` and the mapped lookup returns an authoritative 0 for an email with 400
// opens. Both halves of the signature are required, and the two subtests that must NOT
// error are as much the point as the one that must — an omitted zero counter and an
// additive upstream key are ordinary, and rejecting either would break the client on a
// release that took nothing away.
func TestGetEmailMetrics_PartiallyRenamedCounterVocabularyIsAnError(t *testing.T) {
	tests := map[string]struct {
		counters string
		wantErr  bool
	}{
		"renamed open, sent intact":     {`{"sent":1000,"emailsOpened":400}`, true},
		"mapped key merely absent":      {`{"sent":1000,"open":400}`, false},
		"unknown key merely added":      {`{"sent":1,"delivered":1,"open":1,"click":1,"bounce":1,"unsubscribed":1,"engagements":9}`, false},
		"all mapped absent, only known": {`{"notsent":1}`, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, tc.counters))
			})
			_, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if got := errors.Is(err, ErrRenamedCounter); got != tc.wantErr {
				t.Fatalf("ErrRenamedCounter = %v, want %v (err = %v)", got, tc.wantErr, err)
			}
		})
	}
}

// Counters are event counts, so a negative is malformed upstream data. Left unchecked it
// becomes a negative Impressions and a negative Ctr in the public response, both of which
// read as authoritative. LinkedIn, Meta and Reddit all reject negatives; this closes the
// same hole on HubSpot. The unmapped-key case is included deliberately: a negative
// anywhere is evidence the payload is wrong, and the keys we DO read are no more
// trustworthy for having stayed positive.
func TestGetEmailMetrics_NegativeCounterIsAnError(t *testing.T) {
	for _, counters := range []string{
		`{"sent":1000,"delivered":900,"open":-1,"click":10,"bounce":0,"unsubscribed":0}`,
		`{"sent":1000,"delivered":900,"open":5,"click":10,"bounce":0,"unsubscribed":0,"spamreport":-3}`,
	} {
		t.Run(counters, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, counters))
			})
			m, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday)
			if !errors.Is(err, ErrNegativeCounter) {
				t.Fatalf("err = %v, want ErrNegativeCounter", err)
			}
			if m != nil {
				t.Errorf("metrics = %+v, want nil", m)
			}
		})
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
			// atomic for the same reason as the range assertions: the handler runs on the
			// server's goroutine, and the assertion below is on the test's.
			var called atomic.Bool
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), id, model.MetricsWindowToday); err == nil {
				t.Fatalf("id %q was accepted", id)
			}
			if called.Load() {
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
	// atomic, and not merely because of the assertion at the end: the two retry attempts
	// are separate handler invocations that the server may run on different goroutines, so
	// the read-modify-write itself is the race.
	var attempts atomic.Int64
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
	})
	if _, err := fixedClock(t, c).GetEmailMetrics(context.Background(), "4242", model.MetricsWindowToday); err != nil {
		t.Fatalf("GetEmailMetrics: %v", err)
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

// TestGetEmailMetrics_MonthWindowsOnAMonthEndDate is the case the 2026-03-15 fixture
// cannot make: subtracting a month from the 15th is always valid, so a naive
// AddDate(0, -1, 0) would pass every assertion above.
//
// On the 31st it does not. AddDate NORMALIZES an out-of-range day — 2026-03-31 minus one
// month is 2026-02-31, which becomes 2026-03-03 — so the naive form would report last_month
// as a few days of MARCH, and this_month's start would follow it out of the month entirely.
// timeRangeForWindow avoids that by computing both from the first of the month; this pins
// that it still does.
func TestGetEmailMetrics_MonthWindowsOnAMonthEndDate(t *testing.T) {
	cases := []struct{ window, start, end string }{
		{"this_month", "2026-03-01T00:00:00Z", "2026-03-31T23:59:59.999Z"},
		{"last_month", "2026-02-01T00:00:00Z", "2026-02-28T23:59:59.999Z"},
	}
	for _, tc := range cases {
		t.Run(tc.window, func(t *testing.T) {
			var mu sync.Mutex
			var gotStart, gotEnd string
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				q := r.URL.Query()
				gotStart, gotEnd = q.Get("startTimestamp"), q.Get("endTimestamp")
				mu.Unlock()
				_, _ = io.WriteString(w, statsBody(t, `[4242]`, fullCounters))
			})
			onTheLastOfMarch := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
			if _, err := clockAt(t, c, onTheLastOfMarch).GetEmailMetrics(
				context.Background(), "4242", model.MetricsWindow(tc.window)); err != nil {
				t.Fatalf("GetEmailMetrics: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotStart != tc.start || gotEnd != tc.end {
				t.Errorf("range = %s..%s, want %s..%s", gotStart, gotEnd, tc.start, tc.end)
			}
		})
	}
}
