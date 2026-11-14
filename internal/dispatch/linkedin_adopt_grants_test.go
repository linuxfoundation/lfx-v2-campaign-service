// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// grantAdoptStub answers the campaign read and the campaign list for whichever account a request
// addresses, echoing that account (or `campaignAccount` when set) as the campaign's `account`.
type grantAdoptStub struct {
	mu              sync.Mutex
	paths           []string
	campaignAccount string
}

func (s *grantAdoptStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	echo := s.campaignAccount
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 3 || segs[0] != "adAccounts" || segs[2] != "adCampaigns" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if echo == "" {
		echo = segs[1]
	}
	if len(segs) == 4 {
		_, _ = io.WriteString(w, `{"id":`+segs[3]+`,"account":"urn:li:sponsoredAccount:`+echo+`","name":"KubeCon NA — Hand built","status":"ACTIVE"}`)
		return
	}
	_, _ = io.WriteString(w, `{"elements":[{"id":312,"name":"KubeCon NA — Hand built","status":"ACTIVE"},{"id":313,"name":"OSS Summit — Paused","status":"PAUSED"}],"metadata":{}}`)
}

func (s *grantAdoptStub) drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.paths
	s.paths = nil
	return out
}

func grantAdoptDispatcher(t *testing.T, repo connReader) (*LinkedInDispatcher, *grantAdoptStub) {
	t.Helper()
	stub := &grantAdoptStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	return NewLinkedInDispatcher(repo, identityEncryptor{}, linkedin.WithBaseURL(srv.URL)), stub
}

// TestLinkedIn_AdoptFromASystemAccount pins #581 PR 5: with the flag on, an adoption naming one of
// the LF system row's accounts (a grant, or its default) is read through the system credential
// against THAT account — whatever connection the project has — and records that account, so the
// levers and the scoped monitor reach the campaign through the same grant afterwards.
func TestLinkedIn_AdoptFromASystemAccount(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	own := activeLinkedInConn(goodLinkedInCreds)
	own.ID, own.Version = "own-li", 1
	for name, repo := range map[string]*scopedConnReader{
		"no row of its own":          {rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}},
		"own row on another account": {rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow(), "cncf": own}},
		"disconnected its own row":   {rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}, tombstoned: map[string]bool{"cncf": true}},
	} {
		for _, account := range []string{"500928401", "538170226"} {
			t.Run(name+"/"+account, func(t *testing.T) {
				d, stub := grantAdoptDispatcher(t, repo)
				ref, err := d.LookupCampaign(model.WithAdoptionAccount(context.Background(), account), "cncf", model.ProviderLinkedInAds, "312")
				if err != nil || ref == nil {
					t.Fatalf("LookupCampaign = %+v, %v", ref, err)
				}
				if got := stub.drain(); len(got) != 1 || got[0] != "GET /adAccounts/"+account+"/adCampaigns/312" {
					t.Fatalf("requests = %v, want one read of account %s", got, account)
				}
				row := &model.Campaign{PlatformCampaignID: ref.ID, Result: ref.Result}
				if got := linkedInCreationAccountID(row); got != account {
					t.Fatalf("recorded account = %q, want %s", got, account)
				}
				// The campaign this records is one the levers can reach: a pause resolves the
				// system credential scoped to the same account.
				res, err := d.creds.resolveExisting(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInCreationAccountID(row))
				if err != nil || res.accountID != account {
					t.Fatalf("resolveExisting for the adopted row = %+v, %v; want account %s", res, err, account)
				}
			})
		}
	}
}

// TestLinkedIn_AdoptFromASystemAccountRefusals: the flag, the grant list and the campaign's own
// account each refuse, and the first two send nothing.
func TestLinkedIn_AdoptFromASystemAccountRefusals(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}}
	t.Run("flag off", func(t *testing.T) {
		t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "")
		d, stub := grantAdoptDispatcher(t, repo)
		if _, err := d.LookupCampaign(model.WithAdoptionAccount(context.Background(), "500928401"), "cncf", model.ProviderLinkedInAds, "312"); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
			t.Fatalf("err = %v, want ErrAccountNotManagedByConnection", err)
		}
		if _, err := d.ListAdoptableCampaigns(context.Background(), "cncf", model.ProviderLinkedInAds, "500928401"); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
			t.Fatalf("list err = %v, want ErrAccountNotManagedByConnection", err)
		}
		if got := stub.drain(); len(got) != 0 {
			t.Fatalf("sent %v with the flag off", got)
		}
	})
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	t.Run("ungranted account", func(t *testing.T) {
		d, stub := grantAdoptDispatcher(t, repo)
		if _, err := d.LookupCampaign(model.WithAdoptionAccount(context.Background(), "111111111"), "cncf", model.ProviderLinkedInAds, "312"); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
			t.Fatalf("err = %v, want ErrAccountNotManagedByConnection", err)
		}
		if _, err := d.ListAdoptableCampaigns(context.Background(), "cncf", model.ProviderLinkedInAds, "111111111"); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
			t.Fatalf("list err = %v, want ErrAccountNotManagedByConnection", err)
		}
		if got := stub.drain(); len(got) != 0 {
			t.Fatalf("an ungranted account was contacted: %v", got)
		}
	})
	t.Run("a project row's account is not a system account", func(t *testing.T) {
		// The project's own account is not addressable by name here: the grant list is the
		// boundary, and the project's own row is what adoption WITHOUT account_id reads.
		own := activeLinkedInConn(goodLinkedInCreds)
		d, stub := grantAdoptDispatcher(t, &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow(), "cncf": own}})
		if _, err := d.LookupCampaign(model.WithAdoptionAccount(context.Background(), own.AccountID), "cncf", model.ProviderLinkedInAds, "312"); !errors.Is(err, domain.ErrAccountNotManagedByConnection) {
			t.Fatalf("err = %v, want ErrAccountNotManagedByConnection", err)
		}
		if got := stub.drain(); len(got) != 0 {
			t.Fatalf("sent %v", got)
		}
	})
	t.Run("campaign reports another account", func(t *testing.T) {
		d, stub := grantAdoptDispatcher(t, repo)
		stub.campaignAccount = "538170226"
		ref, err := d.LookupCampaign(model.WithAdoptionAccount(context.Background(), "500928401"), "cncf", model.ProviderLinkedInAds, "312")
		if !errors.Is(err, domain.ErrCampaignAccountMismatch) || ref != nil {
			t.Fatalf("got %+v, %v; want ErrCampaignAccountMismatch", ref, err)
		}
	})
	t.Run("no system row", func(t *testing.T) {
		d, stub := grantAdoptDispatcher(t, &scopedConnReader{rows: map[string]*model.Connection{}})
		if _, err := d.LookupCampaign(model.WithAdoptionAccount(context.Background(), "500928401"), "cncf", model.ProviderLinkedInAds, "312"); !errors.Is(err, domain.ErrSystemConnectionMissing) {
			t.Fatalf("err = %v, want ErrSystemConnectionMissing", err)
		}
		if got := stub.drain(); len(got) != 0 {
			t.Fatalf("sent %v", got)
		}
	})
	t.Run("without account_id the project's own connection is still required", func(t *testing.T) {
		d, stub := grantAdoptDispatcher(t, repo)
		if _, err := d.LookupCampaign(context.Background(), "cncf", model.ProviderLinkedInAds, "312"); !errors.Is(err, domain.ErrAdoptionRequiresOwnConnection) {
			t.Fatalf("err = %v, want ErrAdoptionRequiresOwnConnection", err)
		}
		if got := stub.drain(); len(got) != 0 {
			t.Fatalf("sent %v", got)
		}
	})
}

// TestLinkedIn_ListAdoptableCampaigns: the list reads the named system account's ACTIVE/PAUSED
// campaigns, one walk, no analytics.
func TestLinkedIn_ListAdoptableCampaigns(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsAccountSelectionEnabled, "true")
	d, stub := grantAdoptDispatcher(t, &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: grantedSystemRow()}})
	rows, err := d.ListAdoptableCampaigns(context.Background(), "cncf", model.ProviderLinkedInAds, "500928401")
	if err != nil {
		t.Fatalf("ListAdoptableCampaigns: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "312" || rows[0].Status != "ACTIVE" || rows[1].ID != "313" || rows[1].Name != "OSS Summit — Paused" {
		t.Fatalf("rows = %+v", rows)
	}
	if got := stub.drain(); len(got) != 1 || got[0] != "GET /adAccounts/500928401/adCampaigns" {
		t.Fatalf("requests = %v, want one campaign-list read of the granted account", got)
	}
}
