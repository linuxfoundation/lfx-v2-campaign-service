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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The stored-connection defects every adapter detects, expressed once. Each mutation takes an
// otherwise-good active connection and breaks exactly one thing.
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

// badCreds carries the two malformed credential blobs a defect table needs. They are separate
// cases, not one: `{` produces only a *json.SyntaxError, so a table with just that fixture never
// exercises the *json.UnmarshalTypeError assertion, and a regression that dropped syntax errors
// while still wrapping TYPE errors — the ones that name the credential FIELD — would stay green.
type badCreds struct {
	// incomplete parses cleanly but omits one required field.
	incomplete string
	// wrongType is syntactically valid JSON with a number where a required string belongs, so
	// encoding/json returns a *json.UnmarshalTypeError naming that field.
	wrongType string
}

func connDefectCases(bad badCreds) []connDefectCase {
	return []connDefectCase{
		{"inactive", func(c *model.Connection) { c.Status = model.StatusInactive }, domain.ErrConnectionInactive},
		{"undecodable blob", func(c *model.Connection) { c.EncryptedCredentials = []byte(`{`) }, domain.ErrCredentialsUndecodable},
		{"wrong-typed credential field", func(c *model.Connection) { c.EncryptedCredentials = []byte(bad.wrongType) }, domain.ErrCredentialsUndecodable},
		{"incomplete credentials", func(c *model.Connection) { c.EncryptedCredentials = []byte(bad.incomplete) }, domain.ErrCredentialsIncomplete},
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
//   - ErrSystemConnectionNotUsable iff the credentials came from the LF system fallback. This
//     is what the deferred systemScoped in each helper produces, and it decides whether the
//     caller is told to repair its OWN connection (409) or the operator is told to repair the
//     LF row. Asserted in BOTH directions: a project-owned defect that carried the system
//     marker would send a project chasing a row it cannot see.
//   - No *json.SyntaxError and no *json.UnmarshalTypeError anywhere in the chain. Checked with
//     errors.As rather than a substring match on Error(): a cause still in the chain is
//     reachable by any errors.As-walking logger even when the top-level string looks clean,
//     and the 503 arm logs the chain through safeErrSummary. The bytes it would carry are
//     derived from the DECRYPTED credential blob — encoding/json quotes its input, so
//     *json.SyntaxError names the offending character and *json.UnmarshalTypeError names the
//     field.
func assertConnectionDefectTagged(t *testing.T, err error, want error, fromSystem bool) {
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
	if got := errors.Is(err, domain.ErrSystemConnectionNotUsable); got != fromSystem {
		if fromSystem {
			t.Errorf("error = %v, want errors.Is(err, domain.ErrSystemConnectionNotUsable): these "+
				"credentials came from the LF system row, so dropping the deferred systemScoped "+
				"would tell the project to repair a connection it does not own", err)
		} else {
			t.Errorf("error = %v carries domain.ErrSystemConnectionNotUsable for a PROJECT-owned "+
				"row: the project's own connection is the thing to repair", err)
		}
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

// runConnDefectSuite drives every exported entry point of one adapter, over every defect, in
// both credential scopes.
//
// Driving EVERY entry point rather than just ToggleStatus is what turns "the tagging lives in
// the shared helper" from a claim in the commit message into something a test enforces: inline
// the helper into one path and the other paths go red.
func runConnDefectSuite(
	t *testing.T,
	bad badCreds,
	newConn func() *model.Connection,
	calls func(repo connReader) map[string]func() error,
) {
	t.Helper()
	scopes := map[string]struct {
		fromSystem bool
		repo       func(*model.Connection) connReader
	}{
		"project-owned row": {false, func(c *model.Connection) connReader { return fakeConnReader{conn: c} }},
		"lf system fallback": {true, func(c *model.Connection) connReader {
			// No row under "proj", so resolve falls back to the reserved system scope.
			return &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: c}}
		}},
	}
	for scopeName, scope := range scopes {
		t.Run(scopeName, func(t *testing.T) {
			for _, tc := range connDefectCases(bad) {
				t.Run(tc.name, func(t *testing.T) {
					conn := newConn()
					tc.mutate(conn)
					for callName, call := range calls(scope.repo(conn)) {
						t.Run(callName, func(t *testing.T) {
							assertConnectionDefectTagged(t, call(), tc.want, scope.fromSystem)
						})
					}
				})
			}
		})
	}
}

func TestReddit_UnusableConnectionIsTaggedOnEveryPath(t *testing.T) {
	// Reddit's metrics read is env-gated ahead of the resolve; without this the ReadMetrics
	// call would return ErrMetricsUnsupported and never reach the helper under test.
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	srv := unreachablePlatform(t)
	camp := &model.Campaign{Platform: model.ProviderRedditAds, PlatformCampaignID: "t2_c"}
	runConnDefectSuite(t,
		badCreds{
			incomplete: `{"ClientID":"cid","ClientSecret":"sec"}`,
			wrongType:  `{"ClientID":123,"ClientSecret":"sec","RefreshToken":"rt"}`,
		},
		func() *model.Connection { return activeRedditConn(goodRedditCreds) },
		func(repo connReader) map[string]func() error {
			// Both endpoints, not just the API one. Reddit authenticates with an OAuth
			// refresh against a SEPARATE host, so WithBaseURL alone leaves a regression
			// that got as far as building a client free to reach www.reddit.com — the
			// suite would then depend on external networking instead of tripping
			// unreachablePlatform, and "it never touches the network" is the property
			// under test, not a side effect of it.
			d := NewRedditDispatcher(repo, identityEncryptor{},
				reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL))
			return map[string]func() error{
				"Dispatch": func() error {
					_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, nil)
					return err
				},
				"ToggleStatus": func() error {
					return d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunPaused)
				},
				"ReadMetrics": func() error {
					_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderRedditAds, camp, model.MetricsWindowLast7Days)
					return err
				},
			}
		},
	)
}

func TestTwitter_UnusableConnectionIsTaggedOnEveryPath(t *testing.T) {
	srv := unreachablePlatform(t)
	camp := &model.Campaign{Platform: model.ProviderTwitterAds, PlatformCampaignID: "abc"}
	runConnDefectSuite(t,
		badCreds{
			incomplete: `{"ConsumerKey":"ck","ConsumerSecret":"cs","AccessToken":"at"}`,
			wrongType:  `{"ConsumerKey":123,"ConsumerSecret":"cs","AccessToken":"at","AccessTokenSecret":"ats"}`,
		},
		func() *model.Connection { return activeTwitterConn(goodTwitterCreds) },
		func(repo connReader) map[string]func() error {
			// X/Twitter needs no token-endpoint override: it signs each request with
			// OAuth 1.0a rather than exchanging a refresh token, so there is no second
			// host for a regression to reach.
			d := NewTwitterDispatcher(repo, identityEncryptor{}, twitter.WithBaseURL(srv.URL))
			return map[string]func() error{
				"Dispatch": func() error {
					_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, nil)
					return err
				},
				"ToggleStatus": func() error {
					return d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, camp, model.CampaignRunPaused)
				},
				"ReadMetrics": func() error {
					_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderTwitterAds, camp, model.MetricsWindowLast7Days)
					return err
				},
			}
		},
	)
}

// Microsoft has no ReadMetrics: campaign performance lives in the Reporting API v13, which is
// asynchronous (submit → poll → download), so the adapter exposes only the two synchronous
// paths. See docs/api-catalog.md.
// TestLinkedIn_UnusableConnectionIsTaggedOnEveryPath covers the two entry points LFXV2-3196
// routed through resolveLinkedInCredentials.
//
// The LF-system-fallback half is the load-bearing one. That helper tags system-scoped defects
// from a DEFERRED closure over `conn := res` — a binding taken before the error returns, which
// each set `res` to nil. Read the named return directly instead and the defer no-ops on exactly
// these paths: the service would answer 409 and tell the project to repair a connection row it
// does not own, while whoever installed the LF credential is never paged. Every other LinkedIn
// test uses a project-owned row, so without this case that regression passes the whole suite.
//
// Dispatch is deliberately absent: it validates inline to preserve its notCreated() claim-release
// contract (see internal-dispatch.md), so it does not route through the helper under test.
func TestLinkedIn_UnusableConnectionIsTaggedOnEveryPath(t *testing.T) {
	srv := unreachablePlatform(t)
	camp := &model.Campaign{Platform: model.ProviderLinkedInAds, PlatformCampaignID: "123"}
	runConnDefectSuite(t,
		badCreds{
			// AccessToken is the only required field, so "incomplete" is the empty object.
			incomplete: `{}`,
			wrongType:  `{"AccessToken":123}`,
		},
		func() *model.Connection { return activeLinkedInConn(goodLinkedInCreds) },
		func(repo connReader) map[string]func() error {
			d := NewLinkedInDispatcher(repo, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
			return map[string]func() error{
				"ToggleStatus": func() error {
					return d.ToggleStatus(context.Background(), "proj", model.ProviderLinkedInAds, camp, model.CampaignRunPaused)
				},
				// A SUPPORTED window: ReadMetrics rejects an unsupported one before it
				// resolves credentials, which would never reach the helper under test.
				"ReadMetrics": func() error {
					_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderLinkedInAds, camp, model.MetricsWindowLast7Days)
					return err
				},
			}
		},
	)
}

func TestMicrosoft_UnusableConnectionIsTaggedOnEveryPath(t *testing.T) {
	srv := unreachablePlatform(t)
	camp := &model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"}
	runConnDefectSuite(t,
		badCreds{
			incomplete: `{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev"}`,
			wrongType:  `{"ClientID":123,"ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`,
		},
		func() *model.Connection { return activeMicrosoftConn(goodMicrosoftCreds) },
		func(repo connReader) map[string]func() error {
			// As with Reddit above: login.microsoftonline.com is a different host from
			// the Campaign Management base URL and needs its own override.
			d := NewMicrosoftDispatcher(repo, identityEncryptor{},
				microsoft.WithBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL))
			return map[string]func() error{
				"Dispatch": func() error {
					_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, nil)
					return err
				},
				"ToggleStatus": func() error {
					return d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused)
				},
			}
		},
	)
}
