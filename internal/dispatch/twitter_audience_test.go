// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// twitterAudienceServer stubs X for the audience read on account "acc1": the account (New York,
// USD), job creation, an immediately-finished status read, and a results file giving campaign
// "c1" one segment per segmentation. Every request is recorded under mu before it is answered.
type twitterAudienceServer struct {
	srv *httptest.Server
	// tz is the account's timezone.
	tz string

	mu     sync.Mutex
	reqs   []string
	posts  []string // segmentation_type of each job POST, in order
	starts []string
	ends   []string
	jobSeg map[string]string
	next   int
}

func newTwitterAudienceServer(t *testing.T, tz string) *twitterAudienceServer {
	t.Helper()
	s := &twitterAudienceServer{tz: tz, jobSeg: map[string]string{}, next: 700}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/12/accounts/acc1":
			_, _ = fmt.Fprintf(w, `{"data":{"id":"acc1","timezone":%q,"currency":"USD"}}`, s.tz)
		case r.Method == http.MethodPost && r.URL.Path == "/12/stats/jobs/accounts/acc1":
			q := r.URL.Query()
			id := strconv.Itoa(s.next)
			s.next++
			s.jobSeg[id] = q.Get("segmentation_type")
			s.posts = append(s.posts, q.Get("segmentation_type"))
			s.starts = append(s.starts, q.Get("start_time"))
			s.ends = append(s.ends, q.Get("end_time"))
			_, _ = fmt.Fprintf(w, `{"data":{"id_str":%q,"status":"PROCESSING"}}`, id)
		case r.Method == http.MethodGet && r.URL.Path == "/12/stats/jobs/accounts/acc1":
			ids := strings.Split(r.URL.Query().Get("job_ids"), ",")
			els := make([]string, 0, len(ids))
			for _, id := range ids {
				els = append(els, fmt.Sprintf(`{"id_str":%q,"status":"SUCCESS","url":%q}`, id, s.srv.URL+"/files/"+id))
			}
			_, _ = fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(els, ","))
		case strings.HasPrefix(r.URL.Path, "/files/"):
			seg := s.jobSeg[strings.TrimPrefix(r.URL.Path, "/files/")]
			name := map[string]string{"AGE": "25-34", "GENDER": "Female", "PLATFORMS": "Android"}[seg]
			body := fmt.Sprintf(`{"data":[{"id":"c1","id_data":[{"segment":{"segment_name":%q},"metrics":{"impressions":[1000],"clicks":[40],"billed_charge_local_micro":[25000000]}}]}]}`, name)
			var b bytes.Buffer
			zw := gzip.NewWriter(&b)
			_, _ = zw.Write([]byte(body))
			_ = zw.Close()
			_, _ = w.Write(b.Bytes())
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *twitterAudienceServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func (s *twitterAudienceServer) opts(now time.Time) []twitter.Option {
	return []twitter.Option{twitter.WithBaseURL(s.srv.URL), twitter.WithWriteDelay(0), twitter.WithClock(func() time.Time { return now })}
}

func twitterScope(ids ...string) []model.ProjectCampaignScope {
	out := make([]model.ProjectCampaignScope, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.ProjectCampaignScope{PlatformCampaignID: id})
	}
	return out
}

var twitterAudienceNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func TestTwitter_ReadTwitterAudienceInsights_MapsBucketsAndKeepsRequestWindow(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	s := newTwitterAudienceServer(t, "America/New_York")
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, s.opts(twitterAudienceNow)...)

	ai, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowYesterday, twitterScope("c1"))
	if err != nil {
		t.Fatalf("ReadTwitterAudienceInsights: %v", err)
	}
	if ai.Window != model.MetricsWindowYesterday || ai.Currency != "USD" {
		t.Errorf("window/currency = %q/%q", ai.Window, ai.Currency)
	}
	want := []model.TwitterAudienceBucket{
		{Dimension: model.TwitterAudienceDimensionAge, Value: "25-34", Impressions: 1000, Clicks: 40, CostMicros: 25_000_000, Ctr: 0.04},
		{Dimension: model.TwitterAudienceDimensionGender, Value: "Female", Impressions: 1000, Clicks: 40, CostMicros: 25_000_000, Ctr: 0.04},
		{Dimension: model.TwitterAudienceDimensionPlatform, Value: "Android", Impressions: 1000, Clicks: 40, CostMicros: 25_000_000, Ctr: 0.04},
	}
	if len(ai.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
	for i := range want {
		if ai.Buckets[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, ai.Buckets[i], want[i])
		}
	}
	// The model dimension tokens must be the client's; a drift would publish an enum value the
	// design does not declare.
	if model.TwitterAudienceDimensionAge != twitter.AudienceDimensionAge || model.TwitterAudienceDimensionGender != twitter.AudienceDimensionGender ||
		model.TwitterAudienceDimensionPlatform != twitter.AudienceDimensionPlatform {
		t.Error("model and client dimension tokens diverged")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Join(s.posts, ",") != "AGE,GENDER,PLATFORMS" {
		t.Errorf("segmentations = %v", s.posts)
	}
	// Yesterday on the ACCOUNT's (New York, EDT) calendar: Oct 4 04:00Z to Oct 5 04:00Z.
	for i := range s.starts {
		if s.starts[i] != "2026-10-04T04:00:00Z" || s.ends[i] != "2026-10-05T04:00:00Z" {
			t.Errorf("job %d window = [%s, %s)", i, s.starts[i], s.ends[i])
		}
	}
}

// Every refusal below is decided before X is contacted.
func TestTwitter_ReadTwitterAudienceInsights_RefusedBeforeAnyRequest(t *testing.T) {
	noAccount := activeTwitterConn(goodTwitterCreds)
	noAccount.AccountID = ""
	badAccount := activeTwitterConn(goodTwitterCreds)
	badAccount.AccountID = "acc_1"
	inactive := activeTwitterConn(goodTwitterCreds)
	inactive.Status = model.StatusInactive
	tooMany := make([]string, twitter.MaxAudienceCampaigns+1)
	for i := range tooMany {
		tooMany[i] = "c" + strconv.Itoa(i)
	}
	good := fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}

	for _, tc := range []struct {
		name   string
		reader connReader
		window model.MetricsWindow
		scope  []model.ProjectCampaignScope
		want   []error
	}{
		{"window over 7 days", good, model.MetricsWindowLast30Days, twitterScope("c1"), []error{domain.ErrMetricsWindowUnsupported}},
		{"this month", good, model.MetricsWindowThisMonth, twitterScope("c1"), []error{domain.ErrMetricsWindowUnsupported}},
		{"no connection", fakeConnReader{err: domain.ErrNotFound}, model.MetricsWindowLast7Days, twitterScope("c1"), []error{domain.ErrNotFound}},
		{"no account selected", fakeConnReader{conn: noAccount}, model.MetricsWindowLast7Days, twitterScope("c1"), []error{domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected}},
		{"unaddressable account id", fakeConnReader{conn: badAccount}, model.MetricsWindowLast7Days, twitterScope("c1"), []error{domain.ErrConnectionNotUsable}},
		{"inactive connection", fakeConnReader{conn: inactive}, model.MetricsWindowLast7Days, twitterScope("c1"), []error{domain.ErrConnectionNotUsable}},
		{"incomplete credential", fakeConnReader{conn: activeTwitterConn(`{}`)}, model.MetricsWindowLast7Days, twitterScope("c1"), []error{domain.ErrConnectionNotUsable}},
		{"unusable LF system fallback", &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: inactive}}, model.MetricsWindowLast7Days, twitterScope("c1"), []error{domain.ErrSystemConnectionNotUsable}},
		{"malformed stored campaign id", good, model.MetricsWindowLast7Days, twitterScope("c1", "c-2"), []error{domain.ErrAudienceScopeInvalid}},
		{"empty scope never widens", good, model.MetricsWindowLast7Days, nil, []error{domain.ErrAudienceScopeInvalid}},
		{"scope too large", good, model.MetricsWindowLast7Days, twitterScope(tooMany...), []error{domain.ErrAudienceScopeTooLarge}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
			s := newTwitterAudienceServer(t, "UTC")
			d := NewTwitterDispatcher(tc.reader, identityEncryptor{}, s.opts(twitterAudienceNow)...)
			ai, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, tc.window, tc.scope)
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("error = %v, want it to wrap %v", err, w)
				}
			}
			if ai != nil {
				t.Errorf("a refused read returned %+v", ai)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("X was contacted %d time(s)", n)
			}
		})
	}
}

// With TWITTER_METRICS_ENABLED anything but "true", the read is "not supported" (400) before
// any credential is resolved or request made.
func TestTwitter_ReadTwitterAudienceInsights_GatedOnMetricsFlag(t *testing.T) {
	for _, v := range []string{"", "false", "TRUE", "1"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvTwitterMetricsEnabled, v)
			s := newTwitterAudienceServer(t, "UTC")
			reader := &scopedConnReader{rows: map[string]*model.Connection{"proj": activeTwitterConn(goodTwitterCreds)}}
			d := NewTwitterDispatcher(reader, identityEncryptor{}, s.opts(twitterAudienceNow)...)
			_, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowLast7Days, twitterScope("c1"))
			if !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
				t.Errorf("err = %v, want ErrKeywordInsightsUnsupported", err)
			}
			if n := len(s.requests()); n != 0 || len(reader.gets) != 0 {
				t.Errorf("%d upstream calls and %d connection reads while disabled", n, len(reader.gets))
			}
		})
	}
}

// ANY campaign created under a different ad account refuses the whole read (409).
func TestTwitter_ReadTwitterAudienceInsights_AccountMismatchRefusesWholeRead(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	foreign := model.ProjectCampaignScope{PlatformCampaignID: "c9", Result: json.RawMessage(`{"CampaignID":"c9","AccountID":"acc2"}`)}
	for name, scope := range map[string][]model.ProjectCampaignScope{
		"all foreign": {foreign},
		"partial":     append(twitterScope("c1"), foreign),
	} {
		t.Run(name, func(t *testing.T) {
			s := newTwitterAudienceServer(t, "UTC")
			d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, s.opts(twitterAudienceNow)...)
			_, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowLast7Days, scope)
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("err = %v, want ErrCampaignAccountMismatch", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("X was contacted %d time(s)", n)
			}
		})
	}
}

// A matching or unrecorded provenance proceeds, and a project with no connection of its own reads
// its OWN campaigns through the LF system fallback.
func TestTwitter_ReadTwitterAudienceInsights_MatchingUnknownAndFallbackRead(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	scope := []model.ProjectCampaignScope{
		{PlatformCampaignID: "c1", Result: json.RawMessage(`{"CampaignID":"c1","AccountID":"acc1"}`)},
		{PlatformCampaignID: "c2"},
	}
	for name, reader := range map[string]connReader{
		"own connection":     fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)},
		"LF system fallback": &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: activeTwitterConn(goodTwitterCreds)}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTwitterAudienceServer(t, "UTC")
			d := NewTwitterDispatcher(reader, identityEncryptor{}, s.opts(twitterAudienceNow)...)
			if _, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowLast7Days, scope); err != nil {
				t.Fatalf("ReadTwitterAudienceInsights: %v", err)
			}
		})
	}
}

// An account zone whose days do not start on a whole UTC hour is the permanent timezone refusal
// (409), decided after the account read and before any job.
func TestTwitter_ReadTwitterAudienceInsights_FractionalZoneIsTimezoneUnsupported(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	s := newTwitterAudienceServer(t, "Asia/Kathmandu")
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, s.opts(twitterAudienceNow)...)
	_, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope("c1"))
	if !errors.Is(err, domain.ErrAccountTimezoneUnsupported) {
		t.Errorf("err = %v, want ErrAccountTimezoneUnsupported", err)
	}
	for _, r := range s.requests() {
		if strings.HasPrefix(r, http.MethodPost) {
			t.Errorf("a job was created: %s", r)
		}
	}
}

// An upstream failure carries no domain sentinel, so the service maps it to the 503 default.
func TestTwitter_ReadTwitterAudienceInsights_UpstreamFailureIsUnclassified(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	_, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowLast7Days, twitterScope("c1"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, s := range []error{domain.ErrNotFound, domain.ErrConnectionNotUsable, domain.ErrCampaignAccountMismatch, domain.ErrAudienceScopeInvalid,
		domain.ErrAudienceScopeTooLarge, domain.ErrMetricsWindowUnsupported, domain.ErrKeywordInsightsUnsupported, domain.ErrAccountTimezoneUnsupported} {
		if errors.Is(err, s) {
			t.Errorf("upstream failure classified as %v", s)
		}
	}
}
