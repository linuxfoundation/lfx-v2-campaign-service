// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

func TestParseArgs(t *testing.T) {
	base := []string{"-provider", "reddit-ads", "-project", "my-project", "-campaign", "t3_abc"}
	cases := []struct {
		name    string
		args    []string
		wantErr string // substring; "" means success
		check   func(t *testing.T, o *options)
	}{
		{name: "defaults", args: base, check: func(t *testing.T, o *options) {
			if o.provider != model.ProviderRedditAds || o.project != "my-project" || o.campaign != "t3_abc" ||
				o.window != model.MetricsWindowLast7Days || o.raw || o.timeout != defaultTimeout {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "raw and window", args: append(append([]string{}, base...), "-raw", "-window", "this_month"), check: func(t *testing.T, o *options) {
			if !o.raw || o.window != model.MetricsWindowThisMonth {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "lf-system alias", args: []string{"-provider", "microsoft-ads", "-project", "lf-system", "-campaign", "123"}, check: func(t *testing.T, o *options) {
			if o.project != model.SystemProjectID {
				t.Errorf("project = %q, want %q", o.project, model.SystemProjectID)
			}
		}},
		{name: "twitter", args: []string{"-provider", "twitter-ads", "-project", "p", "-campaign", "abc1"}},
		{name: "linkedin", args: []string{"-provider", "linkedin-ads", "-project", "p", "-campaign", "1"}},
		{name: "meta", args: []string{"-provider", "meta-ads", "-project", "p", "-campaign", "1"}},
		{name: "missing provider", args: base[2:], wantErr: "-provider is required"},
		{name: "google-ads refused", args: []string{"-provider", "google-ads", "-project", "p", "-campaign", "1"}, wantErr: "google-ads is not supported"},
		{name: "hubspot refused", args: []string{"-provider", "hubspot", "-project", "p", "-campaign", "1"}, wantErr: "not supported"},
		{name: "unknown provider", args: []string{"-provider", "tiktok-ads", "-project", "p", "-campaign", "1"}, wantErr: "not supported"},
		{name: "missing project", args: []string{"-provider", "reddit-ads", "-campaign", "1"}, wantErr: "-project is required"},
		{name: "missing campaign", args: []string{"-provider", "reddit-ads", "-project", "p"}, wantErr: "-campaign is required"},
		{name: "bad window", args: append(append([]string{}, base...), "-window", "last_90_days"), wantErr: "not a metrics window"},
		{name: "bad timeout", args: append(append([]string{}, base...), "-timeout", "0s"), wantErr: "-timeout must be positive"},
		{name: "stray positional", args: []string{"-provider", "reddit-ads", "typo", "-project", "p", "-campaign", "1"}, wantErr: `unexpected argument "typo"`},
		{name: "unknown flag", args: append(append([]string{}, base...), "-account", "x"), wantErr: "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseArgs(tc.args, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Errorf("err %T is not a usage error, so it would not exit 2", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, o)
			}
		})
	}
}

func TestParseArgs_HelpIsNotAnError(t *testing.T) {
	if _, err := parseArgs([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
}

// A refused provider must exit 2 before any database or network is touched.
func TestRun_GoogleAdsRefusedBeforeAnyBackend(t *testing.T) {
	var stderr bytes.Buffer
	a := &app{stdout: io.Discard, stderr: &stderr, openBackend: func(context.Context) (*backend, error) {
		t.Fatal("the backend must not be opened for a refused provider")
		return nil, nil
	}}
	code := a.run(context.Background(), []string{"-provider", "google-ads", "-project", "p", "-campaign", "1"})
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "google-ads is not supported") {
		t.Errorf("stderr = %q, want the google-ads refusal", stderr.String())
	}
}

// metricsProviderFor picks the provider whose metrics client sends path, so the original
// metrics allowlist table keeps its meaning under the per-provider list.
func metricsProviderFor(path string) model.Provider {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/reporting/"), strings.Contains(p, "oauth2"), strings.Contains(p, "campaignmanagement"):
		return model.ProviderMicrosoftAds
	case strings.Contains(p, "accesstoken"):
		return model.ProviderLinkedInAds
	}
	return model.ProviderRedditAds
}

func TestAllowedRequest(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/api/v3/ad_accounts/t2_a/campaigns", true},
		{http.MethodHead, "/anything", true},
		{http.MethodPost, "/api/v1/access_token", true},
		{http.MethodPost, "/common/oauth2/v2.0/token", true},
		{http.MethodPost, "/oauth/v2/accessToken", true},
		{http.MethodPost, "/api/v3/ad_accounts/t2_a/reports", true},
		{http.MethodPost, "/Reporting/v13/GenerateReport/Submit", true},
		{http.MethodPost, "/Reporting/v13/GenerateReport/Poll", true},
		{http.MethodPost, "/api/v3/ad_accounts/t2_a/campaigns", false},
		{http.MethodPost, "/CampaignManagement/v13/Campaigns", false},
		{http.MethodPatch, "/api/v3/campaigns/t3_c", false},
		{http.MethodPut, "/12/accounts/abc/campaigns/x", false},
		{http.MethodDelete, "/api/v3/campaigns/t3_c", false},
	}
	for _, tc := range cases {
		if got := allowedRequest(readMetrics, metricsProviderFor(tc.path), tc.method, tc.path); got != tc.want {
			t.Errorf("allowedRequest(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// A write must be refused in-process: the platform never sees it.
func TestRecorder_RefusesWritesWithoutSending(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	hc := newRecorder(newSecretSet(), readMetrics, model.ProviderRedditAds).client()
	for _, m := range []string{http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodPost} {
		req, _ := http.NewRequest(m, srv.URL+"/api/v3/campaigns/t3_c", strings.NewReader(`{}`))
		resp, err := hc.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s was not refused", m)
			continue
		}
		if !errors.Is(err, errReadOnly) {
			t.Errorf("%s: err = %v, want errReadOnly", m, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the platform received %d write request(s); want 0", n)
	}
}

func TestDisplayURL_WithholdsSecretQueryValues(t *testing.T) {
	u, _ := url.Parse("https://user:pw@blob.example.net/report.zip?sv=2020&sig=SUPERSECRETSIG&entity_ids=abc")
	got := displayURL(u)
	for _, leak := range []string{"SUPERSECRETSIG", "user:pw", "pw@"} {
		if strings.Contains(got, leak) {
			t.Errorf("displayURL leaked %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "entity_ids=abc") || !strings.Contains(got, "sig="+redacted) {
		t.Errorf("displayURL = %s, want the non-secret parameters kept and sig withheld", got)
	}
}

func TestRedactBody(t *testing.T) {
	body := `{"data":{"access_token":"tok-123456789","nested":[{"client_secret":"cs-abcdefgh","clicks":4}],
		"ReportDownloadUrl":"https://blob.example.net/r.zip?sig=SIGVALUE123"}}`
	got := redactBody([]byte(body), nil)
	for _, leak := range []string{"tok-123456789", "cs-abcdefgh", "SIGVALUE123"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactBody leaked %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, `"clicks": 4`) {
		t.Errorf("redactBody dropped a non-secret field:\n%s", got)
	}
}

// A real SAS URL has several parameters; JSON re-encoding must not split the URL run at an
// escaped `&` and leave the signature behind (Microsoft's Poll response names the report file).
func TestRedactBody_MultiParameterSignedURL(t *testing.T) {
	body := `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":` +
		`"https://bingads.blob.core.windows.net/r/abc.zip?sv=2020-08-04&st=2026&se=2026&sr=b&sp=r&sig=SUPERSECRETSIG%3D"}}`
	got := redactBody([]byte(body), nil)
	for _, leak := range []string{"SUPERSECRETSIG", "sig=", "/r/abc.zip"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactBody leaked %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, `"Status": "Success"`) {
		t.Errorf("redactBody dropped a non-secret field:\n%s", got)
	}
}

func TestRedactBody_KeepsIntegerPrecision(t *testing.T) {
	if got := redactBody([]byte(`{"id":9007199254740993}`), nil); !strings.Contains(got, "9007199254740993") {
		t.Errorf("redactBody rounded an int64: %s", got)
	}
}

// A known credential with a quote and a backslash, echoed under an ordinary key, is printed
// JSON-escaped; it must be scrubbed from the DECODED string before re-encoding (PR #362 review).
func TestRedactBody_ScrubsAnEscapedEchoedCredential(t *testing.T) {
	const cred = `abc"def\ghi123`
	s := newSecretSet()
	s.add(cred)
	body, _ := json.Marshal(map[string]any{"echo": "upstream saw " + cred, "clicks": 4})
	got := redactBody(body, s)
	for _, leak := range []string{`abc\"def`, `def\\ghi`, `abc"def`} {
		if strings.Contains(got, leak) {
			t.Errorf("redactBody leaked %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "upstream saw "+redacted) || !strings.Contains(got, `"clicks": 4`) {
		t.Errorf("redactBody = %s, want the credential replaced and the rest kept", got)
	}
}

// The secret set also knows a credential's JSON-escaped form, so the final scrub catches it in
// any printed text — an error message quoting a JSON body, for one.
func TestSecretSet_ScrubsTheJSONEscapedForm(t *testing.T) {
	const cred = `abc"def\ghi<123>`
	s := newSecretSet()
	s.add(cred)
	for _, in := range []string{`x abc\"def\\ghi\u003c123\u003e y`, `x abc\"def\\ghi<123> y`} {
		if got := s.scrub(in); strings.Contains(got, "def") {
			t.Errorf("scrub(%q) = %q, want the escaped credential replaced", in, got)
		}
	}
}

// A JSON-looking body cut off mid-value (the capture cap, an interrupted read) is withheld:
// as raw text, an escaped `\u0026` ends SnapshotText's URL run before `sig=` (PR #362 review).
func TestRedactBody_WithholdsIncompleteJSON(t *testing.T) {
	body := `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"https://bingads.blob.core.windows.net/r/a.zip?sv=2020\u0026sig=NEWSIGNATURE`
	got := redactBody([]byte(body), nil)
	if strings.Contains(got, "NEWSIGNATURE") || got != withheldIncompleteJSON {
		t.Errorf("redactBody = %q, want the incomplete body withheld", got)
	}
	if got := redactBody([]byte("plain text error"), nil); got != "plain text error" {
		t.Errorf("redactBody(plain text) = %q, want it kept", got)
	}
}

// A secret-named query value is withheld from the displayed URL AND recorded, so a failure body
// that echoes it on its own (a download naming its `sig`) is scrubbed too (PR #362 review).
func TestRecorder_ScrubsAnEchoedSecretQueryValue(t *testing.T) {
	const sig = "DOWNLOADSIG0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AuthenticationFailed</Code><Detail>signature "+sig+" did not match</Detail></Error>")
	}))
	defer srv.Close()
	secrets := newSecretSet()
	rec := newRecorder(secrets, readMetrics, model.ProviderMicrosoftAds)
	resp, err := rec.client().Get(srv.URL + "/r/a.zip?sv=2020&sig=" + sig)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var out bytes.Buffer
	rec.print(&scrubWriter{w: &out, secrets: secrets})
	if strings.Contains(out.String(), sig) || !strings.Contains(out.String(), "AuthenticationFailed") {
		t.Errorf("print leaked the signature or dropped the body:\n%s", out.String())
	}
}

// blockingTransport holds every request until release is closed.
type blockingTransport struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	close(b.started)
	<-b.release
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"late-token-0123456"}`)), Header: http.Header{}, Request: req}, nil
}

// A token refresh can outlive the read that started it. print must render it as pending, under
// the same lock the late answer is recorded with — run with -race (PR #362 review).
func TestRecorder_PrintsAPendingExchangeWithoutARace(t *testing.T) {
	bt := &blockingTransport{started: make(chan struct{}), release: make(chan struct{})}
	secrets := newSecretSet()
	rec := newRecorder(secrets, readMetrics, model.ProviderMicrosoftAds)
	rec.base = bt
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/api/v1/access_token", strings.NewReader("grant_type=refresh_token"))
		if resp, err := rec.client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-bt.started
	var out bytes.Buffer
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		rec.print(&scrubWriter{w: &out, secrets: secrets})
	}()
	close(bt.release)
	<-finished
	<-done
	if !strings.Contains(out.String(), "-> pending") && !strings.Contains(out.String(), "-> 200") {
		t.Errorf("print = %s, want the exchange as pending or answered", out.String())
	}
	if strings.Contains(out.String(), "-> 0") {
		t.Errorf("print rendered an unfinished request as status 0:\n%s", out.String())
	}
	out.Reset()
	rec.print(&scrubWriter{w: &out, secrets: secrets})
	if !strings.Contains(out.String(), "-> 200") {
		t.Errorf("print after the answer = %s, want -> 200", out.String())
	}
}

// Deterministic: printed while the request is still out, the exchange reads as pending.
func TestRecorder_PrintsAnUnansweredExchangeAsPending(t *testing.T) {
	bt := &blockingTransport{started: make(chan struct{}), release: make(chan struct{})}
	secrets := newSecretSet()
	rec := newRecorder(secrets, readMetrics, model.ProviderMicrosoftAds)
	rec.base = bt
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := rec.client().Get("https://example.invalid/api/v3/me"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-bt.started
	var out bytes.Buffer
	rec.print(&scrubWriter{w: &out, secrets: secrets})
	close(bt.release)
	<-done
	if !strings.Contains(out.String(), "-> pending") {
		t.Errorf("print = %s, want the unanswered exchange shown as pending", out.String())
	}
}

// A Microsoft report download is a zip; -raw prints its files, redacted.
func TestRenderResponse_UnpacksZip(t *testing.T) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("report.csv")
	_, _ = io.WriteString(f, "CampaignId,Impressions,Clicks,Spend\n123,10,2,1.50\nsee https://x.example/p?sig=ZIPSECRETSIG&a=b\n")
	_ = zw.Close()
	b := &cappedBuffer{}
	b.write(zbuf.Bytes())
	got := renderResponse(b, nil)
	if !strings.Contains(got, "zip entry report.csv") || !strings.Contains(got, "123,10,2,1.50") {
		t.Errorf("zip not unpacked:\n%s", got)
	}
	if strings.Contains(got, "ZIPSECRETSIG") {
		t.Errorf("zip entry leaked a signature:\n%s", got)
	}
}

func TestLookupCampaign(t *testing.T) {
	opts := &options{provider: model.ProviderRedditAds, project: "p", campaign: "t3_c"}
	one := model.ProjectCampaignScope{PlatformCampaignID: "t3_c", Result: json.RawMessage(`{"accountId":"t2_a"}`)}
	other := model.ProjectCampaignScope{PlatformCampaignID: "t3_c", Result: json.RawMessage(`{"accountId":"t2_b"}`)}
	unrelated := model.ProjectCampaignScope{PlatformCampaignID: "t3_x"}

	c, found, err := lookupCampaign(context.Background(), &backend{campaigns: &fakeScopes{scopes: []model.ProjectCampaignScope{unrelated, one}}}, opts)
	if err != nil || !found || string(c.Result) != string(one.Result) || c.PlatformCampaignID != "t3_c" {
		t.Errorf("single match: c=%+v found=%v err=%v", c, found, err)
	}
	c, found, err = lookupCampaign(context.Background(), &backend{campaigns: &fakeScopes{scopes: []model.ProjectCampaignScope{unrelated}}}, opts)
	if err != nil || found || len(c.Result) != 0 {
		t.Errorf("no match: c=%+v found=%v err=%v", c, found, err)
	}
	if _, _, err = lookupCampaign(context.Background(), &backend{campaigns: &fakeScopes{scopes: []model.ProjectCampaignScope{one, other}}}, opts); err == nil ||
		!strings.Contains(err.Error(), "different account provenance") {
		t.Errorf("ambiguous rows must be refused, err = %v", err)
	}
}

func TestRecorder_RefusesOversizedTokenResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxCapture+1))
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/access_token", strings.NewReader("grant_type=x"))
	resp, err := newRecorder(newSecretSet(), readMetrics, model.ProviderRedditAds).client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("an oversized token response must be refused, not truncated")
	}
}

func TestSecretSet_ScrubsDecryptedAndHeaderValues(t *testing.T) {
	s := newSecretSet()
	s.addCredentialDocument([]byte(`{"ClientSecret":"the-client-secret","nested":{"RefreshToken":"r/efresh+token"}}`))
	s.addHeaderValue(`OAuth oauth_consumer_key="ck-1234567", oauth_signature="c2lnbmF0dXJl%3D"`)
	in := "a the-client-secret b r%2Fefresh%2Btoken c ck-1234567 d signature c2lnbmF0dXJl= e"
	got := s.scrub(in)
	for _, leak := range []string{"the-client-secret", "r%2Fefresh%2Btoken", "ck-1234567", "c2lnbmF0dXJl="} {
		if strings.Contains(got, leak) {
			t.Errorf("scrub leaked %q: %s", leak, got)
		}
	}
}

// --- an end-to-end run against a fake Reddit -------------------------------------------------

const (
	fakeClientSecret = "fake-client-secret-0123456789"
	fakeRefreshToken = "fake-refresh-token-abcdefghij"
	fakeAccessToken  = "fake-access-token-ZYXWVUTSRQ"
)

type fakeConns struct{ conn *model.Connection }

func (f *fakeConns) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	if projectID != "my-project" {
		return nil, domain.ErrNotFound
	}
	return f.conn, nil
}

func (f *fakeConns) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// fakeScopes is the campaign reader: the scope lists are what the reads here call. Any other
// reader method would panic on the nil embedded interface, which is the point — a read that
// started calling one would fail its test rather than be faked into passing.
type fakeScopes struct {
	domain.CampaignReader
	scopes []model.ProjectCampaignScope
}

func (f *fakeScopes) ListProjectPlatformDeletedCampaignIDs(context.Context, string, model.Provider) ([]model.ProjectCampaignScope, error) {
	return nil, nil
}

func (f *fakeScopes) ListProjectPlatformMonitorCampaignIDs(context.Context, string, model.Provider) ([]model.ProjectCampaignScope, error) {
	return f.scopes, nil
}

func (f *fakeScopes) ListProjectPlatformCampaignIDs(context.Context, string, model.Provider) ([]model.ProjectCampaignScope, error) {
	return f.scopes, nil
}

// fakeReddit serves the token exchange and the report query, and deliberately echoes the
// credentials back in its report response — the worst case a redactor has to survive.
func fakeReddit(t *testing.T, reportStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fakeAccessToken, "token_type": "bearer", "expires_in": 3600})
	})
	mux.HandleFunc("/api/v3/ad_accounts/t2_acct/reports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writes.Add(1)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fakeAccessToken {
			t.Errorf("report request Authorization = %q", got)
		}
		w.WriteHeader(reportStatus)
		_, _ = io.WriteString(w, `{"data":{"metrics":[{"campaign_id":"t3_c","impressions":1200,"clicks":30,"spend":4560000}],`+
			`"echo":"secret=`+fakeClientSecret+` refresh=`+fakeRefreshToken+` bearer=`+fakeAccessToken+`"}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &writes
}

func redditTestApp(t *testing.T, srv *httptest.Server, stdout, stderr io.Writer) *app {
	t.Helper()
	enc, err := crypto.NewAESGCM(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := enc.Encrypt([]byte(`{"ClientID":"fake-client-id-000","ClientSecret":"` + fakeClientSecret +
		`","RefreshToken":"` + fakeRefreshToken + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	conn := &model.Connection{
		Provider: model.ProviderRedditAds, AccountID: "t2_acct", EncryptedCredentials: ciphertext,
		Status: model.StatusActive, ProviderConfig: map[string]string{"conversion_pixel_id": "t2_pixel"},
	}
	be := &backend{
		conns:     &fakeConns{conn: conn},
		campaigns: &fakeScopes{scopes: []model.ProjectCampaignScope{{PlatformCampaignID: "t3_c", Result: json.RawMessage(`{"accountId":"t2_acct"}`)}}},
		enc:       enc,
		close:     func() {},
	}
	return &app{
		stdout: stdout, stderr: stderr,
		openBackend: func(context.Context) (*backend, error) { return be, nil },
		endpoints:   endpoints{api: srv.URL + "/api/v3", token: srv.URL + "/api/v1/access_token"},
	}
}

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{fakeClientSecret, fakeRefreshToken, fakeAccessToken, "fake-client-id-000"} {
		if strings.Contains(out, s) {
			t.Errorf("output leaked %q:\n%s", s, out)
		}
	}
}

func TestRun_RedditAgainstFakePlatform_IgnoresFlagAndRedacts(t *testing.T) {
	// The deployment flag is OFF; the command must read anyway, in this process only.
	t.Setenv(constants.EnvRedditMetricsEnabled, "false")
	t.Setenv(constants.EnvForceSystemAdsAccount, "")
	srv, writes := fakeReddit(t, http.StatusOK)
	var stdout, stderr bytes.Buffer
	code := redditTestApp(t, srv, &stdout, &stderr).run(context.Background(),
		[]string{"-provider", "reddit-ads", "-project", "my-project", "-campaign", "t3_c", "-window", "last_7_days", "-raw"})
	all := stdout.String() + stderr.String()
	if testing.Verbose() {
		t.Log(all)
	}
	if code != exitOK {
		t.Fatalf("exit = %d, output:\n%s", code, all)
	}
	assertNoSecrets(t, all)
	for _, want := range []string{
		`"impressions": 1200`, `"clicks": 30`, `"cost_micros": 4560000`, `"conversions": null`,
		"raw upstream exchanges: 2", "OAuth token exchange: request and response bodies withheld",
		"/ad_accounts/t2_acct/reports -> 200", `"filter": "campaign:id==t3_c"`, redacted,
		"bearer=" + redacted, // the token exchange's token_type is not itself treated as a secret
		"row:      found",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if writes.Load() != 0 {
		t.Errorf("the fake platform saw %d non-read request(s)", writes.Load())
	}
}

// LFX_FORCE_SYSTEM_ADS_ACCOUNT does not redirect metrics reads (they follow the row's recorded
// account), so the note must not claim the system account is used (PR #362 review).
func TestRun_ForceSystemFlagNoteDoesNotClaimTheSystemAccount(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "")
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	srv, _ := fakeReddit(t, http.StatusOK)
	var stdout, stderr bytes.Buffer
	_ = redditTestApp(t, srv, &stdout, &stderr).run(context.Background(),
		[]string{"-provider", "reddit-ads", "-project", "my-project", "-campaign", "t3_c"})
	if !strings.Contains(stdout.String(), "metrics reads ignore it") || strings.Contains(stdout.String(), "is used whatever") {
		t.Errorf("stdout = %s, want the note to say metrics reads ignore the flag", stdout.String())
	}
}

func TestRun_RedditUpstreamFailure_PrintsRawAndRedactsError(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "")
	t.Setenv(constants.EnvForceSystemAdsAccount, "")
	srv, _ := fakeReddit(t, http.StatusBadRequest)
	var stdout, stderr bytes.Buffer
	code := redditTestApp(t, srv, &stdout, &stderr).run(context.Background(),
		[]string{"-provider", "reddit-ads", "-project", "my-project", "-campaign", "t3_c", "-raw"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	all := stdout.String() + stderr.String()
	assertNoSecrets(t, all)
	if !strings.Contains(stdout.String(), "-> 400") || !strings.Contains(stderr.String(), "read failed") {
		t.Errorf("want the failing exchange and the error reported:\n%s", all)
	}
}

// The live database session must refuse writes. Runs only where the Makefile's live-database
// tests run.
func TestOpenReadOnlyPool_RefusesWrites(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := openReadOnlyPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(), "CREATE TABLE verify_metrics_must_not_exist (x int)")
	if err == nil {
		t.Fatal("a write succeeded on the read-only session")
	}
	// read_only_sql_transaction specifically: a permission or schema error would also fail the
	// CREATE without proving the session is read-only.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("want SQLSTATE 25006 (read_only_sql_transaction), got %v", err)
	}
}
