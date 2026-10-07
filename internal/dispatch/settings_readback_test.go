// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The capability is reached by TYPE ASSERTION, so a drifted ReadSettings signature would
// silently turn a platform back into "unsupported". These make that a compile error.
var (
	_ service.SettingsReader = (*MicrosoftDispatcher)(nil)
	_ service.SettingsReader = (*MetaDispatcher)(nil)
	_ service.SettingsReader = (*RedditDispatcher)(nil)
	_ service.SettingsReader = (*TwitterDispatcher)(nil)
)

// settingsClock pins every readback's ReadAt.
var settingsClock = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

func pinnedSettingsClock() time.Time { return settingsClock }

// settingsRoute is one canned answer, keyed by request PATH.
type settingsRoute struct {
	status     int
	body       string
	retryAfter string
}

// settingsAPI is a fake platform API that answers by path and records every request — method,
// path and body — under a mutex. The handler never calls t.Fatal; the test goroutine reads the
// record only through requests(), after the call under test has returned.
type settingsAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []budgetRequest
}

func newSettingsAPI(t *testing.T, routes map[string]settingsRoute) *settingsAPI {
	t.Helper()
	a := &settingsAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.seen = append(a.seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		a.mu.Unlock()
		rep, ok := routes[r.URL.Path]
		if !ok {
			best := ""
			for k := range routes {
				if strings.HasSuffix(r.URL.Path, k) && len(k) > len(best) {
					best = k
				}
			}
			if best != "" {
				rep, ok = routes[best], true
			}
		}
		if !ok {
			rep = settingsRoute{status: http.StatusTeapot, body: `{}`}
		}
		w.Header().Set("Content-Type", "application/json")
		if rep.retryAfter != "" {
			w.Header().Set("Retry-After", rep.retryAfter)
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *settingsAPI) requests() []budgetRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]budgetRequest(nil), a.seen...)
}

// newSettingsTokenServer is an OAuth token endpoint that counts its calls atomically.
func newSettingsTokenServer(t *testing.T) (string, func() int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600, "token_type": "Bearer"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, n.Load
}

// assertSettingsReadOnly fails if any request could have mutated the platform. readPOST names
// the one POST path suffix that is a read (Microsoft's QueryByIds); "" allows none.
func assertSettingsReadOnly(t *testing.T, reqs []budgetRequest, readPOST string) {
	t.Helper()
	for _, r := range reqs {
		if r.Method == http.MethodGet {
			continue
		}
		if r.Method == http.MethodPost && readPOST != "" && strings.HasSuffix(r.Path, readPOST) {
			continue
		}
		t.Fatalf("the readback issued a non-read request %s %s — it must never mutate the platform", r.Method, r.Path)
	}
}

// fieldWant is one expected field: its recorded and upstream sides ("" means absent/nil) and
// its verdict.
type fieldWant struct {
	recorded, upstream string
	verdict            model.SettingsComparison
}

// assertSettingsFields pins the readback's EXACT field set and each field's two sides and
// verdict, and that the counts agree with the list.
func assertSettingsFields(t *testing.T, rb *model.CampaignSettingsReadback, want map[string]fieldWant) {
	t.Helper()
	if len(rb.Fields) != len(want) {
		t.Errorf("readback has %d fields, want %d: %+v", len(rb.Fields), len(want), rb.Fields)
	}
	var diverged, unknown int
	for name, w := range want {
		f := settingsField(t, rb, name)
		if got := derefOr(f.Recorded); got != w.recorded {
			t.Errorf("%s recorded = %q, want %q", name, got, w.recorded)
		}
		if got := derefOr(f.Upstream); got != w.upstream {
			t.Errorf("%s upstream = %q, want %q", name, got, w.upstream)
		}
		if f.Comparison != w.verdict {
			t.Errorf("%s verdict = %q, want %q", name, f.Comparison, w.verdict)
		}
		switch w.verdict {
		case model.SettingsDiverged:
			diverged++
		case model.SettingsUnknown:
			unknown++
		}
	}
	if rb.DivergedCount != diverged || rb.UnknownCount != unknown {
		t.Errorf("counts = (diverged %d, unknown %d), want (%d, %d)", rb.DivergedCount, rb.UnknownCount, diverged, unknown)
	}
}

func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// assertSettings503 pins that err reaches the handler's generic 503 arm: an error matching none
// of the sentinels the handler maps to a 4xx or 500.
func assertSettings503(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, s := range []error{
		domain.ErrPlatformCampaignAbsent, domain.ErrCampaignAccountMismatch, domain.ErrCampaignProvenanceUnknown,
		domain.ErrNotFound, domain.ErrConnectionNotUsable, domain.ErrSystemConnectionNotUsable,
		domain.ErrSystemConnectionMissing, domain.ErrAccountNotSelected, domain.ErrCredentialDecryptionFailed,
		domain.ErrSettingsReadbackUnsupported, domain.ErrCampaignUpstreamIdentityMismatch, service.ErrCampaignNotProvisioned,
	} {
		if errors.Is(err, s) {
			t.Fatalf("err %v matches %v, so it would not be answered 503", err, s)
		}
	}
}

// assertProvenanceUnknown pins the unknown-provenance 409: both sentinels, so mismatch callers
// keep matching while the handler's dedicated arm tells the two apart.
func assertProvenanceUnknown(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown joined with ErrCampaignAccountMismatch", err)
	}
}

// assertMismatch pins the account-mismatch 409 and that it is NOT reported as unknown
// provenance (which has a different remedy).
func assertMismatch(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) || errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Fatalf("err = %v, want ErrCampaignAccountMismatch alone", err)
	}
}

// assertUpstreamIdentityMismatch pins the 409 for a platform answer that contradicts the recorded
// identity while the connection IS the recorded account: its own sentinel, never the account
// mismatch (whose "reconnect the original account" remedy would be unactionable here).
func assertUpstreamIdentityMismatch(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrCampaignUpstreamIdentityMismatch) || errors.Is(err, domain.ErrCampaignAccountMismatch) || errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Fatalf("err = %v, want ErrCampaignUpstreamIdentityMismatch alone", err)
	}
}

// unresolvableConn is a connection reader that fails the test's expectation if consulted: the
// unknown-provenance guard must refuse before any credential is resolved.
type unresolvableConn struct{ consulted *atomic.Bool }

func (u unresolvableConn) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	u.consulted.Store(true)
	return nil, errors.New("the connection must not be resolved for a row with unknown provenance")
}

func (u unresolvableConn) Disconnected(context.Context, string, model.Provider) (bool, error) {
	u.consulted.Store(true)
	return false, nil
}

func dayPtr(s string) *time.Time {
	t, err := time.Parse(campaignDateLayout, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func floatPtr(f float64) *float64 { return &f }

func budgetTypePtr(b model.BudgetType) *model.BudgetType { return &b }

// cloneCampaign deep-copies a row through JSON-free field copies, so a test can prove the
// readback did not write back onto it.
func cloneCampaign(c *model.Campaign) *model.Campaign {
	out := *c
	if c.BudgetAmount != nil {
		v := *c.BudgetAmount
		out.BudgetAmount = &v
	}
	if c.BudgetType != nil {
		v := *c.BudgetType
		out.BudgetType = &v
	}
	if c.StartDate != nil {
		v := *c.StartDate
		out.StartDate = &v
	}
	if c.EndDate != nil {
		v := *c.EndDate
		out.EndDate = &v
	}
	out.Result = append(json.RawMessage(nil), c.Result...)
	return &out
}

func assertRowUntouched(t *testing.T, before, after *model.Campaign) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the readback wrote back onto the campaign row:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestOrchestrator_ReadCampaignSettings_MicrosoftMetaRedditXAreWired drives the REAL
// orchestrator with the four real dispatchers. A row with unknown provenance is refused by the
// dispatcher's own guard (409) — which is only reachable if the orchestrator's type assertion
// found a SettingsReader — and never by ErrSettingsReadbackUnsupported (400). No connection is
// resolved and nothing is contacted.
func TestOrchestrator_ReadCampaignSettings_MicrosoftMetaRedditXAreWired(t *testing.T) {
	var consulted atomic.Bool
	conn := unresolvableConn{consulted: &consulted}
	dispatchers := map[model.Provider]service.PlatformDispatcher{
		model.ProviderMicrosoftAds: NewMicrosoftDispatcher(conn, identityEncryptor{}),
		model.ProviderMetaAds:      NewMetaDispatcher(conn, identityEncryptor{}),
		model.ProviderRedditAds:    NewRedditDispatcher(conn, identityEncryptor{}),
		model.ProviderTwitterAds:   NewTwitterDispatcher(conn, identityEncryptor{}),
	}
	o := service.NewOrchestrator(nil, nil, dispatchers)
	for platform := range dispatchers {
		t.Run(string(platform), func(t *testing.T) {
			camp := &model.Campaign{ID: "c1", Platform: platform, PlatformCampaignID: "123"}
			_, err := o.ReadCampaignSettings(context.Background(), "proj", platform, camp)
			if errors.Is(err, domain.ErrSettingsReadbackUnsupported) {
				t.Fatalf("%s still answers ErrSettingsReadbackUnsupported: %v", platform, err)
			}
			assertProvenanceUnknown(t, err)
		})
	}
	if consulted.Load() {
		t.Fatal("a connection was resolved for a row with unknown provenance")
	}
}

// TestCompareNudgedStart pins the one place a both-sides-read start is reported `unknown`: a LATER
// upstream day within a day of dispatch, which the create path's own nudge can produce. An earlier
// day, or a later one far from dispatch, stays a real divergence; an equal day stays a match.
func TestCompareNudgedStart(t *testing.T) {
	created := time.Date(2026, 8, 1, 23, 58, 0, 0, time.UTC)
	at := func(s string) *time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	for _, tc := range []struct {
		name     string
		upstream *time.Time
		created  time.Time
		want     model.SettingsComparison
	}{
		{"same UTC day matches", at("2026-08-01T10:00:00-07:00"), created, model.SettingsMatch},
		{"next day at dispatch is the nudge", at("2026-08-02T00:08:00Z"), created, model.SettingsUnknown},
		{"an earlier day is a divergence", at("2026-07-31T00:00:00Z"), created, model.SettingsDiverged},
		{"a later day far from dispatch is a divergence", at("2026-08-09T00:00:00Z"), created, model.SettingsDiverged},
		// The nudge moves a start to dispatch time + minutes, so it can never land BEFORE the
		// creation day: a later-than-recorded day earlier than that is an edit, not a nudge.
		{"a later day before the creation day is a divergence", at("2026-08-02T00:00:00Z"), time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), model.SettingsDiverged},
		{"no CreatedAt cannot bound the nudge", at("2026-08-09T00:00:00Z"), time.Time{}, model.SettingsUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := compareNudgedStart(dayPtr("2026-08-01"), tc.upstream, tc.created)
			if f.Comparison != tc.want || derefOr(f.Recorded) != "2026-08-01" || f.Upstream == nil {
				t.Fatalf("got %+v (upstream %q), want verdict %s with both sides shown", f, derefOr(f.Upstream), tc.want)
			}
		})
	}
}

func TestSettingsAmountRenderers(t *testing.T) {
	m := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		got, want string
	}{
		{derefOr(settingsMicrosToUnits(m(2_500_000_000))), "2500.00"},
		{derefOr(settingsMicrosToUnits(m(2_500_004_000))), "2500.004"},
		{derefOr(settingsMicrosToUnits(nil)), ""},
		{formatMinorUnits(5000, 100), "50.00"},
		{formatMinorUnits(1000, 1), "1000.00"},
		{formatMinorUnits(5, 100), "0.05"},
		{formatMicrosoftBudgetAmount(75.25), "75.25"},
		{formatMicrosoftBudgetAmount(75.254), "75.254"},
		{formatMicrosoftBudgetAmount(10.1), "10.10"},
	} {
		if tc.got != tc.want {
			t.Errorf("rendered %q, want %q", tc.got, tc.want)
		}
	}
}
