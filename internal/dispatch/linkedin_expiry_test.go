// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// TestLinkedinExpiryTagsConnectionDefect proves an expired-and-unrefreshable
// LinkedIn credential is re-tagged as the connection-defect pair the service layer
// classifies: ErrConnectionNotUsable (which keeps it OUT of the retryable/503 bucket
// and away from an opaque 500) plus ErrCredentialsExpired (the machine-readable
// reason). This is the production failure from 2026-08-14, where the only evidence
// was a 401 in a server log.
func TestLinkedinExpiryTagsConnectionDefect(t *testing.T) {
	base := fmt.Errorf("linkedin: the LinkedIn connection %q must be reconnected: %w",
		"LF LinkedIn", linkedin.ErrCredentialsExpired)

	got := linkedinExpiry(base)

	if !errors.Is(got, domain.ErrConnectionNotUsable) {
		t.Error("want ErrConnectionNotUsable so the caller gets a non-retryable status, not a 503/500")
	}
	if !errors.Is(got, domain.ErrCredentialsExpired) {
		t.Error("want ErrCredentialsExpired as the machine-readable reason")
	}
	if !errors.Is(got, linkedin.ErrCredentialsExpired) {
		t.Error("the originating cause must be preserved")
	}
	if !strings.Contains(got.Error(), "LF LinkedIn") {
		t.Errorf("error %q must name the connection to be actionable", got)
	}
}

// TestLinkedinExpiryIsNoOpForOtherErrors proves the tagger does not reclassify
// unrelated failures — an upstream outage must stay retryable.
func TestLinkedinExpiryIsNoOpForOtherErrors(t *testing.T) {
	other := errors.New("linkedin API POST /adAccounts -> 503")
	if got := linkedinExpiry(other); !errors.Is(got, other) || errors.Is(got, domain.ErrConnectionNotUsable) {
		t.Errorf("linkedinExpiry(%v) = %v, want the error unchanged", other, got)
	}
	if linkedinExpiry(nil) != nil {
		t.Error("linkedinExpiry(nil) must stay nil")
	}
}

// TestLinkedinConnectionLabelNamesTheRightRow proves the label distinguishes the LF
// SYSTEM row from a project-owned one. One expired system token disables LinkedIn for
// every project falling back to it; telling those operators to fix "the LinkedIn
// connection" would send them to a row they do not own.
func TestLinkedinConnectionLabelNamesTheRightRow(t *testing.T) {
	if got := linkedinConnectionLabel(&resolved{fromSystem: true, label: "LF Shared"}); !strings.Contains(got, "system") {
		t.Errorf("system row label = %q, want it to identify the LF system connection", got)
	}
	if got := linkedinConnectionLabel(&resolved{label: "Acme LinkedIn"}); strings.Contains(got, "system") {
		t.Errorf("project-owned label = %q, must not be attributed to the system row", got)
	}
	if got := linkedinConnectionLabel(&resolved{label: "Acme LinkedIn"}); !strings.Contains(got, "Acme LinkedIn") {
		t.Errorf("label = %q, want the connection's friendly name", got)
	}
	if got := linkedinConnectionLabel(nil); got == "" {
		t.Error("a nil resolved must still yield a non-empty label")
	}
}

// TestLinkedinCredsDecodesTheStoredRefreshFields pins the wire→storage→dispatch
// contract for the refresh fields. The persisted blob has NO json tags, so its keys
// are the Goa struct's GO FIELD NAMES (AccessToken, RefreshToken, ClientID,
// ClientSecret) — see design/connection.go's linkedin-ads-credentials. If
// linkedinCreds ever drifts from those names the fields decode to empty, CanRefresh()
// returns false, and the connection silently degrades to bearer-only: the exact
// 60-day outage this feature exists to prevent, reintroduced without a build error.
func TestLinkedinCredsDecodesTheStoredRefreshFields(t *testing.T) {
	stored := []byte(`{
		"AccessToken":"at-value",
		"RefreshToken":"rt-value",
		"ClientID":"cid-value",
		"ClientSecret":"secret-value"
	}`)

	var got linkedinCreds
	if err := json.Unmarshal(stored, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"AccessToken", got.AccessToken, "at-value"},
		{"RefreshToken", got.RefreshToken, "rt-value"},
		{"ClientID", got.ClientID, "cid-value"},
		{"ClientSecret", got.ClientSecret, "secret-value"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	if !linkedinCredentials(got, "conn").CanRefresh() {
		t.Error("a fully-populated stored credential must be refreshable")
	}
	// A bearer-only blob (the common non-MDP case) must decode and stay non-refreshable.
	var bearer linkedinCreds
	if err := json.Unmarshal([]byte(`{"AccessToken":"at"}`), &bearer); err != nil {
		t.Fatalf("decode bearer-only: %v", err)
	}
	if linkedinCredentials(bearer, "conn").CanRefresh() {
		t.Error("a bearer-only credential must NOT report itself refreshable")
	}
}

// expiredLinkedInCreds is a stored blob whose access token is refreshable in shape but
// whose REFRESH token is already past its deadline, so accessTokenValue fails closed
// with ErrCredentialsExpired WITHOUT any network call. That makes these tests
// hermetic: no httptest token server is needed and no upstream is contacted.
var expiredLinkedInCreds = func() string {
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	return `{"AccessToken":"at","RefreshToken":"rt","ClientID":"ci","ClientSecret":"cs",` +
		`"AccessTokenExpiresAt":"` + past + `","RefreshTokenExpiresAt":"` + past + `"}`
}()

// TestLinkedIn_ExpiredCredentialsAreTaggedOnEveryPath drives all FOUR dispatcher entry
// points end-to-end and asserts each re-tags the client's ErrCredentialsExpired as the
// connection-defect pair. Testing linkedinExpiry in isolation proves only that the
// helper composes sentinels — it proves nothing about whether each call site reaches
// it. Delete any one of the four `if errors.Is(...)` blocks in linkedin.go and exactly
// one subtest here fails.
func TestLinkedIn_ExpiredCredentialsAreTaggedOnEveryPath(t *testing.T) {
	campaign := &model.Campaign{Platform: model.ProviderLinkedInAds, PlatformCampaignID: "555"}

	cases := []struct {
		name string
		call func(d *LinkedInDispatcher) error
	}{
		{"Dispatch", func(d *LinkedInDispatcher) error {
			// A VALID config, so the call reaches the client and fails on the credential
			// rather than on config validation — otherwise the subtest would pass for the
			// wrong reason and prove nothing about the expiry tagging.
			cfg := json.RawMessage(`{"linkedInConfig":{
				"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
				"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
				"targetingProfile":"cloud-native",
				"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
				"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
			}}`)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			return err
		}},
		{"ToggleStatus", func(d *LinkedInDispatcher) error {
			return d.ToggleStatus(context.Background(), "p1", model.ProviderLinkedInAds, campaign, "paused")
		}},
		{"ListAccounts", func(d *LinkedInDispatcher) error {
			_, err := d.ListAccounts(context.Background(), "p1", model.ProviderLinkedInAds)
			return err
		}},
		{"ReadMetrics", func(d *LinkedInDispatcher) error {
			_, err := d.ReadMetrics(context.Background(), "p1", model.ProviderLinkedInAds, campaign, model.MetricsWindowLast7Days)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewLinkedInDispatcher(
				fakeConnReader{conn: activeLinkedInConn(expiredLinkedInCreds)},
				identityEncryptor{},
			)

			err := tc.call(d)
			if err == nil {
				t.Fatal("expected an error for an expired, unrefreshable credential")
			}
			// ErrConnectionNotUsable decides the HTTP status: this must NOT fall through
			// to the retryable 503 bucket or an opaque 500.
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("err = %v; want ErrConnectionNotUsable so the caller gets a non-retryable status", err)
			}
			// ErrCredentialsExpired is the machine-readable reason the service layer
			// turns into the "credentials_expired" token.
			if !errors.Is(err, domain.ErrCredentialsExpired) {
				t.Errorf("err = %v; want ErrCredentialsExpired as the reason sentinel", err)
			}
			if !strings.Contains(err.Error(), "reconnected") {
				t.Errorf("err = %q; want an actionable message telling the operator to reconnect", err)
			}
		})
	}
}
