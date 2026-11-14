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
)

// TestMetaDispatcher_ListMetaLeadForms_ReadsTheConnectionPage: the forms of the connection's
// page_id are listed and the Page is reported.
func TestMetaDispatcher_ListMetaLeadForms_ReadsTheConnectionPage(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"1111","name":"Interest","status":"ACTIVE"}]}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	res, called, err := d.ListMetaLeadForms(context.Background(), "cncf", model.ProviderMetaAds)
	if err != nil || !called {
		t.Fatalf("ListMetaLeadForms = %+v, %v, %v", res, called, err)
	}
	if res.PageID != "987654321" || len(res.Forms) != 1 || res.Forms[0] != (model.MetaLeadForm{ID: "1111", Name: "Interest", Status: "ACTIVE"}) {
		t.Errorf("res = %+v", res)
	}
	if got := path.Load(); got != "/987654321/leadgen_forms" {
		t.Errorf("path = %v, want the connection Page's leadgen_forms edge", got)
	}
}

// TestMetaDispatcher_ListMetaLeadForms_RefusesLocally: a connection without a page_id is not usable
// and no connection is not found; neither reaches Meta.
func TestMetaDispatcher_ListMetaLeadForms_RefusesLocally(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	noPage := activeMetaConn(goodMetaCreds)
	noPage.ProviderConfig = map[string]string{"page_id": "  "}
	d := NewMetaDispatcher(fakeConnReader{conn: noPage}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	if _, called, err := d.ListMetaLeadForms(context.Background(), "cncf", model.ProviderMetaAds); called || !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrProviderConfigInvalid) {
		t.Errorf("no page_id: called %v err %v, want a local not-usable refusal", called, err)
	}
	gone := NewMetaDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	if _, called, err := gone.ListMetaLeadForms(context.Background(), "cncf", model.ProviderMetaAds); called || !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no connection: called %v err %v, want ErrNotFound", called, err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("requests = %d, want none", n)
	}
}

// TestMetaDispatcher_ListMetaLeadForms_UpstreamFailure: a Graph error reaches the caller with called
// true.
func TestMetaDispatcher_ListMetaLeadForms_UpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"(#200) Requires pages_manage_ads","code":200}}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	if res, called, err := d.ListMetaLeadForms(context.Background(), "cncf", model.ProviderMetaAds); res != nil || !called || err == nil {
		t.Fatalf("ListMetaLeadForms = %+v, %v, %v; want an upstream error", res, called, err)
	}
}
