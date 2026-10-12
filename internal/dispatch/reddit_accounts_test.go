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
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.AccountLister = (*RedditDispatcher)(nil)

// redditDiscoveryServer answers the two discovery calls: one business, whose ad accounts are
// accountsJSON (the body of the data array). The token endpoint is a separate server.
func redditDiscoveryServer(t *testing.T, rec *requestRecorder, accountsJSON string) []reddit.Option {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v3/me/businesses" {
			_, _ = io.WriteString(w, `{"data":[{"id":"biz1","name":"The Linux Foundation"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[`+accountsJSON+`]}`)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return []reddit.Option{reddit.WithBaseURL(api.URL + "/api/v3"), reddit.WithTokenURL(tok.URL)}
}

// The account-less connection is the one discovery exists to serve; dispatch refuses it.
func TestRedditListAccountsWorksWithoutASelectedAccount(t *testing.T) {
	rec := &requestRecorder{}
	opts := redditDiscoveryServer(t, rec, `{"id":"t2_gv9wtbfa","name":"LF Events","currency":"USD"}`)
	conn := activeRedditConn(goodRedditCreds)
	conn.AccountID = ""
	d := NewRedditDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, opts...)

	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderRedditAds)
	if err != nil {
		t.Fatalf("discovery must work on a connection with no account id: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "t2_gv9wtbfa" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].Label != "LF Events [USD] (The Linux Foundation)" {
		t.Errorf("label = %q", accounts[0].Label)
	}
}

// The request asks about the CREDENTIAL: the stored account id appears nowhere on the wire.
func TestRedditListAccountsAsksAboutTheCredentialNotTheAccount(t *testing.T) {
	rec := &requestRecorder{}
	opts := redditDiscoveryServer(t, rec, `{"id":"t2_other"}`)
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{}, opts...)

	if _, err := d.ListAccounts(context.Background(), "cncf", model.ProviderRedditAds); err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	paths, queries, _ := rec.all()
	want := []string{"/api/v3/me/businesses", "/api/v3/businesses/biz1/ad_accounts"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range paths {
		if strings.Contains(paths[i], "t2_acct") || strings.Contains(queries[i], "t2_acct") {
			t.Errorf("the stored account id leaked into %q?%q", paths[i], queries[i])
		}
	}
}

func TestRedditListAccountsStillRejectsAnUnusableConnection(t *testing.T) {
	cases := []struct {
		name string
		conn func() *model.Connection
	}{
		{"inactive", func() *model.Connection {
			c := activeRedditConn(goodRedditCreds)
			c.Status = model.StatusInactive
			return c
		}},
		{"undecodable credentials", func() *model.Connection { return activeRedditConn(`{not json`) }},
		{"incomplete credentials", func() *model.Connection { return activeRedditConn(`{"ClientID":"cid"}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &requestRecorder{}
			opts := redditDiscoveryServer(t, rec, `{"id":"t2_a"}`)
			d := NewRedditDispatcher(fakeConnReader{conn: tc.conn()}, identityEncryptor{}, opts...)
			_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderRedditAds)
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
			}
			if paths, _, _ := rec.all(); len(paths) != 0 {
				t.Errorf("an unusable connection reached Reddit: %v", paths)
			}
		})
	}
}

func TestRedditListAccountsMissingConnectionKeepsErrNotFound(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{})
	if _, err := d.ListAccounts(context.Background(), "cncf", model.ProviderRedditAds); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRedditListAccountsReturnsEmptyNotNilWhenUpstreamHasNone(t *testing.T) {
	rec := &requestRecorder{}
	opts := redditDiscoveryServer(t, rec, ``)
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{}, opts...)
	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderRedditAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if accounts == nil || len(accounts) != 0 {
		t.Fatalf("accounts = %#v, want a non-nil empty slice", accounts)
	}
}

func TestRedditAccountLabel(t *testing.T) {
	cases := []struct {
		in   reddit.AdAccount
		want string
	}{
		{reddit.AdAccount{ID: "t2_a", Name: "LF"}, "LF"},
		{reddit.AdAccount{ID: "t2_a"}, "t2_a"},
		{reddit.AdAccount{ID: "t2_a", Name: "  "}, "t2_a"},
		{reddit.AdAccount{ID: "t2_a", Name: "LF", Currency: "USD"}, "LF [USD]"},
		{reddit.AdAccount{ID: "t2_a", Name: "LF", BusinessName: "Linux Foundation"}, "LF (Linux Foundation)"},
		{reddit.AdAccount{ID: "t2_a", Currency: "EUR", BusinessName: "CNCF"}, "t2_a [EUR] (CNCF)"},
	}
	for _, tc := range cases {
		if got := redditAccountLabel(tc.in); got != tc.want {
			t.Errorf("redditAccountLabel(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
