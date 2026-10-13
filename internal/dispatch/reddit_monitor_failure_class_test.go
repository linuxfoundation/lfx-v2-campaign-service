// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// The Reddit client's per-row failure class reaches the service layer through the dispatcher's
// mapping: a report refused with 403 arrives as an auth-classed FetchFailed row.
func TestReddit_ListAccountCampaignMetrics_CarriesFailureClass(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/reports") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"campaigns":[{"id":"t2_c1","name":"c","configured_status":"ACTIVE"}]}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
	)
	rows, err := d.ListAccountCampaignMetrics(context.Background(), "proj", model.ProviderRedditAds, "t2_acct", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaignMetrics: %v", err)
	}
	if len(rows) != 1 || !rows[0].FetchFailed || rows[0].FailureClass != model.MonitorFailureAuth {
		t.Fatalf("rows = %+v, want one FetchFailed row classed auth", rows)
	}
}
