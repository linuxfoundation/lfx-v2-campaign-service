// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// The three stored-connection defects every adapter detects, expressed once. Each mutation
// takes an otherwise-good active connection and breaks exactly one thing.
//
// The account-id case is deliberately in the same table rather than in a separate one: it
// reaches the caller through the same untagged path and produced the same wrong 503, and
// splitting it is what let the Google Ads fix cover discovery while leaving the read and
// toggle paths bare.
type connDefectCase struct {
	name   string
	mutate func(*model.Connection)
	want   error
}

func connDefectCases(incompleteCreds string) []connDefectCase {
	return []connDefectCase{
		{"inactive", func(c *model.Connection) { c.Status = model.StatusInactive }, domain.ErrConnectionInactive},
		{"undecodable blob", func(c *model.Connection) { c.EncryptedCredentials = []byte(`{`) }, domain.ErrCredentialsUndecodable},
		{"incomplete credentials", func(c *model.Connection) { c.EncryptedCredentials = []byte(incompleteCreds) }, domain.ErrCredentialsIncomplete},
		{"no account selected", func(c *model.Connection) { c.AccountID = "" }, domain.ErrAccountNotSelected},
	}
}

// assertConnectionDefectTagged is the whole point of this file, and each assertion pins a
// distinct regression:
//
//   - ErrConnectionNotUsable is what internal/service/brief.go classifies on. Without it the
//     error falls to the handler's default arm and answers 503 — "the platform did not
//     respond" about a platform that was never contacted, with a remedy (retry) that no
//     amount of waiting can satisfy, since only a human editing the connection can clear it.
//   - The reason sentinel is what the handler logs (unusableConnectionReason's fixed
//     vocabulary), so it has to survive ALONGSIDE the status marker, not instead of it.
//   - No *json.SyntaxError anywhere in the chain. This is checked with errors.As rather than
//     a substring match on Error(): a cause still in the chain is reachable by any
//     errors.As-walking logger even when the top-level string looks clean, and the 503 arm
//     logs the chain through safeErrSummary. The bytes it would carry are derived from the
//     DECRYPTED credential blob — encoding/json quotes its input, so *json.SyntaxError names
//     the offending character and *json.UnmarshalTypeError names the field.
func assertConnectionDefectTagged(t *testing.T, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a connection error, got nil")
	}
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable): without it the "+
			"handler falls to its default arm and answers 503 for a condition only an edit to "+
			"the connection can clear", err)
	}
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want errors.Is(err, %v): the reason sentinel is what the handler "+
			"logs, and it must survive alongside the status marker", err, want)
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		t.Errorf("error = %v retains a *json.SyntaxError in its chain: that error is derived from "+
			"the DECRYPTED credential blob and encoding/json quotes its input, so any "+
			"errors.As-walking logger can reach credential-derived bytes for exactly the "+
			"connection whose credentials are malformed. Drop the cause, do not wrap it", err)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		t.Errorf("error = %v retains a *json.UnmarshalTypeError in its chain: it names the "+
			"credential field it was reading. Drop the cause, do not wrap it", err)
	}
}

// unreachablePlatform fails the test if the adapter contacts the platform. A connection with
// one of these defects must be rejected locally — reaching the network at all would mean the
// validation ran too late to have prevented the 503 in the first place.
func unreachablePlatform(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("reached the platform at %s with an unusable connection", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReddit_UnusableConnectionIsTaggedOnToggle(t *testing.T) {
	srv := unreachablePlatform(t)
	for _, tc := range connDefectCases(`{"ClientID":"cid","ClientSecret":"sec"}`) {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeRedditConn(goodRedditCreds)
			tc.mutate(conn)
			d := NewRedditDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				reddit.WithBaseURL(srv.URL),
			)
			camp := &model.Campaign{Platform: model.ProviderRedditAds, PlatformCampaignID: "t2_c"}
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunPaused)
			assertConnectionDefectTagged(t, err, tc.want)
		})
	}
}

func TestTwitter_UnusableConnectionIsTaggedOnToggle(t *testing.T) {
	srv := unreachablePlatform(t)
	for _, tc := range connDefectCases(`{"ConsumerKey":"ck","ConsumerSecret":"cs","AccessToken":"at"}`) {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeTwitterConn(goodTwitterCreds)
			tc.mutate(conn)
			d := NewTwitterDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				twitter.WithBaseURL(srv.URL),
			)
			camp := &model.Campaign{Platform: model.ProviderTwitterAds, PlatformCampaignID: "abc"}
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, camp, model.CampaignRunPaused)
			assertConnectionDefectTagged(t, err, tc.want)
		})
	}
}

func TestMicrosoft_UnusableConnectionIsTaggedOnToggle(t *testing.T) {
	srv := unreachablePlatform(t)
	for _, tc := range connDefectCases(`{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev"}`) {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeMicrosoftConn(goodMicrosoftCreds)
			tc.mutate(conn)
			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				microsoft.WithBaseURL(srv.URL),
			)
			camp := &model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"}
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused)
			assertConnectionDefectTagged(t, err, tc.want)
		})
	}
}
