// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestHubSpotProbe_RejectionNamesNoAccount pins the subject as account-free.
//
// probeSubject.where() renders accountID as "for account X" on a confirmed verdict. Seeding it
// with portal_id put that field into the operator-facing message for a probe that never checked
// an account — the same field this package documents as routing nothing, in the one message
// where an operator reads it as the thing that failed. HubSpot is the only platform with
// nothing to put there, because the token IS the account.
func TestHubSpotProbe_RejectionNamesNoAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := NewHubSpotDispatcher(
		fakeConnReader{conn: hubspotConnInPortal("8112310")},
		identityEncryptor{},
		fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL),
	)
	err := d.ProbeConnection(context.Background(), "tlf", model.ProviderHubSpot)
	if err == nil {
		t.Fatal("ProbeConnection = nil for a refused token")
	}
	if strings.Contains(err.Error(), "8112310") {
		t.Errorf("message %q names the configured portal_id as though it were the account that "+
			"failed; this probe checked no account", err)
	}
	if strings.Contains(err.Error(), "for account") {
		t.Errorf("message %q claims an account subject; HubSpot's probe has none", err)
	}
}

// TestHubSpotProbe_MismatchLogsOnlyTheDerivedPortal pins the SHAPE of the deep-link warning, not
// just that it fires.
//
// The line exists so whoever chases a dead app.hubspot.com link can see which portal those links
// actually resolve into — that value is derived from the token and appears nowhere else, so it
// has to be in the log. The CONFIGURED portal does not: it is the operator's own stored input,
// sitting on the connection row this same line names by project_id, so logging it copies
// operator-supplied data into the log stream for a diagnostic the row already answers. Dropping
// it was a security nit on PR #228; this test is what stops a later edit from "completing" the
// pair.
func TestHubSpotProbe_MismatchLogsOnlyTheDerivedPortal(t *testing.T) {
	srv := tokenInfoServer(t, `{"hubId":8112310}`)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	d := NewHubSpotDispatcher(
		fakeConnReader{conn: hubspotConnInPortal("99999999")},
		identityEncryptor{},
		fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL),
	)
	if err := d.ProbeConnection(context.Background(), "tlf", model.ProviderHubSpot); err != nil {
		t.Fatalf("ProbeConnection = %v, want nil: a stale portal_id is not a verdict", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "authenticated_portal_id=8112310") {
		t.Errorf("the mismatch warning does not carry the portal the token authenticates into; "+
			"without it the line reports a broken deep link and withholds where the links go:\n%s", logged)
	}
	if strings.Contains(logged, "configured_portal_id") || strings.Contains(logged, "99999999") {
		t.Errorf("the mismatch warning logged the operator-supplied portal_id; it is on the "+
			"connection row this line already names by project_id:\n%s", logged)
	}
}
