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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestVerifyLinkedInGrant pins the install-time check (#581): the pairing LinkedIn records must
// match, a confirmed disagreement refuses, and a credential that cannot act refuses before any
// request is made.
func TestVerifyLinkedInGrant(t *testing.T) {
	ctx := context.Background()
	t.Run("agreement passes", func(t *testing.T) {
		srv := linkedInAccountsServer(t, &requestRecorder{}, `{"id":123456789,"reference":"urn:li:organization:987654321"}`)
		if err := VerifyLinkedInGrant(ctx, []byte(goodLinkedInCreds), "123456789", "987654321", linkedin.WithBaseURL(srv.URL)); err != nil {
			t.Fatalf("VerifyLinkedInGrant: %v, want nil", err)
		}
	})
	t.Run("a different org refuses", func(t *testing.T) {
		srv := linkedInAccountsServer(t, &requestRecorder{}, `{"id":123456789,"reference":"urn:li:organization:111111111"}`)
		if err := VerifyLinkedInGrant(ctx, []byte(goodLinkedInCreds), "123456789", "987654321", linkedin.WithBaseURL(srv.URL)); err == nil {
			t.Fatal("VerifyLinkedInGrant accepted an account whose page is another org's")
		}
	})
	// A grant is refused unless LinkedIn itself names orgID for the account: no reference, a
	// person-scoped one or a malformed one states no organization, and the connection test's
	// permissive nil for those must not reach the installer.
	for name, elem := range map[string]string{
		"missing reference":       `{"id":123456789}`,
		"empty reference":         `{"id":123456789,"reference":""}`,
		"person-scoped reference": `{"id":123456789,"reference":"urn:li:person:AbCdEf"}`,
		"malformed reference":     `{"id":123456789,"reference":"urn:li:organization:98765x"}`,
		"mismatched reference":    `{"id":123456789,"reference":"urn:li:organization:111111111"}`,
	} {
		t.Run(name+" refuses", func(t *testing.T) {
			srv := linkedInAccountsServer(t, &requestRecorder{}, elem)
			err := VerifyLinkedInGrant(ctx, []byte(goodLinkedInCreds), "123456789", "987654321", linkedin.WithBaseURL(srv.URL))
			if err == nil {
				t.Fatal("VerifyLinkedInGrant accepted a pairing LinkedIn does not state")
			}
		})
	}
	// The connection test keeps its permissive reading of the same responses.
	for name, elem := range map[string]string{
		"missing reference":       `{"id":123456789}`,
		"person-scoped reference": `{"id":123456789,"reference":"urn:li:person:AbCdEf"}`,
		"malformed reference":     `{"id":123456789,"reference":"urn:li:organization:98765x"}`,
	} {
		t.Run(name+" stays inconclusive for the connection test", func(t *testing.T) {
			srv := linkedInAccountsServer(t, &requestRecorder{}, elem)
			c := linkedin.NewClient(linkedin.Credentials{AccessToken: "tok"}, linkedin.RuntimeConfig{}, linkedin.WithBaseURL(srv.URL))
			if err := c.VerifyAccountOrgReference(ctx, "123456789", "987654321"); err != nil {
				t.Fatalf("VerifyAccountOrgReference = %v, want the permissive nil", err)
			}
		})
	}
	for name, plaintext := range map[string]string{"not json": "nope", "no token": `{"AccessToken":"  "}`} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyLinkedInGrant(ctx, []byte(plaintext), "1", "2"); err == nil {
				t.Fatal("an unusable credential verified a grant")
			}
		})
	}
}

// grantedSystemRow is the LF system LinkedIn row with its default account and one grant (#581).
func grantedSystemRow() *model.Connection {
	return &model.Connection{
		ID: "sys-li", Version: 3, ProjectID: model.SystemProjectID,
		Provider: model.ProviderLinkedInAds, AccountID: "538170226",
		EncryptedCredentials: []byte(goodLinkedInCreds),
		ProviderConfig:       map[string]string{"org_id": "208777"},
		Status:               model.StatusActive,
		LinkedInGrants:       []model.LinkedInAccountGrant{{AccountID: "500928401", OrgID: "12893459", Label: "CNCF"}},
	}
}

func liCampaignIn(account string) *model.Campaign {
	return &model.Campaign{PlatformCampaignID: "555", Result: []byte(`{"accountId":"` + account + `"}`)}
}

type grantToggleReq = struct{ method, path, restli, status string }

func grantToggleDispatcher(t *testing.T, repo connReader) (*LinkedInDispatcher, chan grantToggleReq) {
	t.Helper()
	gotCh := make(chan grantToggleReq, 32)
	srv := httptest.NewServer(linkedinToggleHandler(t, gotCh))
	t.Cleanup(srv.Close)
	return NewLinkedInDispatcher(repo, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) })), gotCh
}

// campaignPaths drains the requests that addressed the campaign itself.
func campaignPaths(gotCh chan grantToggleReq) []string {
	var out []string
	for {
		select {
		case r := <-gotCh:
			if r.method != http.MethodGet && strings.Contains(r.path, "/adCampaigns/555") {
				// Every lever here is a pause: anything else addressed the campaign wrongly.
				out = append(out, r.path+" "+r.restli+" "+r.status)
			}
		default:
			return out
		}
	}
}

// TestLinkedIn_PauseReachesACampaignInAGrantedAccount pins step 2 of #581, "never create what you
// cannot pause": a campaign recorded in one of the system row's GRANTED accounts is paused through
// the system credential against THAT account, whichever arm of resolveExisting gets there — a
// project with no row of its own, one whose own row names another account, and one that
// disconnected. An account that is neither the row's nor granted still gets the provenance
// refusal and no request.
func TestLinkedIn_PauseReachesACampaignInAGrantedAccount(t *testing.T) {
	own := activeLinkedInConn(goodLinkedInCreds)
	own.ID, own.Version = "own-li", 1
	for name, repo := range map[string]*scopedConnReader{
		"no row of its own":          {rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}},
		"own row on another account": {rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow(), "cncf": own}},
		"disconnected its own row":   {rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}, tombstoned: map[string]bool{"cncf": true}},
	} {
		t.Run(name, func(t *testing.T) {
			d, gotCh := grantToggleDispatcher(t, repo)
			if err := d.ToggleStatus(context.Background(), "cncf", model.ProviderLinkedInAds, liCampaignIn("500928401"), model.CampaignRunPaused); err != nil {
				t.Fatalf("pause on a granted account: %v", err)
			}
			paths := campaignPaths(gotCh)
			if len(paths) != 1 || !strings.Contains(paths[0], "/adAccounts/500928401/") || !strings.HasSuffix(paths[0], " PARTIAL_UPDATE PAUSED") {
				t.Fatalf("pause addressed %v, want the granted account 500928401", paths)
			}
			err := d.ToggleStatus(context.Background(), "cncf", model.ProviderLinkedInAds, liCampaignIn("111111111"), model.CampaignRunPaused)
			// A disconnected project answers its own no-connection error (resolveExisting's
			// "the project's error stands"); the others hit the provenance guard.
			want := domain.ErrCampaignAccountMismatch
			if name == "disconnected its own row" {
				want = domain.ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatalf("pause on an ungranted account err = %v, want %v", err, want)
			}
			if p := campaignPaths(gotCh); len(p) != 0 {
				t.Fatalf("an ungranted account was contacted: %v", p)
			}
		})
	}
}

// TestLinkedIn_GrantedAndDefaultAccountsDoNotShareAClient: one system row now serves several
// accounts, and the cached client names exactly one. Pausing a default-account campaign and then a
// granted-account one through the SAME dispatcher must address each campaign's own account.
func TestLinkedIn_GrantedAndDefaultAccountsDoNotShareAClient(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}
	d, gotCh := grantToggleDispatcher(t, repo)
	for _, account := range []string{"538170226", "500928401", "538170226"} {
		if err := d.ToggleStatus(context.Background(), "cncf", model.ProviderLinkedInAds, liCampaignIn(account), model.CampaignRunPaused); err != nil {
			t.Fatalf("pause on %s: %v", account, err)
		}
		if p := campaignPaths(gotCh); len(p) != 1 || !strings.Contains(p[0], account) {
			t.Fatalf("pause recorded on %s addressed %v", account, p)
		}
	}
}

// TestGrantScopedHonoursOnlyTheSystemRow: a grant is honoured on the LF system row alone, the
// copy carries the grant's org, and the cached original's config is never written through.
func TestGrantScopedHonoursOnlyTheSystemRow(t *testing.T) {
	grants := []model.LinkedInAccountGrant{{AccountID: "500928401", OrgID: "12893459"}}
	base := &resolved{accountID: "538170226", providerConfig: map[string]string{"org_id": "208777"}, grants: grants}
	if _, ok := base.grantScoped("500928401"); ok {
		t.Fatal("a project-owned resolution honoured a grant")
	}
	base.fromSystem = true
	got, ok := base.grantScoped("500928401")
	if !ok || got.accountID != "500928401" || got.providerConfig["org_id"] != "12893459" || got.grantAccount != "500928401" {
		t.Fatalf("grantScoped = %+v, %v", got, ok)
	}
	if base.providerConfig["org_id"] != "208777" || base.accountID != "538170226" {
		t.Fatalf("grantScoped wrote through the original: %+v", base)
	}
	if _, ok := base.grantScoped("111"); ok {
		t.Fatal("an ungranted account was scoped")
	}
}

// TestLinkedIn_BrokenSystemRowIsBlamedForAGrantedCampaign: when the project scope and the system
// row both fail, the recorded account decides whose fault it is (systemCreated). A campaign in a
// granted account is the system row's, so the operator-facing system error is returned, not the
// project's "no connection" — which would send the project to fix something it does not own.
func TestLinkedIn_BrokenSystemRowIsBlamedForAGrantedCampaign(t *testing.T) {
	sys := grantedSystemRow()
	sys.EncryptedCredentials = nil
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sys}, tombstoned: map[string]bool{"cncf": true}}
	d, _ := grantToggleDispatcher(t, repo)
	err := d.ToggleStatus(context.Background(), "cncf", model.ProviderLinkedInAds, liCampaignIn("500928401"), model.CampaignRunPaused)
	if !errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Fatalf("err = %v, want the system row's error", err)
	}
}

// grantCreateServer answers a whole LinkedIn create and records every request path and body.
func grantCreateServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func grantCreateConfig(adAccountID string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{
		"adAccountId":"` + adAccountID + `",
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
}

func grantCreateDispatcher(srvURL string, repo connReader) *LinkedInDispatcher {
	return NewLinkedInDispatcher(repo, identityEncryptor{},
		linkedin.WithBaseURL(srvURL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

// TestLinkedIn_CreateInAGrantedAccount pins step 4 of #581: with account selection on, a create
// resolving to the LF system row may name one of its grants, and the campaign is created in that
// account under the GRANT's org (the dark post is authored by it), and records that account.
func TestLinkedIn_CreateInAGrantedAccount(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	srv, seen := grantCreateServer(t)
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}
	d := grantCreateDispatcher(srv.URL, repo)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, grantCreateConfig("500928401")); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("preflight on a granted account = %v, want not-applicable", err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, grantCreateConfig("500928401"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := linkedInCreationAccountID(camp); got != "500928401" {
		t.Fatalf("created row records account %q, want the granted 500928401", got)
	}
	var sawAccount, sawOrg bool
	for _, r := range seen() {
		if strings.Contains(r, "538170226") || strings.Contains(r, "organization:208777") {
			t.Fatalf("a request named the default account or org: %s", r)
		}
		sawAccount = sawAccount || strings.Contains(r, "sponsoredAccount:500928401") || strings.Contains(r, "/adAccounts/500928401/")
		sawOrg = sawOrg || strings.Contains(r, "organization:12893459")
	}
	if !sawAccount || !sawOrg {
		t.Fatalf("create did not use the granted account (%v) and its org (%v): %v", sawAccount, sawOrg, seen())
	}
}

// TestLinkedIn_AdAccountIDRefusals: every account a create may not name is a synchronous
// pre-flight 400 (ErrCreateConfigInvalid) and, if it reached Dispatch anyway, a pre-create refusal
// with nothing sent — the flag off, an ungranted account, and a grant on a project-owned row.
func TestLinkedIn_AdAccountIDRefusals(t *testing.T) {
	own := activeLinkedInConn(goodLinkedInCreds)
	own.ID, own.Version = "own-li", 1
	own.LinkedInGrants = []model.LinkedInAccountGrant{{AccountID: "500928401", OrgID: "12893459"}}
	for name, tc := range map[string]struct {
		flag    string
		repo    connReader
		account string
	}{
		"flag off":                     {flag: "", repo: &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}, account: "500928401"},
		"flag not exactly true":        {flag: "TRUE", repo: &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}, account: "500928401"},
		"ungranted account":            {flag: "true", repo: &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}, account: "111111111"},
		"grant on a project-owned row": {flag: "true", repo: &scopedConnReader{rows: map[string]*model.Connection{"cncf": own}}, account: "500928401"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, tc.flag)
			srv, seen := grantCreateServer(t)
			d := grantCreateDispatcher(srv.URL, tc.repo)
			if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, grantCreateConfig(tc.account)); !errors.Is(err, domain.ErrCreateConfigInvalid) {
				t.Fatalf("preflight = %v, want ErrCreateConfigInvalid", err)
			}
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, grantCreateConfig(tc.account))
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !isNoUpstreamCreate(err) {
				t.Fatalf("Dispatch err = %v, want a pre-create ErrCreateConfigInvalid", err)
			}
			if r := seen(); len(r) != 0 {
				t.Fatalf("a refused create sent %d request(s): %v", len(r), r)
			}
		})
	}
	t.Run("the connection's own account needs no flag", func(t *testing.T) {
		t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "")
		srv, _ := grantCreateServer(t)
		d := grantCreateDispatcher(srv.URL, &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}})
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, grantCreateConfig("538170226")); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Fatalf("preflight on the default account = %v", err)
		}
	})
}

// TestLinkedIn_PickersAndAccountsListFollowTheGrants: a picker naming a granted account reads that
// account, an ungranted one is ErrAccountNotManagedByConnection with nothing sent, and with
// selection on the system row's accounts list is its default and its grants, with no LinkedIn call.
func TestLinkedIn_PickersAndAccountsListFollowTheGrants(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
	}))
	t.Cleanup(srv.Close)
	drain := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := queries
		queries = nil
		return out
	}
	d := grantCreateDispatcher(srv.URL, &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}})

	// Selection off: a grant is not nameable from the pickers either, and nothing is sent.
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "")
	if _, _, err := d.ListConversionActions(model.WithPickerAccount(context.Background(), "500928401"), "cncf", model.ProviderLinkedInAds); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
		t.Fatalf("picker on a granted account with selection off err = %v", err)
	}
	if q := drain(); len(q) != 0 {
		t.Fatalf("a flag-off picker read sent %v", q)
	}

	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	res, _, err := d.ListConversionActions(model.WithPickerAccount(context.Background(), "500928401"), "cncf", model.ProviderLinkedInAds)
	if err != nil || res.AccountID != "500928401" {
		t.Fatalf("picker on a granted account = %+v, %v", res, err)
	}
	if q := drain(); len(q) == 0 || !strings.Contains(q[0], "500928401") {
		t.Fatalf("picker read %v, want the granted account", q)
	}
	if _, _, err := d.ListAudiences(model.WithPickerAccount(context.Background(), "111111111"), "cncf", model.ProviderLinkedInAds, LinkedInAdSegmentKind); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
		t.Fatalf("picker on an ungranted account err = %v", err)
	}
	if q := drain(); len(q) != 0 {
		t.Fatalf("an ungranted picker read sent %v", q)
	}

	accts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds)
	if err != nil || len(accts) != 2 || accts[0].ID != "538170226" || accts[1].ID != "500928401" || !strings.Contains(accts[1].Label, "CNCF") {
		t.Fatalf("accounts with selection on = %+v, %v; want the default then the grant", accts, err)
	}
	if q := drain(); len(q) != 0 {
		t.Fatalf("the grants list called LinkedIn: %v", q)
	}
}

// TestLinkedIn_CreateAccountReportsWhatACreateUses pins the create-account read (#581): it names
// the account and org a create resolves to (the project's own row, the LF system fallback, the
// forced-system redirect), lists exactly what selectLinkedInAccount accepts, and never calls
// LinkedIn.
func TestLinkedIn_CreateAccountReportsWhatACreateUses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the create-account read called LinkedIn: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	ownRow := &model.Connection{
		ID: "own-li", Version: 1, ProjectID: "cncf", Provider: model.ProviderLinkedInAds, AccountID: "111111111",
		EncryptedCredentials: []byte(goodLinkedInCreds), ProviderConfig: map[string]string{"org_id": "222"},
		Status: model.StatusInactive, Label: "CNCF own",
		// A project row never carries grants (the installer refuses it); if one did, it must not widen the list.
		LinkedInGrants: []model.LinkedInAccountGrant{{AccountID: "500928401", OrgID: "12893459"}},
	}
	ids := func(a *model.LinkedInCreateAccount) []string {
		out := make([]string, 0, len(a.Selectable))
		for _, g := range a.Selectable {
			out = append(out, g.AccountID+"/"+g.OrgID)
		}
		return out
	}
	for _, tc := range []struct {
		name, flag, force string
		rows              map[string]*model.Connection
		wantAcct, wantOrg string
		wantSystem        bool
		wantStatus        model.ConnectionStatus
		wantSelectable    []string
	}{
		{"system fallback, selection off", "", "", map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()},
			"538170226", "208777", true, model.StatusActive, []string{"538170226/208777"}},
		{"system fallback, selection on", "true", "", map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()},
			"538170226", "208777", true, model.StatusActive, []string{"538170226/208777", "500928401/12893459"}},
		{"own connection, selection on", "true", "", map[string]*model.Connection{"cncf": ownRow, model.SystemProjectID: grantedSystemRow()},
			"111111111", "222", false, model.StatusInactive, []string{"111111111/222"}},
		{"forced system overrides the own connection", "true", "true", map[string]*model.Connection{"cncf": ownRow, model.SystemProjectID: grantedSystemRow()},
			"538170226", "208777", true, model.StatusActive, []string{"538170226/208777", "500928401/12893459"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, tc.flag)
			t.Setenv(constants.EnvForceSystemAdsAccount, tc.force)
			d := grantCreateDispatcher(srv.URL, &scopedConnReader{rows: tc.rows})
			got, err := d.CreateAccount(context.Background(), "cncf", model.ProviderLinkedInAds)
			if err != nil {
				t.Fatalf("CreateAccount: %v", err)
			}
			if got.AccountID != tc.wantAcct || got.OrgID != tc.wantOrg || got.FromSystem != tc.wantSystem || got.Status != tc.wantStatus || got.SelectionEnabled != (tc.flag == "true") {
				t.Fatalf("CreateAccount = %+v, want account %s org %s system %v status %s", got, tc.wantAcct, tc.wantOrg, tc.wantSystem, tc.wantStatus)
			}
			if g := ids(got); strings.Join(g, ",") != strings.Join(tc.wantSelectable, ",") {
				t.Fatalf("selectable = %v, want %v", g, tc.wantSelectable)
			}
			// The list is the create's contract: every listed account is one selectLinkedInAccount
			// accepts, under the org the list reports.
			res, err := d.creds.resolve(context.Background(), "cncf", model.ProviderLinkedInAds)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			for _, g := range got.Selectable {
				sel, err := selectLinkedInAccount(res, g.AccountID)
				if err != nil || sel.accountID != g.AccountID || strings.TrimSpace(sel.providerConfig["org_id"]) != g.OrgID {
					t.Fatalf("listed %s/%s but a create resolves %+v, %v", g.AccountID, g.OrgID, sel, err)
				}
			}
		})
	}

	t.Run("a disconnected project is not served the system account", func(t *testing.T) {
		d := grantCreateDispatcher(srv.URL, &scopedConnReader{
			rows:       map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()},
			tombstoned: map[string]bool{"cncf": true},
		})
		if _, err := d.CreateAccount(context.Background(), "cncf", model.ProviderLinkedInAds); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("CreateAccount for a disconnected project = %v, want ErrNotFound", err)
		}
	})
	t.Run("no connection anywhere", func(t *testing.T) {
		d := grantCreateDispatcher(srv.URL, &scopedConnReader{rows: map[string]*model.Connection{}})
		if _, err := d.CreateAccount(context.Background(), "cncf", model.ProviderLinkedInAds); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("CreateAccount with no connection = %v, want ErrNotFound", err)
		}
	})
	// creds.resolve decrypts without validating, so an active LF row whose credential document is
	// `{}` (no access token) must still answer the system-connection 500 a create would hit, not a
	// 200 naming an account nothing can spend in. A project-owned row with the same document keeps
	// reporting its stored status: the project owns it and can repair it.
	t.Run("an unusable system credential is the system-connection error", func(t *testing.T) {
		sys := grantedSystemRow()
		sys.EncryptedCredentials = []byte(`{}`)
		d := grantCreateDispatcher(srv.URL, &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sys}})
		got, err := d.CreateAccount(context.Background(), "cncf", model.ProviderLinkedInAds)
		if !errors.Is(err, domain.ErrSystemConnectionNotUsable) || !errors.Is(err, domain.ErrCredentialsIncomplete) {
			t.Fatalf("CreateAccount on a system row with no access token = %+v, %v; want ErrSystemConnectionNotUsable (missing access token)", got, err)
		}
	})
	t.Run("an unusable project credential still reports its stored status", func(t *testing.T) {
		own := *ownRow
		own.EncryptedCredentials = []byte(`{}`)
		own.Status = model.StatusActive
		d := grantCreateDispatcher(srv.URL, &scopedConnReader{rows: map[string]*model.Connection{"cncf": &own, model.SystemProjectID: grantedSystemRow()}})
		got, err := d.CreateAccount(context.Background(), "cncf", model.ProviderLinkedInAds)
		if err != nil || got.FromSystem || got.AccountID != "111111111" || got.Status != model.StatusActive {
			t.Fatalf("CreateAccount on a project row with no access token = %+v, %v; want its stored account and status", got, err)
		}
	})
}
