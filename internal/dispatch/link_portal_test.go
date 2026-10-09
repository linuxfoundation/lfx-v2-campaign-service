// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func portalLessTokenInfoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, hubSpotTokenInfoPath, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hubId":8112310}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const wantPortalLessListURL = hubspot.AppBaseURL + "/contacts/8112310/objectLists/29084/filters"

// The audience builder's client is what every audience-builder response (search, suppression
// lists, existing masters, compose, attach-existing) builds its list links with. Without the
// fallback a portal-less row built them all blank, and the BFF 500'd compose and attach on the
// blank master link.
func TestAudienceBuilderClient_PortalLessRowBuildsLinksFromTheToken(t *testing.T) {
	srv := portalLessTokenInfoServer(t)
	repo := &scopedConnReader{rows: map[string]*model.Connection{"tlf": portalLessHubSpotConn()}}
	b := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))

	client, _, err := b.client(context.Background(), "tlf")
	require.NoError(t, err)
	require.Equal(t, wantPortalLessListURL, client.ListURL("29084"))
}

// The dispatcher's client builds the links on created campaign assets.
func TestHubSpotDispatcherClient_PortalLessRowBuildsLinksFromTheToken(t *testing.T) {
	srv := portalLessTokenInfoServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: portalLessHubSpotConn()}, identityEncryptor{}, &fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL))

	client, err := d.resolveHubSpotClient(context.Background(), "tlf", model.ProviderHubSpot)
	require.NoError(t, err)
	require.Equal(t, wantPortalLessListURL, client.ListURL("29084"))
}
