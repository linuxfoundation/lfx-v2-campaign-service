// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
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
