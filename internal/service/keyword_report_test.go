// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeKeywordReader implements KeywordReportReader with scripted answers, counting each call.
type fakeKeywordReader struct {
	mu         sync.Mutex
	account    string
	accountErr error
	enabledErr error
	enabled    int
	submitID   string
	submitErr  error
	check      *model.KeywordReportCheck
	checkErr   error
	accounts   int
	submits    int
	checks     int
	scopes     [][]model.ProjectCampaignScope
}

func (f *fakeKeywordReader) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (f *fakeKeywordReader) KeywordReportEnabled(model.MetricsWindow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled++
	return f.enabledErr
}

func (f *fakeKeywordReader) KeywordReportAccount(_ context.Context, _ string, _ model.Provider, _ model.MetricsWindow, scope []model.ProjectCampaignScope) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts++
	f.scopes = append(f.scopes, scope)
	if f.accountErr != nil {
		return "", f.accountErr
	}
	return f.account, nil
}

func (f *fakeKeywordReader) SubmitKeywordReport(_ context.Context, _ string, _ model.Provider, _ string, _ model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.KeywordReportSubmission, error) {
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
	d := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	return &model.KeywordReportSubmission{ReportID: f.submitID, WindowStart: d.AddDate(0, 0, -29), WindowEnd: d, CampaignIDs: ids}, nil
}

func (f *fakeKeywordReader) CheckKeywordReport(context.Context, string, model.Provider, string, string) (*model.KeywordReportCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.check, f.checkErr
}

// fakeKeywordStore is an in-memory KeywordReportRepository with the real compare-and-set.
type fakeKeywordStore struct {
	snap     *model.KeywordReportSnapshot
	getErr   error
	keys     []model.KeywordReportKey
	failures []string
}

func (s *fakeKeywordStore) GetKeywordReport(_ context.Context, key model.KeywordReportKey) (*model.KeywordReportSnapshot, error) {
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

func (s *fakeKeywordStore) MarkKeywordReportPending(_ context.Context, key model.KeywordReportKey, p model.PendingKeywordReport) (bool, error) {
	if s.snap == nil {
		s.snap = &model.KeywordReportSnapshot{Key: key}
	}
	if s.snap.Pending != nil {
		return false, nil
	}
	s.snap.Pending = &p
	return true, nil
}

func (s *fakeKeywordStore) CompleteKeywordReport(_ context.Context, _ model.KeywordReportKey, r model.ReadyKeywordReport) (bool, error) {
	if s.snap == nil || s.snap.Pending == nil || s.snap.Pending.ReportID != r.ReportID {
		return false, nil
	}
	s.snap.Ready, s.snap.Pending = &r, nil
	return true, nil
}

func (s *fakeKeywordStore) FailKeywordReport(_ context.Context, _ model.KeywordReportKey, reportID, reason string, _ time.Time) (bool, error) {
	if s.snap == nil || s.snap.Pending == nil || s.snap.Pending.ReportID != reportID {
		return false, nil
	}
	s.snap.Pending = nil
	s.failures = append(s.failures, reason)
	return true, nil
}

func keywordOrch(scope []string, reader *fakeKeywordReader, store domain.KeywordReportRepository) *Orchestrator {
	o := NewOrchestrator(&fakeCampaignRepo{scopeIDs: scope}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: reader})
	o.SetKeywordReportStore(store)
	return o
}

func readKeywords(t *testing.T, o *Orchestrator) *model.ReportedKeywordRead {
	t.Helper()
	read, err := o.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadReportedKeywordPerformance: %v", err)
	}
	return read
}

func kwRow(campaign, kw string, imp int64) model.KeywordReportRow {
	conv := 1.0
	return model.KeywordReportRow{CampaignID: campaign, AdGroupID: "ag-" + campaign, KeywordID: kw, Text: "t" + kw, MatchType: "EXACT", Status: "ENABLED", Impressions: imp, Clicks: imp / 10, Spend: 1.5, Conversions: &conv}
}

func TestReadKeywords_EmptyScopeMakesNoCalls(t *testing.T) {
	r := &fakeKeywordReader{account: "123"}
	store := &fakeKeywordStore{}
	read := readKeywords(t, keywordOrch([]string{}, r, store))
	if read.Rows == nil || len(read.Rows) != 0 || read.MetricsPending || read.MetricsAsOf != nil {
		t.Errorf("got %+v, want an empty, settled result", read)
	}
	if r.accounts+r.submits+r.checks != 0 || len(store.keys) != 0 {
		t.Errorf("an empty scope must touch neither the dispatcher nor the store")
	}
}

// The rollout gate is a property of the platform, not of the project: a project with no
// campaigns must get the same refusal as one with many, not an empty 200 (PR #263 review).
func TestReadKeywords_GateIsCheckedBeforeTheEmptyScopeSuccess(t *testing.T) {
	r := &fakeKeywordReader{account: "123", enabledErr: fmt.Errorf("disabled: %w", domain.ErrKeywordInsightsUnsupported)}
	store := &fakeKeywordStore{}
	_, err := keywordOrch([]string{}, r, store).ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days)
	if !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Fatalf("err = %v, want ErrKeywordInsightsUnsupported", err)
	}
	if r.accounts+r.submits+r.checks != 0 || len(store.keys) != 0 {
		t.Errorf("a gated-off read must touch neither the dispatcher's account path nor the store")
	}
}

func TestReadKeywords_FirstReadSubmitsAndServesNothing(t *testing.T) {
	r := &fakeKeywordReader{account: "123", submitID: "k1"}
	store := &fakeKeywordStore{}
	read := readKeywords(t, keywordOrch([]string{"111", "222"}, r, store))
	if !read.MetricsPending || read.MetricsAsOf != nil || len(read.Rows) != 0 {
		t.Errorf("got %+v, want pending with no metrics", read)
	}
	if r.submits != 1 || store.snap.Pending == nil || store.snap.Pending.ReportID != "k1" {
		t.Errorf("submits=%d pending=%+v", r.submits, store.snap.Pending)
	}
	if got := store.keys[0]; got.AccountID != "123" || got.Window != model.MetricsWindowLast30Days || got.ProjectID != "cncf" {
		t.Errorf("store key = %+v, want the bound account and the window", got)
	}
}

func TestReadKeywords_CollectsAFinishedReport(t *testing.T) {
	r := &fakeKeywordReader{account: "123", check: &model.KeywordReportCheck{
		Status: model.AccountReportReady, Rows: []model.KeywordReportRow{kwRow("111", "1", 10), kwRow("222", "2", 900)},
	}}
	submitted := time.Now().Add(-5 * time.Minute)
	store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Pending: &model.PendingKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111", "222"}, SubmittedAt: submitted,
		WindowStart: time.Now(), WindowEnd: time.Now(),
	}}}
	read := readKeywords(t, keywordOrch([]string{"111", "222"}, r, store))
	if read.MetricsPending || read.MetricsAsOf == nil || !read.MetricsAsOf.Equal(submitted) {
		t.Errorf("as_of=%v pending=%v, want the submission time and nothing pending", read.MetricsAsOf, read.MetricsPending)
	}
	if len(read.Rows) != 2 || read.Rows[0].CriterionID != "2" || read.Rows[0].AdGroupID != "ag-222" {
		t.Fatalf("rows = %+v, want impressions-descending with the keyword handle", read.Rows)
	}
	if got := read.Rows[0]; got.CostMicros != 1_500_000 || got.Ctr != 0.1 || got.Conversions != 1 {
		t.Errorf("row = %+v, want micros, fractional ctr and conversions", got)
	}
	if !read.ConversionsComplete || r.submits != 0 {
		t.Errorf("complete=%v submits=%d", read.ConversionsComplete, r.submits)
	}
}

func TestReadKeywords_FreshCoveringReportMakesNoReportCalls(t *testing.T) {
	r := &fakeKeywordReader{account: "123"}
	store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Ready: &model.ReadyKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111"}, AsOf: time.Now().Add(-time.Minute), Rows: []model.KeywordReportRow{kwRow("111", "1", 5)},
	}}}
	read := readKeywords(t, keywordOrch([]string{"111"}, r, store))
	if r.submits+r.checks != 0 || len(read.Rows) != 1 || read.MetricsPending {
		t.Errorf("submits=%d checks=%d read=%+v", r.submits, r.checks, read)
	}
}

// A campaign the project dispatched AFTER the report was built is not covered: the stale-scope
// report is NOT served (it would present part of the project as all of it) and a new one is
// submitted. A campaign the project no longer owns drops out of a covering report.
func TestReadKeywords_ScopeCoverage(t *testing.T) {
	ready := &model.ReadyKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111", "999"}, AsOf: time.Now().Add(-time.Minute),
		Rows: []model.KeywordReportRow{kwRow("111", "1", 5), kwRow("999", "9", 50)},
	}

	r := &fakeKeywordReader{account: "123", submitID: "k2"}
	store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Ready: ready}}
	read := readKeywords(t, keywordOrch([]string{"111", "222"}, r, store))
	if len(read.Rows) != 0 || read.MetricsAsOf != nil || !read.MetricsPending || r.submits != 1 {
		t.Errorf("uncovered scope: read=%+v submits=%d, want nothing served and a resubmission", read, r.submits)
	}

	r = &fakeKeywordReader{account: "123"}
	store = &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Ready: ready}}
	read = readKeywords(t, keywordOrch([]string{"111"}, r, store))
	if len(read.Rows) != 1 || read.Rows[0].CampaignID != "111" || r.submits != 0 {
		t.Errorf("narrowed scope: rows=%+v submits=%d, want only the project's campaign", read.Rows, r.submits)
	}
}

func TestReadKeywords_StaleReportIsServedWhileTheNextBuilds(t *testing.T) {
	r := &fakeKeywordReader{account: "123", submitID: "k2"}
	store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Ready: &model.ReadyKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111"}, AsOf: time.Now().Add(-2 * accountReportFreshFor), Rows: []model.KeywordReportRow{kwRow("111", "1", 5)},
	}}}
	read := readKeywords(t, keywordOrch([]string{"111"}, r, store))
	if len(read.Rows) != 1 || !read.MetricsPending || r.submits != 1 {
		t.Errorf("read=%+v submits=%d", read, r.submits)
	}
}

func TestReadKeywords_PendingStatesAndAbandon(t *testing.T) {
	pending := func(age time.Duration) *fakeKeywordStore {
		return &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Pending: &model.PendingKeywordReport{
			ReportID: "k1", CampaignIDs: []string{"111"}, SubmittedAt: time.Now().Add(-age),
		}}}
	}
	// Still building: not resubmitted.
	r := &fakeKeywordReader{account: "123", check: &model.KeywordReportCheck{Status: model.AccountReportPending}}
	store := pending(time.Minute)
	if read := readKeywords(t, keywordOrch([]string{"111"}, r, store)); !read.MetricsPending || r.submits != 0 {
		t.Errorf("pending: read=%+v submits=%d", read, r.submits)
	}
	// Failed upstream: dropped and replaced.
	r = &fakeKeywordReader{account: "123", submitID: "k2", check: &model.KeywordReportCheck{Status: model.AccountReportFailed}}
	store = pending(time.Minute)
	readKeywords(t, keywordOrch([]string{"111"}, r, store))
	if len(store.failures) != 1 || store.snap.Pending == nil || store.snap.Pending.ReportID != "k2" {
		t.Errorf("failed: failures=%v pending=%+v", store.failures, store.snap.Pending)
	}
	// Overdue and still pending: abandoned and replaced.
	r = &fakeKeywordReader{account: "123", submitID: "k3", check: &model.KeywordReportCheck{Status: model.AccountReportPending}}
	store = pending(accountReportAbandonAfter + time.Minute)
	readKeywords(t, keywordOrch([]string{"111"}, r, store))
	if len(store.failures) != 1 || store.snap.Pending.ReportID != "k3" {
		t.Errorf("overdue: failures=%v pending=%+v", store.failures, store.snap.Pending)
	}
	// A check error is transient: the report stays pending.
	r = &fakeKeywordReader{account: "123", checkErr: errors.New("boom")}
	store = pending(time.Minute)
	if _, err := keywordOrch([]string{"111"}, r, store).ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err != nil || store.snap.Pending == nil {
		t.Errorf("check error: err=%v pending=%+v", err, store.snap.Pending)
	}
}

// Every refusal from KeywordReportAccount fails the read before the store or any upstream call.
func TestReadKeywords_RefusalsStopBeforeUpstream(t *testing.T) {
	for _, refusal := range []error{
		domain.ErrKeywordInsightsUnsupported, domain.ErrCampaignAccountMismatch,
		domain.ErrKeywordReportScopeTooLarge, domain.ErrKeywordReportScopeInvalid,
		domain.ErrMetricsWindowUnsupported, domain.ErrNotFound,
	} {
		r := &fakeKeywordReader{accountErr: fmt.Errorf("refused: %w", refusal)}
		store := &fakeKeywordStore{}
		o := keywordOrch([]string{"111"}, r, store)
		rec := &recordingMetrics{}
		o.SetMetrics(rec)
		_, err := o.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days)
		if got := rec.upstreamCalls(); len(got) != 0 {
			t.Errorf("%v: a local refusal recorded %d upstream calls, want none", refusal, len(got))
		}
		if !errors.Is(err, refusal) {
			t.Errorf("err = %v, want %v", err, refusal)
		}
		if r.submits+r.checks != 0 || len(store.keys) != 0 {
			t.Errorf("%v: a refusal must not reach the store or the platform", refusal)
		}
	}
}

func TestReadKeywords_SubmitErrorDoesNotFailTheReadButPermanentRefusalDoes(t *testing.T) {
	r := &fakeKeywordReader{account: "123", submitErr: errors.New("transient")}
	if _, err := keywordOrch([]string{"111"}, r, &fakeKeywordStore{}).ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err != nil {
		t.Errorf("a transient submit error must not fail the read: %v", err)
	}
	for _, permanent := range []error{
		domain.ErrCampaignAccountMismatch, domain.ErrKeywordReportScopeInvalid,
		domain.ErrKeywordReportScopeTooLarge, domain.ErrServiceDefect,
	} {
		r = &fakeKeywordReader{account: "123", submitErr: fmt.Errorf("x: %w", permanent)}
		if _, err := keywordOrch([]string{"111"}, r, &fakeKeywordStore{}).ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); !errors.Is(err, permanent) {
			t.Errorf("%v: a permanent refusal must fail the read, got %v", permanent, err)
		}
	}
}

func TestReadKeywords_RefusesWithoutStoreCapabilityOrHealthyStore(t *testing.T) {
	o := NewOrchestrator(&fakeCampaignRepo{scopeIDs: []string{"111"}}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: okDispatcher{}})
	o.SetKeywordReportStore(&fakeKeywordStore{})
	if _, err := o.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("no capability: %v", err)
	}
	if _, err := keywordOrch([]string{"111"}, &fakeKeywordReader{account: "1"}, nil).ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err == nil {
		t.Errorf("no store: want an error")
	}
	store := &fakeKeywordStore{getErr: errors.New("db down")}
	if _, err := keywordOrch([]string{"111"}, &fakeKeywordReader{account: "1"}, store).ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days); err == nil {
		t.Errorf("store read error: want an error, not \"no metrics yet\"")
	}
}

func TestMergeKeywordReport_CapsAndFlagsUnknownConversions(t *testing.T) {
	rows := make([]model.KeywordReportRow, 0, keywordReportRowCap+5)
	for i := 0; i < keywordReportRowCap+5; i++ {
		rows = append(rows, kwRow("111", fmt.Sprint(i), int64(i)))
	}
	rows[len(rows)-1].Conversions = nil // the top row by impressions
	snap := &model.KeywordReportSnapshot{Ready: &model.ReadyKeywordReport{CampaignIDs: []string{"111"}, AsOf: time.Now(), Rows: rows}}
	read := mergeKeywordReport(model.MetricsWindowLast7Days, snap, map[string]bool{"111": true})
	if len(read.Rows) != keywordReportRowCap || !read.Truncated {
		t.Errorf("rows=%d truncated=%v", len(read.Rows), read.Truncated)
	}
	if read.ConversionsComplete || read.Rows[0].Conversions != 0 {
		t.Errorf("an unreported conversion count must clear ConversionsComplete")
	}
	if read.Rows[0].Impressions < read.Rows[1].Impressions {
		t.Errorf("rows must be impressions-descending")
	}
}

func microsoftKeywordService(scope []string, reader *fakeKeywordReader, store domain.KeywordReportRepository) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(keywordOrch(scope, reader, store))
	return svc
}

func TestGetMicrosoftAdsKeywords_MapsTheSavedReport(t *testing.T) {
	asOf := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)
	row := kwRow("111", "9001", 100)
	row.Conversions = nil
	qs := int64(6)
	row.QualityScore = &qs
	store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Ready: &model.ReadyKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111"}, AsOf: asOf, Rows: []model.KeywordReportRow{row}, Partial: true,
	}}}
	// asOf is far older than accountReportFreshFor, so the read also submits a refresh.
	got, err := microsoftKeywordService([]string{"111"}, &fakeKeywordReader{account: "123", submitID: "k2"}, store).
		GetMicrosoftAdsKeywords(context.Background(), &conn.GetMicrosoftAdsKeywordsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("GetMicrosoftAdsKeywords: %v", err)
	}
	if got.Window != "last_30_days" || got.RowCount != 1 || got.Truncated {
		t.Errorf("envelope = %+v", got)
	}
	if got.MetricsAsOf == nil || *got.MetricsAsOf != "2026-10-05T14:30:00Z" || !got.MetricsPending {
		t.Errorf("as_of=%v pending=%v", got.MetricsAsOf, got.MetricsPending)
	}
	if !got.DataIncomplete {
		t.Errorf("data_incomplete must carry the served report's Partial flag")
	}
	if got.ConversionsComplete {
		t.Errorf("conversions_complete must be false when a row's count was not reported")
	}
	r := got.Rows[0]
	if r.CriterionID != "9001" || r.AdGroupID != "ag-111" || r.CampaignID != "111" || r.CostMicros != 1_500_000 || r.QualityScore == nil || *r.QualityScore != 6 {
		t.Errorf("row = %+v", r)
	}
}

func TestGetMicrosoftAdsKeywords_RefusesUnservableWindowsAndSystemScope(t *testing.T) {
	r := &fakeKeywordReader{account: "123"}
	svc := microsoftKeywordService([]string{"111"}, r, &fakeKeywordStore{})
	for _, w := range []string{"yesterday", "last_14_days", "last_90_days"} {
		w := w
		_, err := svc.GetMicrosoftAdsKeywords(context.Background(), &conn.GetMicrosoftAdsKeywordsPayload{ProjectID: "p", Window: &w})
		br, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Errorf("window %s: got %T, want 400", w, err)
			continue
		}
		// The message lists ONLY the Microsoft windows — never one this read refuses.
		if br.Message != "window must be one of: last_30_days, last_7_days, last_month, this_month, today" {
			t.Errorf("window %s: message = %q", w, br.Message)
		}
	}
	if _, err := svc.GetMicrosoftAdsKeywords(context.Background(), &conn.GetMicrosoftAdsKeywordsPayload{ProjectID: model.SystemProjectID}); err == nil {
		t.Errorf("the reserved system scope must be refused")
	}
	if r.accounts != 0 {
		t.Errorf("a refused request must not reach the dispatcher")
	}
}

func TestGetMicrosoftAdsKeywords_ClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{domain.ErrKeywordInsightsUnsupported, "400"}, // MICROSOFT_METRICS_ENABLED off
		{domain.ErrMetricsWindowUnsupported, "400"},
		{domain.ErrNotFound, "404"}, // no connection of the project's own
		{domain.ErrCampaignAccountMismatch, "409"},
		{domain.ErrKeywordReportScopeTooLarge, "409"},
		{domain.ErrKeywordReportScopeInvalid, "409"},
		{domain.ErrServiceDefect, "500"},
		{errors.New("boom"), "503"},
	} {
		_, err := microsoftKeywordService([]string{"111"}, &fakeKeywordReader{accountErr: fmt.Errorf("x: %w", tc.err)}, &fakeKeywordStore{}).
			GetMicrosoftAdsKeywords(context.Background(), &conn.GetMicrosoftAdsKeywordsPayload{ProjectID: "p"})
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
			ok = ok && strings.Contains(su.Message, "keyword insights")
		}
		if !ok {
			t.Errorf("%v: got %T (%v), want %s", tc.err, err, err, tc.want)
		}
	}
}

// TestGetMicrosoftAdsKeywords_PinsTheNotConnectedMessages pins the exact wording of the two
// refusals lfx-self-serve's Microsoft keyword table reads as "not connected" rather than as a
// read failure (microsoft-keywords-table.component.ts, isNotConnectedError). Those messages
// carry no other discriminator, so rewording either one here changes what operators see on
// every project without a Microsoft connection — update that matcher in the same change.
func TestGetMicrosoftAdsKeywords_PinsTheNotConnectedMessages(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{domain.ErrKeywordInsightsUnsupported, "keyword and audience insights are not supported for this platform"},
		{domain.ErrNotFound, "no microsoft ads connection configured for this project"},
	} {
		_, err := microsoftKeywordService([]string{"111"}, &fakeKeywordReader{accountErr: fmt.Errorf("x: %w", tc.err)}, &fakeKeywordStore{}).
			GetMicrosoftAdsKeywords(context.Background(), &conn.GetMicrosoftAdsKeywordsPayload{ProjectID: "p"})
		var got string
		switch e := err.(type) {
		case *conn.BadRequestError:
			got = e.Message
		case *conn.NotFoundError:
			got = e.Message
		default:
			t.Errorf("%v: got %T (%v), want a 400 or 404", tc.err, err, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%v: message = %q, want %q", tc.err, got, tc.want)
		}
	}
}
