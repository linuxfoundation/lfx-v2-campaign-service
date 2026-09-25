// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// tokenInfoServer answers the private-app token-info endpoint with one portal and nothing else.
func tokenInfoServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hubSpotTokenInfoPath {
			t.Errorf("connection probe called %s %s; it must make the token-info read and nothing else",
				r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hubspotConnInPortal(portalID string) *model.Connection {
	c := activeHubSpotConn(goodHubSpotCreds)
	c.ProviderConfig = map[string]string{"portal_id": portalID}
	return c
}

// TestHubSpotProbe_PortalIDMismatchIsNotAVerdict is the fix for a probe that failed WORKING
// connections.
//
// portal_id is not the account this connection dispatches to. Nothing routes on it: its only
// readers interpolate it into app.hubspot.com deep links for assets that already exist, and the
// portal a campaign actually lands in is the token's own, derived by the client — the same
// reasoning the ReadMetrics provenance guard already records. So a connection whose portal_id is
// blank, stale, or simply never filled in is a connection that WORKS, and the message the old
// arm produced ("the credential authenticates but does not reach account X") was false as well
// as failing.
func TestHubSpotProbe_PortalIDMismatchIsNotAVerdict(t *testing.T) {
	cases := []struct {
		name       string
		configured string
	}{
		{name: "stale configured portal", configured: "99999999"},
		{name: "matching configured portal", configured: "8112310"},
		{name: "no configured portal", configured: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tokenInfoServer(t, `{"hubId":8112310}`)
			d := NewHubSpotDispatcher(
				fakeConnReader{conn: hubspotConnInPortal(tc.configured)},
				identityEncryptor{},
				fakeAudienceReader{},
				hubspot.WithBaseURL(srv.URL),
			)
			if err := d.ProbeConnection(context.Background(), "tlf", model.ProviderHubSpot); err != nil {
				t.Fatalf("ProbeConnection = %v, want nil: the token authenticated, and portal_id routes nothing", err)
			}
		})
	}
}

// TestHubSpotProbe_RejectedTokenStillFails guards the other direction — the fix above must not
// turn the probe into one that passes unconditionally. A token HubSpot itself refuses is the
// case this endpoint exists for.
func TestHubSpotProbe_RejectedTokenStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"status":"error","message":"expired authentication"}`)
	}))
	defer srv.Close()

	d := NewHubSpotDispatcher(
		fakeConnReader{conn: hubspotConnInPortal("8112310")},
		identityEncryptor{},
		fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL),
	)
	if err := d.ProbeConnection(context.Background(), "tlf", model.ProviderHubSpot); err == nil {
		t.Fatal("ProbeConnection = nil for a token HubSpot refused; the endpoint would report a dead connection as healthy")
	}
}
