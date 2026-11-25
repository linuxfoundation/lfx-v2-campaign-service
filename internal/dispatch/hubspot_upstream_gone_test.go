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
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// The dispatcher satisfies the orchestrator's optional reuse check. Asserted at compile time for
// the same reason as the SearchEmails assertion in hubspot_emails_test.go: a signature drift would
// otherwise make the orchestrator's type assertion fail silently, and a draft deleted in HubSpot
// could once again never be staged again.
var _ interface {
	UpstreamGone(context.Context, string, *model.Campaign) (bool, error)
} = (*HubSpotDispatcher)(nil)

// presenceServer answers the portal lookup with testPortalID and a read of email 4242 with
// emailStatus, counting the email reads.
func presenceServer(t *testing.T, emailStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	reads := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case accountDetailsPath:
			_, _ = io.WriteString(w, testPortalResponse)
		case "/marketing/v3/emails/4242":
			reads.Add(1)
			w.WriteHeader(emailStatus)
			if emailStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"id":"4242","name":"Synthetic Summit 2026 — b1","state":"DRAFT"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"error","message":"synthetic"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, reads
}

func presenceDispatcherFor(srv *httptest.Server) *HubSpotDispatcher {
	return NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))
}

func TestHubSpot_UpstreamGoneOnlyOnADefinite404(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantGone bool
		wantErr  bool
	}{
		{name: "deleted in HubSpot", status: http.StatusNotFound, wantGone: true},
		{name: "still there", status: http.StatusOK, wantGone: false},
		// Any other answer settles nothing: the orchestrator keeps reusing the row.
		{name: "refused", status: http.StatusForbidden, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, reads := presenceServer(t, tc.status)

			gone, err := presenceDispatcherFor(srv).UpstreamGone(context.Background(), "proj-1", stagedEmail())

			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if gone != tc.wantGone {
				t.Errorf("gone = %v, want %v", gone, tc.wantGone)
			}
			if reads.Load() != 1 {
				t.Errorf("email read %d times, want 1", reads.Load())
			}
		})
	}
}

// The email id means nothing outside the portal that minted it, so a row whose portal is unknown,
// or a token now in another portal, cannot be judged: a 404 there would say "not in this portal",
// not "deleted". Both refuse before reading the email.
func TestHubSpot_UpstreamGoneRefusesWhereThePortalCannotBeTrusted(t *testing.T) {
	unrecorded := stagedEmail()
	unrecorded.Result = json.RawMessage(`{"id":"4242"}`)
	moved := stagedEmail()
	moved.Result = json.RawMessage(`{"id":"4242","portalId":"999"}`)

	for _, tc := range []struct {
		name    string
		c       *model.Campaign
		wantErr error
	}{
		{name: "no recorded portal", c: unrecorded, wantErr: domain.ErrCampaignProvenanceUnknown},
		{name: "token in another portal", c: moved, wantErr: domain.ErrCampaignAccountMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, reads := presenceServer(t, http.StatusNotFound)

			gone, err := presenceDispatcherFor(srv).UpstreamGone(context.Background(), "proj-1", tc.c)

			if gone || !errors.Is(err, tc.wantErr) {
				t.Fatalf("gone = %v, err = %v; want false and %v", gone, err, tc.wantErr)
			}
			if reads.Load() != 0 {
				t.Errorf("email read %d times, want 0", reads.Load())
			}
		})
	}
}

// A row with no platform id never reached HubSpot, so there is nothing to look up.
func TestHubSpot_UpstreamGoneIgnoresARowWithNoEmail(t *testing.T) {
	srv, reads := presenceServer(t, http.StatusNotFound)
	c := stagedEmail()
	c.PlatformCampaignID = ""

	gone, err := presenceDispatcherFor(srv).UpstreamGone(context.Background(), "proj-1", c)

	if gone || err != nil {
		t.Fatalf("gone = %v, err = %v; want false, nil", gone, err)
	}
	if reads.Load() != 0 {
		t.Errorf("email read %d times, want 0", reads.Load())
	}
}
