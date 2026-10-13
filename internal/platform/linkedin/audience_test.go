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

// audienceStub answers each statistics request by the MEMBER_* pivot it names, recording every
// raw query. A pivot with no body configured answers `{"elements":[]}`.
type audienceStub struct {
	mu      sync.Mutex
	queries []string
	bodies  map[string]string // MEMBER_* pivot -> body
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
		pivot := ""
		for _, p := range audiencePivots {
			if strings.Contains(r.URL.RawQuery, "pivots=List(CAMPAIGN,"+p.pivot+")") {
				pivot = p.pivot
			}
		}
		if pivot == "" {
			t.Errorf("request names no known pivot: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if b, ok := s.bodies[pivot]; ok {
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
		// Two of the project's campaigns, one value shared (summed), one absent counter (0),
		// pivotValues in either order.
		"MEMBER_JOB_FUNCTION": `{"elements":[
			{"pivotValues":["urn:li:sponsoredCampaign:111","urn:li:function:8"],"impressions":100,"clicks":4,"costInUsd":"1.50"},
			{"pivotValues":["urn:li:function:8","urn:li:sponsoredCampaign:222"],"impressions":50,"clicks":2,"costInUsd":"0.25"},
			{"pivotValues":["urn:li:sponsoredCampaign:111","urn:li:function:3"],"impressions":200}
		],"paging":{"count":10,"start":0,"links":[]}}`,
		"MEMBER_COMPANY_SIZE": `{"elements":[
			{"pivotValues":["urn:li:sponsoredCampaign:111","SIZE_11_TO_50"],"impressions":9,"clicks":1,"costInUsd":"0.10"}
		]}`,
		"MEMBER_COUNTRY_V2": `{"elements":[
			{"pivotValues":["urn:li:sponsoredCampaign:222","urn:li:geo:103644278"],"impressions":70,"clicks":7,"costInUsd":"2"}
		]}`,
	}}
	c := newAudienceClient(t, s)
	got, err := c.GetAudienceInsights(context.Background(), model.MetricsWindowLast7Days, []string{"111", "222", "111"})
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

	if n := s.calls(); n != len(audiencePivots) {
		t.Fatalf("requests = %d, want one per pivot (%d)", n, len(audiencePivots))
	}
	facet := "campaigns=List(" + url.QueryEscape("urn:li:sponsoredCampaign:111") + "," + url.QueryEscape("urn:li:sponsoredCampaign:222") + ")"
	for i, q := range s.queries {
		for _, frag := range []string{
			"q=statistics",
			"pivots=List(CAMPAIGN," + audiencePivots[i].pivot + ")",
			"timeGranularity=ALL",
			"dateRange=(start:(day:7,month:10,year:2026),end:(day:13,month:10,year:2026))",
			facet, // duplicates collapsed, order kept
			"fields=impressions,clicks,costInUsd",
		} {
			if !strings.Contains(q, frag) {
				t.Errorf("request %d missing %q: %s", i, frag, q)
			}
		}
		// The campaign facet IS the tenant boundary; an accounts facet could only widen it.
		if strings.Contains(q, "accounts=") {
			t.Errorf("request %d carries an accounts facet: %s", i, q)
		}
	}
}

// Every guard below refuses the WHOLE read; none may echo the offending upstream value.
func TestGetAudienceInsights_DecodeGuardsFailClosed(t *testing.T) {
	el := func(pv, rest string) string {
		return `{"elements":[{"pivotValues":` + pv + rest + `}]}`
	}
	ok := `["urn:li:sponsoredCampaign:111","urn:li:function:8"]`
	many := make([]string, audienceMaxElements)
	for i := range many {
		many[i] = fmt.Sprintf(`{"pivotValues":["urn:li:sponsoredCampaign:111","urn:li:function:%d"],"impressions":1}`, i+1)
	}
	for _, tc := range []struct {
		name, body, leak string
	}{
		{"foreign campaign", el(`["urn:li:sponsoredCampaign:999777","urn:li:function:8"]`, `,"impressions":1`), "999777"},
		{"duplicate campaign and value", `{"elements":[{"pivotValues":` + ok + `,"impressions":1},{"pivotValues":` + ok + `,"impressions":1}]}`, ""},
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
		{"numeric cost", el(ok, `,"costInUsd":1.5`), ""},
		{"malformed cost", el(ok, `,"costInUsd":"1e9x"`), "1e9x"},
		{"pivotValues missing", `{"elements":[{"impressions":1}]}`, ""},
		{"one pivot value", el(`["urn:li:sponsoredCampaign:111"]`, ``), ""},
		{"two campaign URNs", el(`["urn:li:sponsoredCampaign:111","urn:li:sponsoredCampaign:111"]`, ``), ""},
		{"wrong URN type for the pivot", el(`["urn:li:sponsoredCampaign:111","urn:li:seniority:4"]`, ``), "seniority:4"},
		{"bare URN prefix", el(`["urn:li:sponsoredCampaign:111","urn:li:function:"]`, ``), ""},
		{"value outside the charset", el(`["urn:li:sponsoredCampaign:111","<script>x"]`, ``), "<script>"},
		{"next page offered", `{"elements":[],"paging":{"links":[{"rel":"next","href":"/adAnalytics?start=10"}]}}`, ""},
		{"at the element ceiling", `{"elements":[` + strings.Join(many, ",") + `]}`, ""},
		{"malformed JSON", `{"elements":[`, ""},
		{"overflow when summed", `{"elements":[{"pivotValues":` + ok + `,"impressions":9223372036854775807},{"pivotValues":["urn:li:sponsoredCampaign:222","urn:li:function:8"],"impressions":1}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{bodies: map[string]string{"MEMBER_JOB_FUNCTION": tc.body}}
			c := newAudienceClient(t, s)
			got, err := c.GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"111", "222"})
			if err == nil {
				t.Fatalf("accepted; buckets = %+v", got)
			}
			if got != nil {
				t.Errorf("a refused read returned buckets: %+v", got)
			}
			if tc.leak != "" && strings.Contains(err.Error(), tc.leak) {
				t.Errorf("error echoes upstream content %q: %v", tc.leak, err)
			}
			// The read stops at the failing pivot: later pivots are never requested.
			if n := s.calls(); n != 1 {
				t.Errorf("requests = %d, want 1 (fail on the first pivot)", n)
			}
		})
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
			_, err := c.GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"111"})
			if err == nil {
				t.Fatal("accepted")
			}
			if got := errors.Is(err, ErrCredentialsExpired); got != tc.wantExpired {
				t.Errorf("errors.Is(ErrCredentialsExpired) = %v, want %v (%v)", got, tc.wantExpired, err)
			}
			if strings.Contains(err.Error(), "upstream-secret-text") || strings.Contains(err.Error(), "tok-secret-abc") {
				t.Errorf("error leaks upstream body or token: %v", err)
			}
			if n := s.calls(); n != 1 {
				t.Errorf("requests = %d, want exactly 1 (no retry)", n)
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
			if _, err := c.GetAudienceInsights(context.Background(), tc.window, tc.ids); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if n := s.calls(); n != 0 {
				t.Errorf("LinkedIn contacted %d time(s)", n)
			}
		})
	}
	// Exactly at the cap is served (and duplicates do not count toward it).
	s := &audienceStub{}
	c := newAudienceClient(t, s)
	if _, err := c.GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, append(append([]string{}, atCap...), atCap[0])); err != nil {
		t.Errorf("at the cap: %v", err)
	}
}
