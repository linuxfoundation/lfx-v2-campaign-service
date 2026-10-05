// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// msKeywordServer stubs the token host and the Reporting service (Submit, Poll, download),
// counting every Reporting call so a refusal test can prove nothing reached Microsoft.
type msKeywordServer struct {
	calls     atomic.Int32
	mu        sync.Mutex
	submitRaw []byte
	pollReply string
	zip       []byte
	// submitReject, when set, is returned by Submit as a 400 body.
	submitReject string
}

func newMSKeywordServer(t *testing.T) (*msKeywordServer, []microsoft.Option) {
	t.Helper()
	m := &msKeywordServer{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/GenerateReport/Submit"):
			if got := r.Header.Get("CustomerAccountId"); got != "1234567" {
				t.Errorf("CustomerAccountId = %q, want the connection's 1234567", got)
			}
			m.submitRaw = raw
			if m.submitReject != "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, m.submitReject)
				return
			}
			_, _ = io.WriteString(w, `{"ReportRequestId":"kr-1"}`)
		case strings.HasSuffix(r.URL.Path, "/GenerateReport/Poll"):
			_, _ = io.WriteString(w, strings.ReplaceAll(m.pollReply, "%URL%", api.URL+"/download?sig=x"))
		case r.URL.Path == "/download":
			_, _ = w.Write(m.zip)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	clock := func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }
	return m, []microsoft.Option{
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(api.URL),
		microsoft.WithReportingBaseURL(api.URL), microsoft.WithClock(clock),
	}
}

func keywordZip(t *testing.T, csvBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("KeywordPerformanceReport.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, csvBody)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// msScope builds a scope entry; account "" records no creation account (a legacy row).
func msScope(id, account string) model.ProjectCampaignScope {
	s := model.ProjectCampaignScope{PlatformCampaignID: id}
	if account != "" {
		s.Result = json.RawMessage(fmt.Sprintf(`{"accountId":%q}`, account))
	}
	return s
}

func msKeywordDispatcher(opts []microsoft.Option) *MicrosoftDispatcher {
	return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
}

var _ service.KeywordReportReader = (*MicrosoftDispatcher)(nil)

// Gate off: the same 400 as a platform with no keyword read, from every method, and nothing
// reaches Microsoft.
func TestMicrosoftKeywords_DisabledByDefault(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "")
	m, opts := newMSKeywordServer(t)
	d := msKeywordDispatcher(opts)
	scope := []model.ProjectCampaignScope{msScope("111", "1234567")}
	for name, call := range map[string]func() error{
		"account": func() error {
			_, err := d.KeywordReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days, scope)
			return err
		},
		"submit": func() error {
			_, err := d.SubmitKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", model.MetricsWindowLast30Days, scope)
			return err
		},
		"check": func() error {
			_, err := d.CheckKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1")
			return err
		},
	} {
		if err := call(); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
			t.Errorf("%s: err = %v, want ErrKeywordInsightsUnsupported", name, err)
		}
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("%d upstream calls with the gate off, want none", n)
	}
}

// Every refusal arm stops before any upstream call, on every method that could reach Microsoft.
func TestMicrosoftKeywords_RefusalsMakeNoUpstreamCall(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	tooMany := make([]model.ProjectCampaignScope, microsoft.MaxKeywordReportCampaigns+1)
	for i := range tooMany {
		tooMany[i] = msScope(fmt.Sprint(i+1), "")
	}
	own := []model.ProjectCampaignScope{msScope("111", "1234567")}
	cases := []struct {
		name    string
		window  model.MetricsWindow
		account string
		scope   []model.ProjectCampaignScope
		want    error
	}{
		{"empty scope", model.MetricsWindowLast30Days, "1234567", nil, microsoft.ErrKeywordReportScope},
		{"scope past the report ceiling", model.MetricsWindowLast30Days, "1234567", tooMany, domain.ErrKeywordReportScopeTooLarge},
		{"one campaign from another account", model.MetricsWindowLast30Days, "1234567",
			[]model.ProjectCampaignScope{msScope("111", "1234567"), msScope("222", "7654321")}, domain.ErrCampaignAccountMismatch},
		{"unsupported window", model.MetricsWindowYesterday, "1234567", own, domain.ErrMetricsWindowUnsupported},
		{"malformed stored campaign id", model.MetricsWindowLast30Days, "1234567",
			[]model.ProjectCampaignScope{msScope("111", "1234567"), msScope("0222", "")}, domain.ErrKeywordReportScopeInvalid},
		{"non-numeric stored campaign id", model.MetricsWindowLast30Days, "1234567",
			[]model.ProjectCampaignScope{msScope("abc", "")}, domain.ErrKeywordReportScopeInvalid},
		{"account the connection is not bound to", model.MetricsWindowLast30Days, "7654321", own, domain.ErrAccountNotManagedByConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, opts := newMSKeywordServer(t)
			d := msKeywordDispatcher(opts)
			_, serr := d.SubmitKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.account, tc.window, tc.scope)
			if !errors.Is(serr, tc.want) {
				t.Errorf("submit: err = %v, want %v", serr, tc.want)
			}
			if tc.account == "1234567" {
				_, aerr := d.KeywordReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window, tc.scope)
				if !errors.Is(aerr, tc.want) {
					t.Errorf("account: err = %v, want %v", aerr, tc.want)
				}
			} else if _, cerr := d.CheckKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.account, "kr-1"); !errors.Is(cerr, tc.want) {
				t.Errorf("check: err = %v, want %v", cerr, tc.want)
			}
			if n := m.calls.Load(); n != 0 {
				t.Errorf("%d upstream calls, want none for a refused read", n)
			}
		})
	}
}

// The provenance filter: an entry with no recorded account is "unknown, proceed"; the bound
// account is returned and the submitted scope is exactly the project's campaigns.
func TestMicrosoftKeywords_AccountAndSubmitScope(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	m, opts := newMSKeywordServer(t)
	d := msKeywordDispatcher(opts)
	scope := []model.ProjectCampaignScope{msScope("111", "1234567"), msScope("222", "")}
	acct, err := d.KeywordReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast7Days, scope)
	if err != nil || acct != "1234567" {
		t.Fatalf("KeywordReportAccount = %q, %v", acct, err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Fatalf("KeywordReportAccount made %d upstream calls, want none", n)
	}
	sub, err := d.SubmitKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, acct, model.MetricsWindowLast7Days, scope)
	if err != nil {
		t.Fatalf("SubmitKeywordReport: %v", err)
	}
	if sub.ReportID != "kr-1" || strings.Join(sub.CampaignIDs, ",") != "111,222" {
		t.Errorf("submission = %+v", sub)
	}
	if got := sub.WindowStart.Format("2006-01-02") + ".." + sub.WindowEnd.Format("2006-01-02"); got != "2026-09-29..2026-10-05" {
		t.Errorf("window = %s", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !bytes.Contains(m.submitRaw, []byte(`"Campaigns":[{"AccountId":"1234567","CampaignId":"111"},{"AccountId":"1234567","CampaignId":"222"}]`)) ||
		bytes.Contains(m.submitRaw, []byte(`AccountIds`)) {
		t.Errorf("submit scope must be exactly the project's campaigns: %s", m.submitRaw)
	}
}

// The system fallback is never consulted: only the project's own connection may be read.
func TestMicrosoftKeywords_RefusesSystemFallback(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: activeMicrosoftConn(goodMicrosoftCreds)},
	}, identityEncryptor{})
	_, err := d.KeywordReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast30Days, []model.ProjectCampaignScope{msScope("111", "")})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want domain.ErrNotFound", err)
	}
}

// Check maps each poll state, and normalises a finished report's rows onto the published
// vocabulary with the (ad group id, keyword id) handle intact.
func TestMicrosoftKeywords_CheckStates(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	for reply, want := range map[string]model.AccountReportStatus{
		`{"ReportRequestStatus":{"Status":"Pending"}}`: model.AccountReportPending,
		`{"ReportRequestStatus":{"Status":"Error"}}`:   model.AccountReportFailed,
	} {
		m, opts := newMSKeywordServer(t)
		m.pollReply = reply
		got, err := msKeywordDispatcher(opts).CheckKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1")
		if err != nil || got.Status != want || got.Rows != nil {
			t.Errorf("%s: got %+v, %v", reply, got, err)
		}
	}

	m, opts := newMSKeywordServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zip = keywordZip(t, `"CampaignId","CampaignName","AdGroupId","AdGroupName","KeywordId","Keyword","BidMatchType","KeywordStatus","QualityScore","Impressions","Clicks","Spend","ConversionsQualified"
"111","C","501","AG","9001","kubernetes","Exact","Active","8","100","5","2.50","1"
"111","C","501","AG","9002","k8s","Predictive","Paused","--","10","0","0","0"
`)
	got, err := msKeywordDispatcher(opts).CheckKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", "kr-1")
	if err != nil {
		t.Fatalf("CheckKeywordReport: %v", err)
	}
	if got.Status != model.AccountReportReady || len(got.Rows) != 2 {
		t.Fatalf("got %+v", got)
	}
	a, b := got.Rows[0], got.Rows[1]
	if a.KeywordID != "9001" || a.AdGroupID != "501" || a.CampaignID != "111" || a.MatchType != "EXACT" || a.Status != "ENABLED" || a.Spend != 2.5 || *a.QualityScore != 8 {
		t.Errorf("row 1 = %+v", a)
	}
	if b.MatchType != "UNKNOWN" || b.Status != "PAUSED" || b.QualityScore != nil {
		t.Errorf("row 2 = %+v", b)
	}
}

// One campaign held by two live rows (the scope query's DISTINCT includes the result blob, and
// Microsoft has no live-row uniqueness index) is sent ONCE and counted ONCE against the ceiling,
// while the provenance check still runs over every row.
func TestMicrosoftKeywords_ScopeIsDeduplicated(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	m, opts := newMSKeywordServer(t)
	d := msKeywordDispatcher(opts)
	dup := []model.ProjectCampaignScope{msScope("111", "1234567"), msScope("111", ""), msScope("222", "")}
	sub, err := d.SubmitKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567", model.MetricsWindowLast7Days, dup)
	if err != nil {
		t.Fatalf("SubmitKeywordReport: %v", err)
	}
	if strings.Join(sub.CampaignIDs, ",") != "111,222" {
		t.Errorf("CampaignIDs = %v, want each campaign once", sub.CampaignIDs)
	}
	m.mu.Lock()
	if n := bytes.Count(m.submitRaw, []byte(`"CampaignId":"111"`)); n != 1 {
		t.Errorf("campaign 111 sent %d times, want once: %s", n, m.submitRaw)
	}
	m.mu.Unlock()

	// The ceiling counts DISTINCT campaigns: 300 distinct plus duplicates is allowed.
	atCap := make([]model.ProjectCampaignScope, 0, microsoft.MaxKeywordReportCampaigns+10)
	for i := 1; i <= microsoft.MaxKeywordReportCampaigns; i++ {
		atCap = append(atCap, msScope(fmt.Sprint(i), ""))
	}
	for i := 1; i <= 10; i++ {
		atCap = append(atCap, msScope(fmt.Sprint(i), ""))
	}
	if _, err := d.KeywordReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast7Days, atCap); err != nil {
		t.Errorf("300 distinct campaigns with duplicate rows must fit the ceiling: %v", err)
	}

	// A duplicate row recording ANOTHER account still refuses the read.
	mixed := []model.ProjectCampaignScope{msScope("111", "1234567"), msScope("111", "7654321")}
	if _, err := d.KeywordReportAccount(context.Background(), "cncf", model.ProviderMicrosoftAds, model.MetricsWindowLast7Days, mixed); !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("err = %v, want ErrCampaignAccountMismatch from the duplicate row", err)
	}
}

// Microsoft refusing the campaign-only scope (2027) is the same refusal on every read: it is
// tagged permanent (a service defect), never left as a transient upstream error.
func TestMicrosoftKeywords_ScopeRejectionIsPermanent(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	for _, body := range []string{`{"Code":2027,"Message":"scope rejected"}`, `{"ErrorCode":"InvalidAccountThruCampaignReportScope"}`} {
		m, opts := newMSKeywordServer(t)
		m.submitReject = body
		_, err := msKeywordDispatcher(opts).SubmitKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567",
			model.MetricsWindowLast7Days, []model.ProjectCampaignScope{msScope("111", "")})
		if !errors.Is(err, domain.ErrServiceDefect) || !errors.Is(err, microsoft.ErrKeywordReportScopeRejected) {
			t.Errorf("%s: err = %v, want ErrServiceDefect wrapping ErrKeywordReportScopeRejected", body, err)
		}
	}
	// An unrelated rejection stays an ordinary (transient) error.
	m, opts := newMSKeywordServer(t)
	m.submitReject = `{"Code":105,"Message":"other"}`
	_, err := msKeywordDispatcher(opts).SubmitKeywordReport(context.Background(), "cncf", model.ProviderMicrosoftAds, "1234567",
		model.MetricsWindowLast7Days, []model.ProjectCampaignScope{msScope("111", "")})
	if err == nil || errors.Is(err, domain.ErrServiceDefect) {
		t.Errorf("err = %v, want a non-defect error", err)
	}
}
