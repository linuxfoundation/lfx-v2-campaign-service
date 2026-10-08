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
	"sync/atomic"
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

	// gate, when set before the first request, holds every account read until it is closed;
	// entered receives one value per account read that reached the gate. Both are read outside
	// mu so a held request never blocks the others' bookkeeping.
	gate    chan struct{}
	entered chan struct{}
	release func()

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
		if s.gate != nil && r.Method == http.MethodGet && r.URL.Path == "/12/accounts/acc1" {
			s.entered <- struct{}{}
			<-s.gate
		}
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

func (s *twitterAudienceServer) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func newGatedTwitterAudience(t *testing.T) (*twitterAudienceServer, *TwitterDispatcher) {
	t.Helper()
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	s := newTwitterAudienceServer(t, "UTC")
	s.gate = make(chan struct{})
	s.entered = make(chan struct{}, 16)
	// Registered after the server's own Cleanup, so it runs FIRST: a test that fails before
	// releasing the gate must not leave handlers blocked, or srv.Close would wait forever.
	var once sync.Once
	release := func() { once.Do(func() { close(s.gate) }) }
	t.Cleanup(release)
	s.release = release
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, s.opts(twitterAudienceNow)...)
	d.audienceNow = func() time.Time { return twitterAudienceNow }
	return s, d
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// N concurrent identical reads share ONE set of stats jobs.
func TestTwitter_AudienceGuard_ConcurrentIdenticalReadsShareJobs(t *testing.T) {
	s, d := newGatedTwitterAudience(t)
	joined := make(chan struct{}, 16)
	d.audience.onJoin = func() { joined <- struct{}{} }
	const n = 5
	errs := make(chan error, n)
	read := func() {
		_, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope("c1"))
		errs <- err
	}
	go read()
	waitFor(t, s.entered, "the leader's account read")
	for i := 1; i < n; i++ {
		go read()
	}
	for i := 1; i < n; i++ {
		waitFor(t, joined, "a follower to join the in-flight read")
	}
	s.release()
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("read %d: %v", i, err)
		}
	}
	if posts := s.count(http.MethodPost); posts != 3 {
		t.Errorf("%d stats jobs for %d identical reads, want one set of 3", posts, n)
	}
}

// At most one audience read runs per ad account: a second, different read waits, and gives up
// with its context rather than creating jobs of its own.
func TestTwitter_AudienceGuard_OneReadPerAccount(t *testing.T) {
	s, d := newGatedTwitterAudience(t)
	first := make(chan error, 1)
	go func() {
		_, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope("c1"))
		first <- err
	}()
	waitFor(t, s.entered, "the first read's account read")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := d.ReadTwitterAudienceInsights(ctx, "proj", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope("c2"))
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("second read: err = %v, want the busy-account refusal", err)
	}
	select {
	case <-s.entered:
		t.Error("the second read reached X while the first was running")
	default:
	}
	s.release()
	if err := <-first; err != nil {
		t.Errorf("first read: %v", err)
	}
}

// A successful result is reused within the TTL and for the same account-local window: a refresh
// creates zero jobs. Past the TTL, or across the account's midnight, the read runs again.
func TestTwitter_AudienceGuard_CacheHitCreatesNoJobs(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	s := newTwitterAudienceServer(t, "UTC")
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, s.opts(twitterAudienceNow)...)
	clock := time.Date(2026, 10, 5, 23, 50, 0, 0, time.UTC)
	d.audienceNow = func() time.Time { return clock }
	read := func(scope ...string) {
		t.Helper()
		if _, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope(scope...)); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	read("c1")
	clock = clock.Add(2 * time.Minute)
	read("c1", "c1") // same scope after de-duplication
	if posts := s.count(http.MethodPost); posts != 3 {
		t.Fatalf("%d jobs after a cached refresh, want 3", posts)
	}
	read("c1", "c2") // a different scope is never served from another's entry
	if posts := s.count(http.MethodPost); posts != 6 {
		t.Fatalf("%d jobs after a different scope, want 6", posts)
	}
	clock = time.Date(2026, 10, 5, 23, 55, 30, 0, time.UTC) // 5.5 minutes after c1 was stored
	read("c1")
	if posts := s.count(http.MethodPost); posts != 9 {
		t.Fatalf("%d jobs after the TTL, want 9", posts)
	}
	// The client's clock is pinned to Oct 5 12:00, so its "today" is Oct 5. Stored at 23:59 and
	// asked again at 00:01 on Oct 6 (inside the TTL), that entry names yesterday's instants on the
	// account's calendar and must not be served.
	clock = time.Date(2026, 10, 5, 23, 59, 0, 0, time.UTC)
	read("c1", "c3")
	clock = time.Date(2026, 10, 6, 0, 1, 0, 0, time.UTC)
	read("c1", "c3")
	if posts := s.count(http.MethodPost); posts != 15 {
		t.Errorf("%d jobs after the account's midnight, want 15", posts)
	}
}

// Failures are never cached.
func TestTwitter_AudienceGuard_FailuresAreNotCached(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	for i := 0; i < 2; i++ {
		if _, err := d.ReadTwitterAudienceInsights(context.Background(), "proj", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope("c1")); err == nil {
			t.Fatal("expected an error")
		}
	}
	if calls.Load() < 2 {
		t.Errorf("%d upstream calls; a failure was served from the cache", calls.Load())
	}
}

// fakeStatsLease owns only the accounts in owned, and records every account asked about.
type fakeStatsLease struct {
	mu    sync.Mutex
	owned map[string]bool
	asked []string
}

func (f *fakeStatsLease) Own(_ context.Context, accountID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, accountID)
	if f.owned[accountID] {
		return nil
	}
	return domain.ErrStatsJobLeaseNotHeld
}

// A pod that does not own the account's stats-job lease refuses the audience read and the monitor's
// report submission WITHOUT contacting X; the owning pod runs both.
func TestTwitter_StatsJobLease_NonOwnerRefusesWithoutContactingX(t *testing.T) {
	t.Setenv(constants.EnvTwitterMetricsEnabled, "true")
	opts, calls, _ := twitterMonitorServer(t, twitterMonitorRoutes)
	lease := &fakeStatsLease{owned: map[string]bool{}}
	reader := &scopedConnReader{rows: map[string]*model.Connection{"cncf": activeTwitterConn(goodTwitterCreds)}}
	d := NewTwitterDispatcher(reader, identityEncryptor{}, opts...)
	d.SetStatsJobLease(lease)

	if _, err := d.ReadTwitterAudienceInsights(context.Background(), "cncf", model.ProviderTwitterAds, model.MetricsWindowToday, twitterScope("c1")); !errors.Is(err, domain.ErrStatsJobLeaseNotHeld) {
		t.Errorf("audience read: err = %v, want ErrStatsJobLeaseNotHeld", err)
	}
	if _, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", 7); !errors.Is(err, domain.ErrStatsJobLeaseNotHeld) {
		t.Errorf("monitor submit: err = %v, want ErrStatsJobLeaseNotHeld", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a non-owner made %d request(s) to X", n)
	}
	lease.mu.Lock()
	asked := strings.Join(lease.asked, ",")
	lease.mu.Unlock()
	if asked != "acc1,acc1" {
		t.Errorf("lease asked about %q, want the connection's account twice", asked)
	}

	// The owner submits (the stub creates a job).
	lease.mu.Lock()
	lease.owned["acc1"] = true
	lease.mu.Unlock()
	if _, err := d.SubmitAccountReport(context.Background(), "cncf", model.ProviderTwitterAds, "acc1", 7); err != nil {
		t.Errorf("owner submit: %v", err)
	}
	if calls.Load() == 0 {
		t.Error("the owner did not reach X")
	}
}
