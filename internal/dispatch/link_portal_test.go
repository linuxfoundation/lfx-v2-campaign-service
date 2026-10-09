// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// portalLessHubSpotConn is the shape prod's LF system row had on 2026-10-09: an active HubSpot
// connection with a working token and no portal_id.
func portalLessHubSpotConn() *model.Connection {
	c := activeHubSpotConn(goodHubSpotCreds)
	c.ProviderConfig = map[string]string{}
	return c
}

func portalLessTokenInfoServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// t.Errorf, not require: FailNow must not run on the handler's goroutine.
		if r.URL.Path != hubSpotTokenInfoPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hubId":8112310}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const wantPortalLessListURL = hubspot.AppBaseURL + "/contacts/8112310/objectLists/29084/filters"

// The audience builder's client is what every audience-builder response (search, suppression
// lists, existing masters, compose, attach-existing) builds its list links with. Without the
// fallback a portal-less row built them all blank, and the BFF 500'd compose and attach on the
// blank master link.
func TestAudienceBuilderClient_PortalLessRowBuildsLinksFromTheToken(t *testing.T) {
	srv, _ := portalLessTokenInfoServer(t)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"tlf": portalLessHubSpotConn()}}
	b := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))

	client, _, err := b.client(context.Background(), "tlf")
	require.NoError(t, err)
	require.Equal(t, wantPortalLessListURL, client.ListURL("29084"))
}

// Capabilities answers without an authenticated call, so it must not pay for the lookup.
func TestAudienceExplorerCapabilities_MakesNoTokenInfoCall(t *testing.T) {
	srv, calls := portalLessTokenInfoServer(t)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"tlf": portalLessHubSpotConn()}}
	x := NewAudienceExplorer(NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL)), nil, nil, nil)

	require.True(t, x.Capabilities(context.Background(), "tlf").HubSpotConfigured)
	require.Zero(t, calls.Load(), "Capabilities made a token-info call")
}

// The wizard's entry point builds created and cloned emails' edit links.
func TestHubSpotDispatcherResolveEmailClient_PortalLessRowBuildsLinksFromTheToken(t *testing.T) {
	srv, _ := portalLessTokenInfoServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: portalLessHubSpotConn()}, identityEncryptor{}, &fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL))

	client, err := d.ResolveEmailClient(context.Background(), "tlf")
	require.NoError(t, err)
	require.Equal(t, wantPortalLessListURL, client.ListURL("29084"))
}

// The shared resolver also serves ReadMetrics, PreflightCreate, ProbeConnection and the monitor,
// none of which builds a link; a lookup there would only spend their budgets.
func TestHubSpotDispatcherSharedResolver_MakesNoTokenInfoCall(t *testing.T) {
	srv, calls := portalLessTokenInfoServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: portalLessHubSpotConn()}, identityEncryptor{}, &fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL))

	_, err := d.resolveHubSpotClient(context.Background(), "tlf", model.ProviderHubSpot)
	require.NoError(t, err)
	require.Zero(t, calls.Load(), "the shared resolver made a token-info call")
}

// Dispatch returns the cloned email's edit link on the campaign. On a portal-less row it must
// build that link from the portal assertAudiencePortal just verified, and must not ask again.
func TestHubSpotDispatch_PortalLessRowLinksFromTheVerifiedPortalInOneLookup(t *testing.T) {
	srv, rec := hubspotServer(t)
	d := NewHubSpotDispatcher(
		fakeConnReader{conn: portalLessHubSpotConn()},
		identityEncryptor{},
		fakeAudienceReader{auds: builtHubSpotAudienceInPortal("26724", nil, "8112310")},
		hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	require.NoError(t, err)
	require.NotNil(t, camp)

	var result struct {
		HubspotURL string `json:"hubspotUrl"`
	}
	require.NoError(t, json.Unmarshal(camp.Result, &result))
	require.Equal(t, hubspot.AppBaseURL+"/email/8112310/edit/999/settings", result.HubspotURL)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, 1, rec.tokenInfoCalls, "the link must reuse the guard's verified portal, not look it up again")
}
