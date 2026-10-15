// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// audienceClock is 2026-10-13 UTC; last_7_days is therefore 2026-10-07..2026-10-13.
var audienceClock = func() time.Time { return time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC) }

// audienceStub answers each Analytics request by the MEMBER_* pivot and the single campaign it
// names, recording every raw query. Bodies are keyed "PIVOT/campaignID"; an unconfigured pair
// answers `{"elements":[]}`.
type audienceStub struct {
	mu      sync.Mutex
	queries []string
	bodies  map[string]string // "MEMBER_*/<campaign id>" -> body
	status  int
}

func (s *audienceStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.RawQuery)
		s.mu.Unlock()
		if r.URL.Path != "/adAnalytics" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, `{"message":"upstream-secret-text"}`)
			return
		}
		q := r.URL.RawQuery
		pivot := ""
		for _, p := range audiencePivots {
			if strings.Contains(q, "&pivot="+p.pivot+"&") {
				pivot = p.pivot
			}
		}
		if pivot == "" {
			t.Errorf("request names no known pivot: %s", q)
		}
		campaign := ""
		if i := strings.Index(q, "campaigns=List("+url.QueryEscape(campaignURNPrefix)); i >= 0 {
			rest := q[i+len("campaigns=List("+url.QueryEscape(campaignURNPrefix)):]
			campaign = rest[:strings.Index(rest, ")")]
		}
		w.Header().Set("Content-Type", "application/json")
		if b, ok := s.bodies[pivot+"/"+campaign]; ok {
			_, _ = io.WriteString(w, b)
			return
		}
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}
}

func (s *audienceStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

func newAudienceClient(t *testing.T, s *audienceStub) *Client {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL), WithClock(audienceClock))
}

func TestGetAudienceInsights_RequestShapeAndAggregation(t *testing.T) {
	s := &audienceStub{bodies: map[string]string{
		// Two of the project's campaigns, one value shared across them (summed), one absent
		// counter (0).
		"MEMBER_JOB_FUNCTION/111": `{"elements":[
			{"pivotValues":["urn:li:function:8"],"impressions":100,"clicks":4,"costInUsd":"1.50"},
			{"pivotValues":["urn:li:function:3"],"impressions":200}
		],"paging":{"count":10,"start":0,"links":[]}}`,
		"MEMBER_JOB_FUNCTION/222": `{"elements":[
			{"pivotValues":["urn:li:function:8"],"impressions":50,"clicks":2,"costInUsd":"0.25"}
		]}`,
		"MEMBER_COMPANY_SIZE/111": `{"elements":[
			{"pivotValues":["SIZE_11_TO_50"],"impressions":9,"clicks":1,"costInUsd":"0.10"}
		]}`,
		"MEMBER_COUNTRY_V2/222": `{"elements":[
			{"pivotValues":["urn:li:geo:103644278"],"impressions":70,"clicks":7,"costInUsd":"2"}
		]}`,
	}}
	c := newAudienceClient(t, s)
	got, err := c.GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast7Days, []string{"111", "222", "111"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if got.Window != model.MetricsWindowLast7Days {
		t.Errorf("window = %q", got.Window)
	}
	want := []AudienceBucket{
		{Dimension: "job_function", Value: "urn:li:function:3", Impressions: 200},
		{Dimension: "job_function", Value: "urn:li:function:8", Impressions: 150, Clicks: 6, CostMicros: 1_750_000, Ctr: 6.0 / 150},
		{Dimension: "company_size", Value: "SIZE_11_TO_50", Impressions: 9, Clicks: 1, CostMicros: 100_000, Ctr: 1.0 / 9},
		{Dimension: "country", Value: "urn:li:geo:103644278", Impressions: 70, Clicks: 7, CostMicros: 2_000_000, Ctr: 0.1},
	}
	if fmt.Sprint(got.Buckets) != fmt.Sprint(want) {
		t.Errorf("buckets =\n%+v\nwant\n%+v", got.Buckets, want)
	}

	// One request per (pivot, distinct campaign): duplicates collapse.
	if n := s.calls(); n != len(audiencePivots)*2 {
		t.Fatalf("requests = %d, want one per pivot per campaign (%d)", n, len(audiencePivots)*2)
	}
	seen := map[string]bool{}
	for i, q := range s.queries {
		for _, frag := range []string{
			"q=analytics",
			"timeGranularity=ALL",
			"dateRange=(start:(day:7,month:10,year:2026),end:(day:13,month:10,year:2026))",
			"accounts=List(" + url.QueryEscape("urn:li:sponsoredAccount:512345678") + ")",
			"fields=impressions,clicks,costInUsd,pivotValues",
		} {
			if !strings.Contains(q, frag) {
				t.Errorf("request %d missing %q: %s", i, frag, q)
			}
		}
		// The Statistics finder documents no MEMBER_* pivot; nothing multi-pivot may be sent.
		if strings.Contains(q, "q=statistics") || strings.Contains(q, "pivots=") {
			t.Errorf("request %d uses the multi-pivot finder: %s", i, q)
		}
		// Exactly one accounts facet and exactly one campaign URN: attribution comes from the
		// request, because an Analytics element carries no campaign.
		if n := strings.Count(q, "accounts="); n != 1 {
			t.Errorf("request %d carries %d accounts facets, want 1: %s", i, n, q)
		}
		if n := strings.Count(q, url.QueryEscape(campaignURNPrefix)); n != 1 {
			t.Errorf("request %d names %d campaign URNs, want exactly 1: %s", i, n, q)
		}
		for _, p := range audiencePivots {
			for _, id := range []string{"111", "222"} {
				if strings.Contains(q, "&pivot="+p.pivot+"&") && strings.Contains(q, "campaigns=List("+url.QueryEscape(campaignURNPrefix+id)+")") {
					seen[p.pivot+"/"+id] = true
				}
			}
		}
	}
	if len(seen) != len(audiencePivots)*2 {
		t.Errorf("distinct (pivot, campaign) requests = %d, want %d: %v", len(seen), len(audiencePivots)*2, seen)
	}
}

// Every guard below refuses the WHOLE read; none may echo the offending upstream value.
func TestGetAudienceInsights_DecodeGuardsFailClosed(t *testing.T) {
	el := func(pv, rest string) string {
		return `{"elements":[{"pivotValues":` + pv + rest + `}]}`
	}
	ok := `["urn:li:function:8"]`
	many := make([]string, audienceMaxElements)
	for i := range many {
		many[i] = fmt.Sprintf(`{"pivotValues":["urn:li:function:%d"],"impressions":1}`, i+1)
	}
	for _, tc := range []struct {
		name, body, leak string
	}{
		{"campaign URN as the value", el(`["urn:li:sponsoredCampaign:999777"]`, `,"impressions":1`), "999777"},
		{"two pivot values", el(`["urn:li:sponsoredCampaign:111","urn:li:function:8"]`, `,"impressions":1`), ""},
		{"two demographic values", el(`["urn:li:function:8","urn:li:function:9"]`, `,"impressions":1`), ""},
		{"empty pivot values", el(`[]`, `,"impressions":1`), ""},
		{"duplicate value", `{"elements":[{"pivotValues":` + ok + `,"impressions":1},{"pivotValues":` + ok + `,"impressions":1}]}`, ""},
		{"missing elements", `{}`, ""},
		{"null elements", `{"elements":null}`, ""},
		{"repeated elements key", `{"elements":[{"pivotValues":` + ok + `,"impressions":5}],"elements":[]}`, ""},
		{"case-folded repeated key", `{"elements":[{"pivotValues":` + ok + `,"impressions":5}],"Elements":[]}`, ""},
		{"repeated counter key", el(ok, `,"impressions":5,"impressions":1`), ""},
		{"explicit null impressions", el(ok, `,"impressions":null`), ""},
		{"explicit null clicks", el(ok, `,"clicks":null`), ""},
		{"explicit null cost", el(ok, `,"costInUsd":null`), ""},
		{"string impressions", el(ok, `,"impressions":"12"`), ""},
		{"fractional impressions", el(ok, `,"impressions":1.5`), ""},
		{"negative clicks", el(ok, `,"clicks":-1`), ""},
		{"clicks with absent impressions", el(ok, `,"clicks":3`), ""},
		{"clicks with zero impressions", el(ok, `,"impressions":0,"clicks":3,"costInUsd":"1.00"`), ""},
		{"numeric cost", el(ok, `,"costInUsd":1.5`), ""},
		{"malformed cost", el(ok, `,"costInUsd":"1e9x"`), "1e9x"},
		{"pivotValues missing", `{"elements":[{"impressions":1}]}`, ""},
		{"wrong URN type for the pivot", el(`["urn:li:seniority:4"]`, ``), "seniority:4"},
		{"bare URN prefix", el(`["urn:li:function:"]`, ``), ""},
		{"value outside the charset", el(`["<script>x"]`, ``), "<script>"},
		{"next page offered", `{"elements":[],"paging":{"links":[{"rel":"next","href":"/adAnalytics?start=10"}]}}`, ""},
		{"at the element ceiling", `{"elements":[` + strings.Join(many, ",") + `]}`, ""},
		{"malformed JSON", `{"elements":[`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{bodies: map[string]string{"MEMBER_JOB_FUNCTION/111": tc.body}}
			c := newAudienceClient(t, s)
			got, err := c.GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, []string{"111", "222"})
			if err == nil {
				t.Fatalf("accepted; buckets = %+v", got)
			}
			if got != nil {
				t.Errorf("a refused read returned buckets: %+v", got)
			}
			if tc.leak != "" && strings.Contains(err.Error(), tc.leak) {
				t.Errorf("error echoes upstream content %q: %v", tc.leak, err)
			}
		})
	}
	// company_size has no documented URN type, so only the explicit campaign-URN refusal stands
	// between a CAMPAIGN-grouped answer and a bucket keyed by a campaign.
	cs := &audienceStub{bodies: map[string]string{"MEMBER_COMPANY_SIZE/111": el(`["urn:li:sponsoredCampaign:111"]`, `,"impressions":1`)}}
	if got, err := newAudienceClient(t, cs).GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, []string{"111"}); err == nil {
		t.Errorf("a campaign URN as a company_size value was accepted: %+v", got)
	}
	// Each campaign's counters fit int64, but their sum across campaigns does not.
	s := &audienceStub{bodies: map[string]string{
		"MEMBER_JOB_FUNCTION/111": el(ok, `,"impressions":9223372036854775807`),
		"MEMBER_JOB_FUNCTION/222": el(ok, `,"impressions":1`),
	}}
	if got, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, []string{"111", "222"}); err == nil {
		t.Errorf("overflow across campaigns accepted: %+v", got)
	}
}

// The request count is bounded: at the campaign cap a read issues exactly
// len(audiencePivots)*MaxAudienceCampaigns requests, never more than audienceConcurrency at once.
func TestGetAudienceInsights_RequestCountAndConcurrencyBounded(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak, total := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		total++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(Credentials{AccessToken: "tok"}, RuntimeConfig{}, WithBaseURL(srv.URL), WithClock(audienceClock))
	ids := make([]string, MaxAudienceCampaigns)
	for i := range ids {
		ids[i] = fmt.Sprint(1000 + i)
	}
	if _, err := c.GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, ids); err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if total != len(audiencePivots)*MaxAudienceCampaigns {
		t.Errorf("requests = %d, want %d", total, len(audiencePivots)*MaxAudienceCampaigns)
	}
	if peak > audienceConcurrency {
		t.Errorf("peak concurrency = %d, want at most %d", peak, audienceConcurrency)
	}
	if MaxAudienceCampaigns*len(audiencePivots) > 50 {
		t.Errorf("a read may issue %d requests; the 20s budget was sized for at most 50", MaxAudienceCampaigns*len(audiencePivots))
	}
}

// A non-2xx answer is an apiError with no upstream body; a 401 is the credentials-expired
// sentinel the dispatcher re-tags; a 429 is not retried.
func TestGetAudienceInsights_UpstreamStatuses(t *testing.T) {
	for _, tc := range []struct {
		status      int
		wantExpired bool
	}{
		{http.StatusInternalServerError, false},
		{http.StatusTooManyRequests, false},
		{http.StatusForbidden, false},
		{http.StatusUnauthorized, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			s := &audienceStub{status: tc.status}
			c := newAudienceClient(t, s)
			_, err := c.GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, []string{"111"})
			if err == nil {
				t.Fatal("accepted")
			}
			if got := errors.Is(err, ErrCredentialsExpired); got != tc.wantExpired {
				t.Errorf("errors.Is(ErrCredentialsExpired) = %v, want %v (%v)", got, tc.wantExpired, err)
			}
			if strings.Contains(err.Error(), "upstream-secret-text") || strings.Contains(err.Error(), "tok-secret-abc") {
				t.Errorf("error leaks upstream body or token: %v", err)
			}
			// One request per pivot at most (no retry); the first failure cancels the rest.
			if n := s.calls(); n < 1 || n > len(audiencePivots) {
				t.Errorf("requests = %d, want 1..%d (no retry)", n, len(audiencePivots))
			}
		})
	}
}

// Scope and window faults are refused before any request.
func TestGetAudienceInsights_LocalRefusalsNeverContactLinkedIn(t *testing.T) {
	tooMany := make([]string, MaxAudienceCampaigns+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(1000 + i)
	}
	atCap := tooMany[:MaxAudienceCampaigns]
	for _, tc := range []struct {
		name   string
		window model.MetricsWindow
		ids    []string
		want   error
	}{
		{"empty scope", model.MetricsWindowLast30Days, nil, ErrAudienceScopeInvalid},
		{"non-digit id", model.MetricsWindowLast30Days, []string{"12a"}, ErrAudienceScopeInvalid},
		{"urn id", model.MetricsWindowLast30Days, []string{"urn:li:sponsoredCampaign:1"}, ErrAudienceScopeInvalid},
		{"leading zero", model.MetricsWindowLast30Days, []string{"0123"}, ErrAudienceScopeInvalid},
		{"padded id", model.MetricsWindowLast30Days, []string{" 123"}, ErrAudienceScopeInvalid},
		{"over the cap", model.MetricsWindowLast30Days, tooMany, ErrAudienceScopeTooLarge},
		{"yesterday", model.MetricsWindowYesterday, []string{"111"}, ErrUnsupportedWindow},
		{"last_14_days", model.MetricsWindowLast14Days, []string{"111"}, ErrUnsupportedWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{}
			c := newAudienceClient(t, s)
			if _, err := c.GetAudienceInsights(context.Background(), "512345678", tc.window, tc.ids); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if n := s.calls(); n != 0 {
				t.Errorf("LinkedIn contacted %d time(s)", n)
			}
		})
	}
	// The bound account is interpolated into the accounts facet, so a malformed one is refused
	// before any request.
	for _, bad := range []string{"", "urn:li:sponsoredAccount:1", "12 3", "abc"} {
		s := &audienceStub{}
		c := newAudienceClient(t, s)
		if _, err := c.GetAudienceInsights(context.Background(), bad, model.MetricsWindowLast30Days, []string{"111"}); !errors.Is(err, ErrInvalidAccountID) {
			t.Errorf("account %q: err = %v, want ErrInvalidAccountID", bad, err)
		}
		if n := s.calls(); n != 0 {
			t.Errorf("account %q: LinkedIn contacted %d time(s)", bad, n)
		}
	}
	// Exactly at the cap is served (and duplicates do not count toward it).
	s := &audienceStub{}
	c := newAudienceClient(t, s)
	if _, err := c.GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, append(append([]string{}, atCap...), atCap[0])); err != nil {
		t.Errorf("at the cap: %v", err)
	}
}

// A failing request must cancel the read: requests already in flight see their contexts
// cancelled, requests still queued behind audienceConcurrency never reach LinkedIn, and no partial
// result is returned. Two campaigns give 10 requests, so 5 are queued when the first 5 start.
func TestGetAudienceInsights_FirstFailureCancelsInFlightAndQueued(t *testing.T) {
	const safety = 5 * time.Second
	var mu sync.Mutex
	arrived, cancelled := 0, 0
	allOthersWaiting := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failing := strings.Contains(r.URL.RawQuery, "&pivot=MEMBER_JOB_FUNCTION&") &&
			strings.Contains(r.URL.RawQuery, url.QueryEscape(campaignURNPrefix+"111"))
		mu.Lock()
		arrived++
		if arrived == audienceConcurrency {
			close(allOthersWaiting)
		}
		mu.Unlock()
		if failing {
			// Fail only once every other in-flight slot is occupied by a waiting request, so
			// the cancellation has something to cancel.
			select {
			case <-allOthersWaiting:
			case <-time.After(safety):
				t.Error("the other in-flight requests never arrived")
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		select {
		case <-r.Context().Done():
			mu.Lock()
			cancelled++
			mu.Unlock()
		case <-time.After(safety):
			t.Errorf("an in-flight request was not cancelled after another failed: %s", r.URL.RawQuery)
			_, _ = io.WriteString(w, `{"elements":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(Credentials{AccessToken: "tok"}, RuntimeConfig{}, WithBaseURL(srv.URL), WithClock(audienceClock))

	begun := time.Now()
	got, err := c.GetAudienceInsights(context.Background(), "512345678", model.MetricsWindowLast30Days, []string{"111", "222"})
	if err == nil {
		t.Fatalf("a read with a failed request succeeded: %+v", got)
	}
	if got != nil {
		t.Errorf("a failed read returned a partial result: %+v", got)
	}
	// The read must report the failure that CAUSED the cancellation — the 500 — not a sibling's
	// cancellation or any other error.
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusInternalServerError {
		t.Errorf("error = %v, want the failing request's *apiError with status 500", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("the read reported a sibling's cancellation instead of the failure that caused it: %v", err)
	}
	if took := time.Since(begun); took >= safety {
		t.Errorf("the read took %v: in-flight requests were waited out, not cancelled", took)
	}
	// Server-side cancellation is observed asynchronously after the client gives up.
	deadline := time.Now().Add(safety)
	for {
		mu.Lock()
		a, cn := arrived, cancelled
		mu.Unlock()
		if cn == audienceConcurrency-1 || time.Now().After(deadline) {
			if a != audienceConcurrency {
				t.Errorf("%d requests reached LinkedIn, want exactly %d: queued requests must never be sent", a, audienceConcurrency)
			}
			if cn != audienceConcurrency-1 {
				t.Errorf("%d in-flight requests were cancelled, want %d", cn, audienceConcurrency-1)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
