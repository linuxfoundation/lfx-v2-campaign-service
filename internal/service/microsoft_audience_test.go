// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// audienceNow is the pinned clock every report-backed audience test reads by.
var audienceNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fakeAudienceReader implements AudienceReportReader with scripted answers, counting each call.
type fakeAudienceReader struct {
	mu         sync.Mutex
	account    string
	accountErr error
	enabledErr error
	submitID   string
	submitErr  error
	check      *model.AudienceReportCheck
	checkErr   error
	accounts   int
	submits    int
	checks     int
	// dates, when set, is ReportWindowDates; nil answers the fake period whatever the clock.
	dates func(model.MetricsWindow, time.Time) (time.Time, time.Time, error)
	// submitNow is the clock a scripted-dates submission resolves its window at.
	submitNow time.Time
}

// fakeAudiencePeriodStart..fakeAudiencePeriodEnd is the period the fake's submissions cover and,
// unless a test scripts dates, the period every window resolves to.
var (
	fakeAudiencePeriodStart = audienceNow.AddDate(0, 0, -29)
	fakeAudiencePeriodEnd   = audienceNow
)

func (f *fakeAudienceReader) ReportWindowDates(w model.MetricsWindow, now time.Time) (time.Time, time.Time, error) {
	if f.dates != nil {
		return f.dates(w, now)
	}
	return fakeAudiencePeriodStart, fakeAudiencePeriodEnd, nil
}

func (f *fakeAudienceReader) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (f *fakeAudienceReader) AudienceReportEnabled(model.MetricsWindow) error { return f.enabledErr }

func (f *fakeAudienceReader) AudienceReportAccount(context.Context, string, model.Provider, model.MetricsWindow, []model.ProjectCampaignScope) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts++
	if f.accountErr != nil {
		return "", f.accountErr
	}
	return f.account, nil
}

func (f *fakeAudienceReader) SubmitAudienceReport(_ context.Context, _ string, _ model.Provider, _ string, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.InsightReportSubmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	ids := make([]string, 0, len(scope))
	for _, s := range scope {
		ids = append(ids, s.PlatformCampaignID)
	}
	start, end := fakeAudiencePeriodStart, fakeAudiencePeriodEnd
	if f.dates != nil {
		start, end, _ = f.dates(window, f.submitNow)
	}
	return &model.InsightReportSubmission{ReportID: f.submitID, WindowStart: start, WindowEnd: end, CampaignIDs: ids}, nil
}

func (f *fakeAudienceReader) CheckAudienceReport(context.Context, string, model.Provider, string, string) (*model.AudienceReportCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.check, f.checkErr
}

// fakeAudienceStore is an in-memory AudienceReportRepository with the real compare-and-set.
type fakeAudienceStore struct {
	snap     *model.AudienceReportSnapshot
	getErr   error
	keys     []model.InsightReportKey
	failures []string
}

func (s *fakeAudienceStore) GetAudienceReport(_ context.Context, key model.InsightReportKey) (*model.AudienceReportSnapshot, error) {
	s.keys = append(s.keys, key)
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.snap == nil {
		return nil, domain.ErrNotFound
	}
	cp := *s.snap
	cp.Key = key
	return &cp, nil
}

func (s *fakeAudienceStore) MarkAudienceReportPending(_ context.Context, key model.InsightReportKey, p model.PendingInsightReport) (bool, error) {
	if s.snap == nil {
		s.snap = &model.AudienceReportSnapshot{Key: key}
	}
	if s.snap.Pending != nil {
		return false, nil
	}
	s.snap.Pending = &p
	return true, nil
}

func (s *fakeAudienceStore) CompleteAudienceReport(_ context.Context, _ model.InsightReportKey, r model.ReadyAudienceReport) (bool, error) {
	if s.snap == nil || s.snap.Pending == nil || s.snap.Pending.ReportID != r.ReportID {
		return false, nil
	}
	s.snap.Ready, s.snap.Pending = &r, nil
	return true, nil
}

func (s *fakeAudienceStore) FailAudienceReport(_ context.Context, _ model.InsightReportKey, reportID, reason string, _ time.Time) (bool, error) {
	if s.snap == nil || s.snap.Pending == nil || s.snap.Pending.ReportID != reportID {
		return false, nil
	}
	s.snap.Pending = nil
	s.failures = append(s.failures, reason)
	return true, nil
}

func audienceOrch(scope []string, reader *fakeAudienceReader, store domain.AudienceReportRepository) *Orchestrator {
	o := NewOrchestrator(&fakeCampaignRepo{scopeIDs: scope}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: reader})
	o.SetAudienceReportStore(store)
	o.SetInsightReportClock(func() time.Time { return audienceNow })
	return o
}

func readAudience(t *testing.T, o *Orchestrator) *model.ReportedAudienceRead {
	t.Helper()
	read, err := o.ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadReportedAudience: %v", err)
	}
	return read
}

func agRow(campaign, age, gender string, imp int64) model.AudienceReportRow {
	return model.AudienceReportRow{CampaignID: campaign, AgeGroup: age, Gender: gender, Impressions: imp, Clicks: imp / 10, Spend: 1.5}
}

func TestReadAudience_EmptyScopeMakesNoCalls(t *testing.T) {
	r := &fakeAudienceReader{account: "123"}
	store := &fakeAudienceStore{}
	read := readAudience(t, audienceOrch([]string{}, r, store))
	if read.Buckets == nil || len(read.Buckets) != 0 || read.MetricsPending || read.MetricsAsOf != nil {
		t.Errorf("got %+v, want an empty, settled result", read)
	}
	if r.accounts+r.submits+r.checks != 0 || len(store.keys) != 0 {
		t.Errorf("an empty scope must touch neither the dispatcher nor the store")
	}
}

func TestReadAudience_GateIsCheckedBeforeTheEmptyScopeSuccess(t *testing.T) {
	r := &fakeAudienceReader{enabledErr: fmt.Errorf("disabled: %w", domain.ErrKeywordInsightsUnsupported)}
	store := &fakeAudienceStore{}
	_, err := audienceOrch([]string{}, r, store).ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days)
	if !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Fatalf("err = %v, want ErrKeywordInsightsUnsupported", err)
	}
	if r.accounts+r.submits+r.checks != 0 || len(store.keys) != 0 {
		t.Errorf("a gated-off read must touch neither the account path nor the store")
	}
}

func TestReadAudience_FirstReadSubmitsAndServesNothing(t *testing.T) {
	r := &fakeAudienceReader{account: "123", submitID: "a1"}
	store := &fakeAudienceStore{}
	read := readAudience(t, audienceOrch([]string{"111", "222"}, r, store))
	if !read.MetricsPending || read.MetricsAsOf != nil || len(read.Buckets) != 0 {
		t.Errorf("got %+v, want pending with no buckets", read)
	}
	if r.submits != 1 || store.snap.Pending == nil || store.snap.Pending.ReportID != "a1" || !store.snap.Pending.SubmittedAt.Equal(audienceNow) {
		t.Errorf("submits=%d pending=%+v (submitted-at must be the pinned clock)", r.submits, store.snap.Pending)
	}
	if got := store.keys[0]; got.AccountID != "123" || got.Window != model.MetricsWindowLast30Days || got.ProjectID != "cncf" {
		t.Errorf("store key = %+v", got)
	}
}

// A finished report is collected and summed per (age, gender) across the project's campaigns,
// confined to the campaigns it owns now, ordered by impressions.
func TestReadAudience_CollectsAndSumsAFinishedReport(t *testing.T) {
	r := &fakeAudienceReader{account: "123", check: &model.AudienceReportCheck{
		Status: model.AccountReportReady, Partial: true, Rows: []model.AudienceReportRow{
			agRow("111", "25-34", "Female", 100),
			agRow("222", "25-34", "Female", 50),
			agRow("111", "35-49", "Male", 400),
			agRow("222", "65+", "Unknown", 0),
		},
	}}
	submitted := audienceNow.Add(-5 * time.Minute)
	store := &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: &model.PendingInsightReport{
		ReportID: "a1", CampaignIDs: []string{"111", "222"}, SubmittedAt: submitted, WindowStart: fakeAudiencePeriodStart, WindowEnd: fakeAudiencePeriodEnd,
	}}}
	read := readAudience(t, audienceOrch([]string{"111", "222"}, r, store))
	if read.MetricsPending || read.MetricsAsOf == nil || !read.MetricsAsOf.Equal(submitted) || !read.DataIncomplete {
		t.Errorf("as_of=%v pending=%v incomplete=%v", read.MetricsAsOf, read.MetricsPending, read.DataIncomplete)
	}
	want := []model.ReportedAudienceBucket{
		{AgeGroup: "35-49", Gender: "Male", Impressions: 400, Clicks: 40, CostMicros: 1_500_000, Ctr: 0.1},
		{AgeGroup: "25-34", Gender: "Female", Impressions: 150, Clicks: 15, CostMicros: 3_000_000, Ctr: 0.1},
		{AgeGroup: "65+", Gender: "Unknown", Impressions: 0, Clicks: 0, CostMicros: 1_500_000, Ctr: 0},
	}
	if len(read.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", read.Buckets)
	}
	for i := range want {
		if read.Buckets[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, read.Buckets[i], want[i])
		}
	}
	if r.submits != 0 {
		t.Errorf("a fresh covering report must not be resubmitted")
	}
}

func TestReadAudience_ScopeCoverageAndFreshness(t *testing.T) {
	ready := &model.ReadyAudienceReport{
		WindowStart: fakeAudiencePeriodStart, WindowEnd: fakeAudiencePeriodEnd,
		ReportID: "a1", CampaignIDs: []string{"111", "999"}, AsOf: audienceNow.Add(-time.Minute),
		Rows: []model.AudienceReportRow{agRow("111", "25-34", "Female", 5), agRow("999", "25-34", "Female", 50)},
	}
	// Uncovered campaign: not served, resubmitted.
	r := &fakeAudienceReader{account: "123", submitID: "a2"}
	store := &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Ready: ready}}
	read := readAudience(t, audienceOrch([]string{"111", "222"}, r, store))
	if len(read.Buckets) != 0 || read.MetricsAsOf != nil || !read.MetricsPending || r.submits != 1 {
		t.Errorf("uncovered: read=%+v submits=%d", read, r.submits)
	}
	// Narrowed scope: the dropped campaign's rows are not summed in; fresh, so no report calls.
	r = &fakeAudienceReader{account: "123"}
	store = &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Ready: ready}}
	read = readAudience(t, audienceOrch([]string{"111"}, r, store))
	if len(read.Buckets) != 1 || read.Buckets[0].Impressions != 5 || r.submits+r.checks != 0 {
		t.Errorf("narrowed: buckets=%+v submits=%d checks=%d", read.Buckets, r.submits, r.checks)
	}
	// Stale (by the pinned clock): served while the next builds.
	stale := *ready
	stale.AsOf = audienceNow.Add(-accountReportFreshFor - time.Second)
	r = &fakeAudienceReader{account: "123", submitID: "a3"}
	store = &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Ready: &stale}}
	read = readAudience(t, audienceOrch([]string{"111"}, r, store))
	if len(read.Buckets) != 1 || !read.MetricsPending || r.submits != 1 {
		t.Errorf("stale: read=%+v submits=%d", read, r.submits)
	}
}

func TestReadAudience_PendingStatesAndAbandon(t *testing.T) {
	pending := func(age time.Duration) *fakeAudienceStore {
		return &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: &model.PendingInsightReport{
			ReportID: "a1", CampaignIDs: []string{"111"}, SubmittedAt: audienceNow.Add(-age),
			WindowStart: fakeAudiencePeriodStart, WindowEnd: fakeAudiencePeriodEnd,
		}}}
	}
	r := &fakeAudienceReader{account: "123", check: &model.AudienceReportCheck{Status: model.AccountReportPending}}
	store := pending(time.Minute)
	if read := readAudience(t, audienceOrch([]string{"111"}, r, store)); !read.MetricsPending || r.submits != 0 {
		t.Errorf("pending: read=%+v submits=%d", read, r.submits)
	}
	r = &fakeAudienceReader{account: "123", submitID: "a2", check: &model.AudienceReportCheck{Status: model.AccountReportFailed}}
	store = pending(time.Minute)
	readAudience(t, audienceOrch([]string{"111"}, r, store))
	if len(store.failures) != 1 || store.snap.Pending == nil || store.snap.Pending.ReportID != "a2" {
		t.Errorf("failed: failures=%v pending=%+v", store.failures, store.snap.Pending)
	}
	r = &fakeAudienceReader{account: "123", submitID: "a3", check: &model.AudienceReportCheck{Status: model.AccountReportPending}}
	store = pending(accountReportAbandonAfter + time.Second)
	readAudience(t, audienceOrch([]string{"111"}, r, store))
	if len(store.failures) != 1 || store.snap.Pending.ReportID != "a3" {
		t.Errorf("overdue: failures=%v pending=%+v", store.failures, store.snap.Pending)
	}
	// Exactly at the abandon threshold (pinned clock) it is NOT yet abandoned.
	r = &fakeAudienceReader{account: "123", check: &model.AudienceReportCheck{Status: model.AccountReportPending}}
	store = pending(accountReportAbandonAfter)
	readAudience(t, audienceOrch([]string{"111"}, r, store))
	if len(store.failures) != 0 || store.snap.Pending == nil || store.snap.Pending.ReportID != "a1" {
		t.Errorf("at threshold: failures=%v pending=%+v", store.failures, store.snap.Pending)
	}
}

func TestReadAudience_RefusalsAndPermanence(t *testing.T) {
	for _, refusal := range []error{
		domain.ErrKeywordInsightsUnsupported, domain.ErrCampaignAccountMismatch,
		domain.ErrAudienceScopeTooLarge, domain.ErrAudienceScopeInvalid,
		domain.ErrMetricsWindowUnsupported, domain.ErrNotFound,
	} {
		r := &fakeAudienceReader{accountErr: fmt.Errorf("refused: %w", refusal)}
		store := &fakeAudienceStore{}
		o := audienceOrch([]string{"111"}, r, store)
		rec := &recordingMetrics{}
		o.SetMetrics(rec)
		_, err := o.ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days)
		if !errors.Is(err, refusal) || len(rec.upstreamCalls()) != 0 || r.submits+r.checks != 0 || len(store.keys) != 0 {
			t.Errorf("%v: err=%v upstream=%d; a refusal must stop before the store and the platform", refusal, err, len(rec.upstreamCalls()))
		}
	}
	r := &fakeAudienceReader{account: "123", submitErr: errors.New("transient")}
	if _, err := audienceOrch([]string{"111"}, r, &fakeAudienceStore{}).ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err != nil {
		t.Errorf("a transient submit error must not fail the read: %v", err)
	}
	for _, permanent := range []error{
		domain.ErrCampaignAccountMismatch, domain.ErrAudienceScopeInvalid,
		domain.ErrAudienceScopeTooLarge, domain.ErrServiceDefect,
	} {
		r = &fakeAudienceReader{account: "123", submitErr: fmt.Errorf("x: %w", permanent)}
		if _, err := audienceOrch([]string{"111"}, r, &fakeAudienceStore{}).ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); !errors.Is(err, permanent) {
			t.Errorf("%v: a permanent refusal must fail the read, got %v", permanent, err)
		}
	}
}

func TestReadAudience_RefusesWithoutCapabilityOrStore(t *testing.T) {
	// A dispatcher with only the KEYWORD report capability does not serve audiences.
	o := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"111"}}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: &fakeKeywordReader{account: "1"}})
	o.SetAudienceReportStore(&fakeAudienceStore{})
	if _, err := o.ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("no capability: %v", err)
	}
	if _, err := audienceOrch([]string{"111"}, &fakeAudienceReader{account: "1"}, nil).ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err == nil {
		t.Errorf("no store: want an error")
	}
	store := &fakeAudienceStore{getErr: errors.New("db down")}
	if _, err := audienceOrch([]string{"111"}, &fakeAudienceReader{account: "1"}, store).ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err == nil {
		t.Errorf("store read error: want an error")
	}
}

func microsoftAudienceService(scope []string, reader *fakeAudienceReader, store domain.AudienceReportRepository) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(audienceOrch(scope, reader, store))
	return svc
}

func TestGetMicrosoftAdsAudience_MapsTheSavedReport(t *testing.T) {
	asOf := audienceNow.Add(-time.Minute)
	store := &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Ready: &model.ReadyAudienceReport{
		WindowStart: fakeAudiencePeriodStart, WindowEnd: fakeAudiencePeriodEnd,
		ReportID: "a1", CampaignIDs: []string{"111"}, AsOf: asOf, Partial: true,
		Rows: []model.AudienceReportRow{agRow("111", "25-34", "Female", 100)},
	}}}
	got, err := microsoftAudienceService([]string{"111"}, &fakeAudienceReader{account: "123"}, store).
		GetMicrosoftAdsAudience(context.Background(), &conn.GetMicrosoftAdsAudiencePayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("GetMicrosoftAdsAudience: %v", err)
	}
	if got.Window != "last_30_days" || got.BucketCount != 1 || got.MetricsPending || !got.DataIncomplete {
		t.Errorf("envelope = %+v", got)
	}
	if got.MetricsAsOf == nil || *got.MetricsAsOf != "2026-10-07T11:59:00Z" {
		t.Errorf("metrics_as_of = %v", got.MetricsAsOf)
	}
	b := got.Buckets[0]
	if b.AgeGroup != "25-34" || b.Gender != "Female" || b.Impressions != 100 || b.Clicks != 10 || b.CostMicros != 1_500_000 || b.Ctr != 0.1 {
		t.Errorf("bucket = %+v", b)
	}
}

func TestGetMicrosoftAdsAudience_RefusesUnservableWindowsAndSystemScope(t *testing.T) {
	r := &fakeAudienceReader{account: "123"}
	svc := microsoftAudienceService([]string{"111"}, r, &fakeAudienceStore{})
	for _, w := range []string{"yesterday", "last_14_days"} {
		w := w
		_, err := svc.GetMicrosoftAdsAudience(context.Background(), &conn.GetMicrosoftAdsAudiencePayload{ProjectID: "p", Window: &w})
		br, ok := err.(*conn.BadRequestError)
		if !ok || br.Message != microsoftKeywordWindowMessage {
			t.Errorf("window %s: got %T %v, want the keyword read's 400", w, err, err)
		}
	}
	if _, err := svc.GetMicrosoftAdsAudience(context.Background(), &conn.GetMicrosoftAdsAudiencePayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Errorf("the reserved system scope must be refused")
	}
	if r.accounts != 0 {
		t.Errorf("a refused request must not reach the dispatcher")
	}
}

func TestGetMicrosoftAdsAudience_ClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{domain.ErrKeywordInsightsUnsupported, "400"},
		{domain.ErrMetricsWindowUnsupported, "400"},
		{domain.ErrNotFound, "404"},
		{domain.ErrCampaignAccountMismatch, "409"},
		{domain.ErrAudienceScopeTooLarge, "409"},
		{domain.ErrAudienceScopeInvalid, "409"},
		{domain.ErrServiceDefect, "500"},
		{errors.New("upstream said SECRET-DETAIL"), "503"},
	} {
		_, err := microsoftAudienceService([]string{"111"}, &fakeAudienceReader{accountErr: fmt.Errorf("x: %w", tc.err)}, &fakeAudienceStore{}).
			GetMicrosoftAdsAudience(context.Background(), &conn.GetMicrosoftAdsAudiencePayload{ProjectID: "p"})
		var ok bool
		switch tc.want {
		case "400":
			_, ok = err.(*conn.BadRequestError)
		case "404":
			_, ok = err.(*conn.NotFoundError)
		case "409":
			_, ok = err.(*conn.ConflictError)
		case "500":
			_, ok = err.(*conn.InternalServerError)
		case "503":
			var su *conn.ConnServiceUnavailableError
			su, ok = err.(*conn.ConnServiceUnavailableError)
			ok = ok && strings.Contains(su.Message, "audience insights") && !strings.Contains(su.Message, "SECRET-DETAIL")
		}
		if !ok {
			t.Errorf("%v: got %T (%v), want %s", tc.err, err, err, tc.want)
		}
	}
	// The gated-off answer is the keyword read's exact 400.
	_, err := microsoftAudienceService([]string{}, &fakeAudienceReader{enabledErr: domain.ErrKeywordInsightsUnsupported}, &fakeAudienceStore{}).
		GetMicrosoftAdsAudience(context.Background(), &conn.GetMicrosoftAdsAudiencePayload{ProjectID: "p"})
	if br, ok := err.(*conn.BadRequestError); !ok || br.Message != "keyword and audience insights are not supported for this platform" {
		t.Errorf("gate off: got %T %v", err, err)
	}
}

// TestPublishedMicrosoftAudienceSpec reads the GENERATED OpenAPI documents (gen/ and the kodata
// copy the service serves) and pins what this read promises: the route, the Microsoft window
// enum, the required envelope and bucket fields, NO device field and NO currency, and a
// composite example whose bucket_count matches its buckets.
func TestPublishedMicrosoftAudienceSpec(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "gen", "http", "openapi3.json"),
		filepath.Join("..", "..", "cmd", "campaign-service", "kodata", "gen", "http", "openapi3.json"),
	} {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			type schema struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
				Example    json.RawMessage            `json:"example"`
			}
			var doc struct {
				Paths      map[string]map[string]json.RawMessage `json:"paths"`
				Components struct {
					Schemas map[string]schema `json:"schemas"`
				} `json:"components"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if _, ok := doc.Paths["/projects/{project_id}/microsoft-ads/audience"]["get"]; !ok {
				t.Fatalf("GET /projects/{project_id}/microsoft-ads/audience is not published")
			}
			env, ok := doc.Components.Schemas["MicrosoftAdsAudience"]
			if !ok {
				t.Fatalf("no MicrosoftAdsAudience schema")
			}
			if got := strings.Join(env.Required, ","); got != "window,buckets,bucket_count,metrics_pending,data_incomplete" {
				t.Errorf("envelope required = %s", got)
			}
			var window struct {
				Enum []string `json:"enum"`
			}
			_ = json.Unmarshal(env.Properties["window"], &window)
			if got := strings.Join(window.Enum, ","); got != "today,last_7_days,last_30_days,this_month,last_month" {
				t.Errorf("window enum = %s", got)
			}
			if _, has := env.Properties["account_currency"]; has {
				t.Errorf("account_currency must not be published: Microsoft's report carries no currency")
			}
			var ex struct {
				Buckets     []map[string]any `json:"buckets"`
				BucketCount int              `json:"bucket_count"`
			}
			if err := json.Unmarshal(env.Example, &ex); err != nil || len(ex.Buckets) == 0 || ex.BucketCount != len(ex.Buckets) {
				t.Errorf("composite example must carry buckets matching bucket_count: %s", env.Example)
			}
			bucket, ok := doc.Components.Schemas["MicrosoftAdsAudienceBucket"]
			if !ok {
				t.Fatalf("no MicrosoftAdsAudienceBucket schema")
			}
			if got := strings.Join(bucket.Required, ","); got != "age_group,gender,impressions,clicks,cost_micros,ctr" {
				t.Errorf("bucket required = %s", got)
			}
			for name := range bucket.Properties {
				if strings.Contains(name, "device") {
					t.Errorf("bucket publishes %s: Microsoft's age/gender report has no device dimension", name)
				}
			}
			if len(bucket.Example) == 0 {
				t.Errorf("bucket has no type-level example")
			}
		})
	}
}
