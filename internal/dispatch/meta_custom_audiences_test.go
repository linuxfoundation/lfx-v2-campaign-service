// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The read goes to the connection's ad account and maps every row.
func TestMeta_ListCustomAudiencesReadsTheConnectionsAccount(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/act_777/customaudiences" {
			t.Errorf("unexpected request %s", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"111","name":"KubeCon","subtype":"LOOKALIKE","account_id":"777","approximate_count_lower_bound":5}]}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	res, called, err := d.ListMetaCustomAudiences(context.Background(), "cncf", model.ProviderMetaAds)
	if err != nil || !called {
		t.Fatalf("ListMetaCustomAudiences: called=%v err=%v", called, err)
	}
	if res.AccountID != "act_777" || len(res.Audiences) != 1 {
		t.Fatalf("res = %+v", res)
	}
	a := res.Audiences[0]
	if a.ID != "111" || a.Name != "KubeCon" || a.Subtype != "LOOKALIKE" || a.CountLower == nil || *a.CountLower != 5 || a.CountUpper != nil {
		t.Fatalf("row = %+v", a)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("%d requests, want 1", hits)
	}
}

// A project with no Meta connection of its own reads the LF system account, as its create would:
// the read uses the create's credential resolution, so the ids it lists are the ones a create on
// that account accepts.
func TestMeta_ListCustomAudiencesFollowsTheCreatesSystemFallback(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	sys := activeMetaConn(goodMetaCreds)
	sys.AccountID = "act_999"
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sys}}
	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	res, called, err := d.ListMetaCustomAudiences(context.Background(), "cncf", model.ProviderMetaAds)
	if err != nil || !called || res.AccountID != "act_999" || res.Audiences == nil {
		t.Fatalf("= %+v, called=%v, err=%v; want the system account's (empty) list", res, called, err)
	}
	if got, _ := path.Load().(string); got != "/act_999/customaudiences" {
		t.Fatalf("read %q, want /act_999/customaudiences", got)
	}
}

// With LFX_FORCE_SYSTEM_ADS_ACCOUNT on, a create goes to the LF system account even when the
// project has its own connection, so the picker must list THAT account's audiences.
func TestMeta_ListCustomAudiencesFollowsTheForcedSystemAccount(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	sys := activeMetaConn(goodMetaCreds)
	sys.AccountID = "act_999"
	repo := &scopedConnReader{rows: map[string]*model.Connection{"cncf": activeMetaConn(goodMetaCreds), model.SystemProjectID: sys}}
	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	res, _, err := d.ListMetaCustomAudiences(context.Background(), "cncf", model.ProviderMetaAds)
	if err != nil || res.AccountID != "act_999" {
		t.Fatalf("= %+v, err=%v; want the forced system account act_999", res, err)
	}
	if got, _ := path.Load().(string); got != "/act_999/customaudiences" {
		t.Fatalf("read %q, want /act_999/customaudiences", got)
	}
}

// Local refusals send nothing and report called=false: no connection, and a connection with no
// (or a malformed) ad account.
func TestMeta_ListCustomAudiencesLocalRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Meta request %s", r.URL.RequestURI())
	}))
	defer srv.Close()
	noAccount := activeMetaConn(goodMetaCreds)
	noAccount.AccountID = ""
	badAccount := activeMetaConn(goodMetaCreds)
	badAccount.AccountID = "act_7/7"
	for _, tc := range []struct {
		name string
		repo connReader
		want error
	}{
		{"no connection", fakeConnReader{err: domain.ErrNotFound}, domain.ErrNotFound},
		{"no ad account", fakeConnReader{conn: noAccount}, nil},
		{"malformed ad account", fakeConnReader{conn: badAccount}, domain.ErrConnectionNotUsable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewMetaDispatcher(tc.repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
			res, called, err := d.ListMetaCustomAudiences(context.Background(), "cncf", model.ProviderMetaAds)
			if err == nil || called || res != nil {
				t.Fatalf("= %+v, called=%v, err=%v; want a local refusal", res, called, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
