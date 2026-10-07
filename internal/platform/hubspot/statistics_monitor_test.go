// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// queryRec captures the statistics request's query. Written by the handler goroutine, read by
// the test goroutine after the call returns.
type queryRec struct {
	mu    sync.Mutex
	query url.Values
	calls int
}

func (r *queryRec) set(q url.Values) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.query, r.calls = q, r.calls+1
}

func (r *queryRec) get() (url.Values, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.query, r.calls
}

func monitorClient(t *testing.T, body string) (*Client, *queryRec) {
	t.Helper()
	rec := &queryRec{}
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.set(r.URL.Query())
		_, _ = io.WriteString(w, body)
	})
	return fixedClock(t, c), rec
}

func TestMonitorSpan_TrailingUTCDaysIncludingToday(t *testing.T) {
	c, _ := monitorClient(t, `{}`)
	start, end, asOf, err := c.MonitorSpan(30)
	if err != nil {
		t.Fatalf("MonitorSpan: %v", err)
	}
	// Clock pinned to 2026-03-15T12:00Z: 30 days today inclusive is Feb 14 .. Mar 15.
	if want := time.Date(2026, 2, 14, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("start = %s, want %s", start, want)
	}
	if want := time.Date(2026, 3, 15, 23, 59, 59, int(999*time.Millisecond), time.UTC); !end.Equal(want) {
		t.Errorf("end = %s, want %s", end, want)
	}
	if want := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC); !asOf.Equal(want) {
		t.Errorf("asOf = %s, want the pinned clock %s", asOf, want)
	}
	if _, _, _, err := c.MonitorSpan(0); err == nil {
		t.Error("a zero-day span was accepted")
	}
}

func TestGetEmailCounters_MapsSevenCountersAndSendsTheSpan(t *testing.T) {
	c, rec := monitorClient(t, statsBody(t, `[4242]`,
		`{"sent":1000,"delivered":950,"open":400,"click":80,"bounce":50,"unsubscribed":7,"spamreport":2,"hardbounced":30}`))
	start, end, _, _ := c.MonitorSpan(7)
	got, err := c.GetEmailCounters(context.Background(), "4242", start, end)
	if err != nil {
		t.Fatalf("GetEmailCounters: %v", err)
	}
	want := EmailCounters{Sent: 1000, Delivered: 950, Opens: 400, Clicks: 80, Bounces: 50, Unsubscribes: 7, SpamReports: 2}
	if *got != want {
		t.Errorf("counters = %+v, want %+v", *got, want)
	}
	q, calls := rec.get()
	if calls != 1 {
		t.Fatalf("%d requests, want 1", calls)
	}
	if q.Get("emailIds") != "4242" || len(q["emailIds"]) != 1 {
		t.Errorf("emailIds = %v, want exactly the one id", q["emailIds"])
	}
	if q.Get("startTimestamp") != "2026-03-09T00:00:00Z" || q.Get("endTimestamp") != "2026-03-15T23:59:59.999Z" {
		t.Errorf("span = %s .. %s", q.Get("startTimestamp"), q.Get("endTimestamp"))
	}
}

// An explicit null counter is refused on BOTH reads that share the decode — decoded into an
// integer it would be an authoritative 0. An absent counter keeps its omitted-zero meaning.
func TestStatistics_ExplicitNullCounterIsRefused(t *testing.T) {
	body := statsBody(t, `[4242]`, `{"sent":1000,"delivered":950,"bounce":null}`)
	c, _ := monitorClient(t, body)
	start, end, _, _ := c.MonitorSpan(30)
	if _, err := c.GetEmailCounters(context.Background(), "4242", start, end); !errors.Is(err, ErrNullCounter) {
		t.Errorf("GetEmailCounters err = %v, want ErrNullCounter", err)
	}
	if _, err := c.GetEmailMetrics(context.Background(), "4242", model.MetricsWindowLast30Days); !errors.Is(err, ErrNullCounter) {
		t.Errorf("GetEmailMetrics err = %v, want ErrNullCounter", err)
	}
}

// The raw bytes are checked before decoding: a repeated key decodes to its last value without
// error, so `"sent":1000,"sent":0` would read as whichever the decoder kept.
func TestStatistics_UntrustworthyBytesAreRefusedBeforeDecoding(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate counter":        `{"emails":[4242],"aggregate":{"counters":{"sent":1000,"sent":0}}}`,
		"duplicate emails field":   `{"emails":[9999],"emails":[4242],"aggregate":{"counters":{"sent":1}}}`,
		"case-folded duplicate":    `{"emails":[4242],"aggregate":{"counters":{"sent":1}},"Aggregate":{"counters":{"sent":2}}}`,
		"unpaired surrogate":       `{"emails":[4242],"aggregate":{"counters":{"sent":1,"\uD800x":1}}}`,
		"malformed UTF-8 in a key": "{\"emails\":[4242],\"aggregate\":{\"counters\":{\"sent\":1,\"x\xff\":1}}}",
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := monitorClient(t, body)
			start, end, _, _ := c.MonitorSpan(30)
			_, err := c.GetEmailCounters(context.Background(), "4242", start, end)
			if !errors.Is(err, identityjson.ErrUntrustworthy) {
				t.Fatalf("err = %v, want identityjson.ErrUntrustworthy", err)
			}
		})
	}
}

// The raw check is scoped to how each level DECODES. deviceBreakdown, ratios and the other open
// maps are keyed exactly by encoding/json, so a case-distinct pair there is two entries, not an
// ambiguity, and must not fail a working read; an EXACT duplicate in the same map still is.
func TestStatistics_OpenMapsAreCheckedForExactDuplicatesOnly(t *testing.T) {
	caseDistinct := `{"emails":[4242],"aggregate":{"counters":{"sent":10,"delivered":10},` +
		`"deviceBreakdown":{"Mobile":{"open":1},"mobile":{"open":2}},"ratios":{"OpenRatio":1,"openratio":1}}}`
	exactDup := `{"emails":[4242],"aggregate":{"counters":{"sent":10,"delivered":10},` +
		`"deviceBreakdown":{"mobile":{"open":1},"mobile":{"open":2}}}}`
	for name, tc := range map[string]struct {
		body   string
		refuse bool
	}{"case-distinct keys in an open map": {caseDistinct, false}, "exact duplicate in an open map": {exactDup, true}} {
		t.Run(name, func(t *testing.T) {
			c, _ := monitorClient(t, tc.body)
			start, end, _, _ := c.MonitorSpan(30)
			_, err := c.GetEmailCounters(context.Background(), "4242", start, end)
			if _, merr := c.GetEmailMetrics(context.Background(), "4242", model.MetricsWindowLast30Days); (merr != nil) != (err != nil) {
				t.Errorf("the two reads disagree: counters %v, metrics %v", err, merr)
			}
			if tc.refuse != errors.Is(err, identityjson.ErrUntrustworthy) || (!tc.refuse && err != nil) {
				t.Fatalf("err = %v, refuse=%v", err, tc.refuse)
			}
		})
	}
}

// The rename guard watches what the CALLER reads. The monitor reads spamreport, so a response
// missing it while carrying an unrecognised key is a rename to the monitor — and NOT to the
// per-campaign read, which never looks spamreport up.
func TestStatistics_RenameGuardWatchesWhatEachReadUses(t *testing.T) {
	body := statsBody(t, `[4242]`, `{"sent":1,"delivered":1,"open":1,"click":1,"bounce":1,"unsubscribed":1,"spamReports":1}`)
	c, _ := monitorClient(t, body)
	start, end, _, _ := c.MonitorSpan(30)
	if _, err := c.GetEmailCounters(context.Background(), "4242", start, end); !errors.Is(err, ErrRenamedCounter) {
		t.Errorf("GetEmailCounters err = %v, want ErrRenamedCounter", err)
	}
	if _, err := c.GetEmailMetrics(context.Background(), "4242", model.MetricsWindowLast30Days); err != nil {
		t.Errorf("GetEmailMetrics err = %v, want success: it does not read spamreport", err)
	}
}

func TestGetEmailCounters_NoSendInTheSpanIsTheSentinelNotZeros(t *testing.T) {
	c, _ := monitorClient(t, statsBody(t, `[]`, `{}`))
	start, end, _, _ := c.MonitorSpan(30)
	got, err := c.GetEmailCounters(context.Background(), "4242", start, end)
	if !errors.Is(err, ErrNoSentEmailInWindow) || got != nil {
		t.Fatalf("got %+v, %v; want nil, ErrNoSentEmailInWindow", got, err)
	}
}

// A malformed id or an empty span is refused before any request.
func TestGetEmailCounters_RefusesBadInputBeforeSending(t *testing.T) {
	c, rec := monitorClient(t, statsBody(t, `[4242]`, fullCounters))
	start, end, _, _ := c.MonitorSpan(30)
	if _, err := c.GetEmailCounters(context.Background(), "04242", start, end); err == nil {
		t.Error("a non-canonical id was accepted")
	}
	if _, err := c.GetEmailCounters(context.Background(), "4242", end, start); err == nil {
		t.Error("an inverted span was accepted")
	}
	if _, calls := rec.get(); calls != 0 {
		t.Errorf("%d requests sent for refused input", calls)
	}
	if ValidateEmailID("4242") != nil || ValidateEmailID("abc") == nil || ValidateEmailID("") == nil {
		t.Error("ValidateEmailID disagrees with the canonical-positive-integer rule")
	}
}

// Token-info is identity evidence: a doubled hubId is refused rather than resolved to one.
func TestAuthenticatedPortalID_RefusesADuplicateHubID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"hubId":8112310,"hubId":999}`)
	})
	if _, err := c.AuthenticatedPortalID(context.Background()); !errors.Is(err, identityjson.ErrUntrustworthy) {
		t.Fatalf("err = %v, want identityjson.ErrUntrustworthy", err)
	}
}
