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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestSanitizeSnapshotURL: the query/fragment (which may carry secrets) must be
// stripped before a URL is stored in the unencrypted config_snapshot.
func TestSanitizeSnapshotURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"https://example.com/reg?token=SECRET&x=1", "https://example.com/reg"},
		{"https://example.com/p#frag-SECRET", "https://example.com/p"},
		{"https://example.com/path", "https://example.com/path"},
		{"t3_abc123", "t3_abc123"}, // reddit thing-id, no query — unchanged
		{"not a url?token=SECRET", "not a url"},
		{"https://user:pass@example.com/x?token=SECRET", ""}, // secretlint-disable-line -- fixture asserting userinfo fails closed
	}
	for _, tc := range cases {
		if got := sanitizeSnapshotURL(tc.in); got != tc.want {
			t.Errorf("sanitizeSnapshotURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEnvelopeHSToken covers the shared top-level hsToken extraction: a valid string
// is returned trimmed, absence yields "", and a wrong-typed value is an error (not a
// silent fallback).
func TestEnvelopeHSToken(t *testing.T) {
	cases := []struct {
		name     string
		envelope string
		want     string
		wantErr  bool
	}{
		{"empty envelope", ``, "", false},
		{"absent field", `{"redditConfig":{"budgetUsd":1}}`, "", false},
		{"valid string", `{"hsToken":"  HS-123  ","redditConfig":{}}`, "HS-123", false},
		{"empty string", `{"hsToken":""}`, "", false},
		{"wrong type number", `{"hsToken":123,"redditConfig":{}}`, "", true},
		{"wrong type object", `{"hsToken":{"x":1}}`, "", true},
		{"explicit null", `{"hsToken":null,"redditConfig":{}}`, "", true},
		{"malformed envelope", `{bad`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := envelopeHSToken([]byte(tc.envelope))
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got nil (result %q)", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyCampaignConfig covers the shared budget/schedule/config mapping used by
// every adapter: budget + daily/lifetime type, parsed dates, config snapshot, and the
// over-range budget guard.
func TestApplyCampaignConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("daily budget + dates + snapshot", func(t *testing.T) {
		c := &model.Campaign{Platform: model.ProviderRedditAds}
		applyCampaignConfig(ctx, c, 500, false, "2099-01-02", "2099-03-04", map[string]any{"k": "v"})
		if c.BudgetAmount == nil || *c.BudgetAmount != 500 {
			t.Errorf("BudgetAmount = %v, want 500", c.BudgetAmount)
		}
		if c.BudgetType == nil || *c.BudgetType != model.BudgetDaily {
			t.Errorf("BudgetType = %v, want daily", c.BudgetType)
		}
		if c.StartDate == nil || c.StartDate.Format(campaignDateLayout) != "2099-01-02" {
			t.Errorf("StartDate = %v, want 2099-01-02", c.StartDate)
		}
		if c.EndDate == nil || c.EndDate.Format(campaignDateLayout) != "2099-03-04" {
			t.Errorf("EndDate = %v, want 2099-03-04", c.EndDate)
		}
		if len(c.ConfigSnapshot) == 0 {
			t.Error("ConfigSnapshot should be populated")
		}
	})

	t.Run("lifetime flag sets lifetime type", func(t *testing.T) {
		c := &model.Campaign{}
		applyCampaignConfig(ctx, c, 10, true, "", "", nil)
		if c.BudgetType == nil || *c.BudgetType != model.BudgetLifetime {
			t.Errorf("BudgetType = %v, want lifetime", c.BudgetType)
		}
	})

	t.Run("zero budget leaves amount and type nil", func(t *testing.T) {
		c := &model.Campaign{}
		applyCampaignConfig(ctx, c, 0, false, "", "", nil)
		if c.BudgetAmount != nil || c.BudgetType != nil {
			t.Errorf("a zero budget must leave BudgetAmount/BudgetType nil, got %v/%v", c.BudgetAmount, c.BudgetType)
		}
	})

	t.Run("over-range budget is not persisted", func(t *testing.T) {
		// 1e12 exceeds NUMERIC(14,2); persisting it would overflow the column. The guard
		// leaves budget_amount NULL (the campaign already exists upstream) rather than
		// failing the whole row write.
		c := &model.Campaign{Platform: model.ProviderMetaAds}
		applyCampaignConfig(ctx, c, 1e12, true, "", "", nil)
		if c.BudgetAmount != nil {
			t.Errorf("an over-range budget must not be persisted, got %v", *c.BudgetAmount)
		}
		if c.BudgetType != nil {
			t.Errorf("BudgetType must be nil when budget is not persisted, got %v", *c.BudgetType)
		}
	})

	t.Run("budget at the boundary is persisted", func(t *testing.T) {
		c := &model.Campaign{}
		applyCampaignConfig(ctx, c, maxPersistedBudget, false, "", "", nil)
		if c.BudgetAmount == nil {
			t.Error("a budget at the max boundary must still be persisted")
		}
	})

	t.Run("blank or malformed dates are nil", func(t *testing.T) {
		c := &model.Campaign{}
		applyCampaignConfig(ctx, c, 1, false, "", "not-a-date", nil)
		if c.StartDate != nil {
			t.Errorf("a blank start date must be nil, got %v", c.StartDate)
		}
		if c.EndDate != nil {
			t.Errorf("a malformed end date must be nil, got %v", c.EndDate)
		}
	})
}

// scopedConnReader answers per PROJECT SCOPE, unlike fakeConnReader: the fallback is entirely
// about WHICH scope was asked, so a fake that cannot tell them apart passes against an
// implementation that never consults the system scope at all.
type scopedConnReader struct {
	rows map[string]*model.Connection
	errs map[string]error
	gets []string // every project id asked for, in order

	// tombstoned models the state Get CANNOT express: a row soft-deleted by Delete, which
	// Get filters out and reports as ErrNotFound like any other absence. disconnectErr is the
	// probe itself failing, which must not be read as "no".
	tombstoned    map[string]bool
	disconnectErr error
}

func (f *scopedConnReader) Disconnected(_ context.Context, projectID string, _ model.Provider) (bool, error) {
	if f.disconnectErr != nil {
		return false, f.disconnectErr
	}
	return f.tombstoned[projectID], nil
}

func (f *scopedConnReader) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	f.gets = append(f.gets, projectID)
	if err, ok := f.errs[projectID]; ok {
		return nil, err
	}
	if c, ok := f.rows[projectID]; ok {
		return c, nil
	}
	return nil, domain.ErrNotFound
}

func usableConn(creds, accountID string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderGoogleAds,
		AccountID:            accountID,
		EncryptedCredentials: []byte(creds),
		Status:               model.StatusActive,
	}
}

// TestResolveFallsBackToSystemAccount: a project with no connection of its own runs on the LF
// system account rather than failing.
func TestResolveFallsBackToSystemAccount(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account"),
	}}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.accountID != "sys-account" || string(got.plaintext) != `{"sys":true}` {
		t.Errorf("resolved %q/%q, want the system account's credentials", got.accountID, got.plaintext)
	}
	if len(repo.gets) != 2 || repo.gets[0] != "cncf" || repo.gets[1] != model.SystemProjectID {
		t.Errorf("scopes asked = %v, want the project first then the system scope", repo.gets)
	}
}

// TestResolveDoesNotFallBackFromABrokenProjectConnection: a project that HAS a connection
// recorded an intent to bill its own. This asymmetry is what makes the fallback safe.
func TestResolveDoesNotFallBackFromABrokenProjectConnection(t *testing.T) {
	cases := map[string]*model.Connection{
		// One refused by resolve, one by the adapter after it. Both must stop at the
		// project's row, never the system account's.
		"no stored credentials": {Provider: model.ProviderGoogleAds, Status: model.StatusActive},
		"inactive":              {Provider: model.ProviderGoogleAds, AccountID: "1", EncryptedCredentials: []byte(`{}`), Status: model.StatusInactive},
	}
	for name, projectConn := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &scopedConnReader{rows: map[string]*model.Connection{
				"cncf":                projectConn,
				model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account"),
			}}
			got, err := newCredsSource(repo, identityEncryptor{}).
				resolve(context.Background(), "cncf", model.ProviderGoogleAds)
			if err == nil && string(got.plaintext) == `{"sys":true}` {
				t.Error("resolved the system account's credentials for a project that has its own connection")
			}
			for _, scope := range repo.gets {
				if scope == model.SystemProjectID {
					t.Error("the system account was consulted for a project that has its own connection")
				}
			}
		})
	}
}

// TestResolveDoesNotFallBackFromATransientProjectLookupFailure pins the one `if` that
// separates "this project genuinely has no connection" from "something went wrong talking
// to the project's own connection". The fallback is gated on errors.Is(err,
// domain.ErrNotFound); every other repository error must fail closed.
//
// The distinction is consequential in one direction only. Falling back on a genuine
// absence spends LF budget on behalf of a project that chose to have none — which is the
// designed behaviour. Falling back on a DB timeout spends it on behalf of a project that
// may have a perfectly good connection of its own, on the strength of a lookup that never
// answered. The two are one keyword apart in the source and indistinguishable at the call
// site, which is why the boundary is worth a test rather than a comment.
func TestResolveDoesNotFallBackFromATransientProjectLookupFailure(t *testing.T) {
	transient := errors.New("connection refused")
	repo := &scopedConnReader{
		errs: map[string]error{"cncf": transient},
		// A perfectly usable system row, so a fallback here would SUCCEED. The test is
		// only meaningful because the wrong behaviour is the silent, working one.
		rows: map[string]*model.Connection{
			model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account"),
		},
	}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err == nil {
		t.Fatalf("resolve returned %q for a project whose own lookup failed; a transient error "+
			"must not be read as an absence", got.accountID)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want the transient error rather than an absence — classifying it as "+
			"ErrNotFound is what would let a later refactor route it into the fallback", err)
	}
	if !errors.Is(err, transient) {
		t.Errorf("err = %v, want it to wrap the repository's own error", err)
	}
	for _, scope := range repo.gets {
		if scope == model.SystemProjectID {
			t.Error("the system scope was consulted after the project's own lookup FAILED; " +
				"the project may well have a connection, and running its campaign on LF's " +
				"account is not a recoverable mistake")
		}
	}
}

// TestFallbackOutcomes covers what the system-scope lookup may yield beyond a usable row: an
// absence (the error must name the CALLER's project, not the reserved scope), an unusable system
// row (refused, not trusted because it is ours), and a lookup FAILURE (a 503, never a 404).
func TestFallbackOutcomes(t *testing.T) {
	usable := &model.Connection{Provider: model.ProviderGoogleAds, Status: model.StatusActive}
	for name, tc := range map[string]struct {
		repo *scopedConnReader
		want error
	}{
		"no system account": {&scopedConnReader{}, domain.ErrNotFound},
		"unusable system row": {&scopedConnReader{rows: map[string]*model.Connection{
			model.SystemProjectID: usable,
		}}, domain.ErrConnectionNotUsable},
		"system lookup fails": {&scopedConnReader{errs: map[string]error{
			model.SystemProjectID: errors.New("connection refused"),
		}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newCredsSource(tc.repo, identityEncryptor{}).
				resolve(context.Background(), "cncf", model.ProviderGoogleAds)
			switch {
			case tc.want == nil && (err == nil || errors.Is(err, domain.ErrNotFound)):
				t.Fatalf("err = %v, want a non-absence error", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == domain.ErrNotFound && (!strings.Contains(err.Error(), "cncf") ||
				strings.Contains(err.Error(), model.SystemProjectID)) {
				t.Errorf("err = %q, want it to name the project and not the reserved scope", err)
			}
		})
	}
}

// TestResolveAtTheSystemScopeDoesNotRecurse: a brief already at the reserved scope asks once.
func TestResolveAtTheSystemScopeDoesNotRecurse(t *testing.T) {
	repo := &scopedConnReader{}
	if _, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), model.SystemProjectID, model.ProviderGoogleAds); err == nil {
		t.Fatal("want ErrNotFound, got nil")
	}
	if len(repo.gets) != 1 {
		t.Errorf("scopes asked = %v, want exactly one lookup", repo.gets)
	}
}

// TestUnusableSystemConnectionKeepsItsOrigin: whose connection is broken decides who can fix
// it. A defect in the project's own row is its owner's to edit and answers 400; the same
// defect in the LF system row reaches a project that has no connection and cannot address the
// system scope, so it must arrive carrying ErrSystemConnectionNotUsable and be paged instead.
func TestUnusableSystemConnectionKeepsItsOrigin(t *testing.T) {
	broken := func() *model.Connection {
		c := usableConn(`{"sys":true}`, "sys-account")
		c.EncryptedCredentials = nil
		return c
	}

	_, err := newCredsSource(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: broken()},
	}, identityEncryptor{}).resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("system fallback err = %v, want ErrConnectionNotUsable", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("system fallback err = %v, want it to name the SYSTEM connection", err)
	}

	// The project's own broken row must NOT pick up the system marker, or every 400 that
	// tells an owner to fix their connection becomes a 500 that tells nobody anything.
	_, err = newCredsSource(&scopedConnReader{
		rows: map[string]*model.Connection{"cncf": broken()},
	}, identityEncryptor{}).resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("project connection err = %v, want ErrConnectionNotUsable", err)
	}
	if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("project connection err = %v, must not be attributed to the system account", err)
	}
}

// TestSystemScopedCoversEveryStoredStateDefectOnDiscovery: systemScoped is not a property of one
// error site. resolveGoogleAdsDiscoveryClient rejects TWO classes of stored state — the
// credentials themselves, and login_customer_id — and a defect in the LF fallback row is the
// operator's page in both cases. Tagging only the first left a project running on the fallback a
// 400 telling it to edit a connection it does not own and cannot reach.
func TestSystemScopedCoversEveryStoredStateDefectOnDiscovery(t *testing.T) {
	sysConn := usableConn(goodGoogleAdsCreds, "8666746580")
	sysConn.ProviderConfig = map[string]string{"login_customer_id": "974-698-3954"}

	d := NewGoogleAdsDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: sysConn},
	}, identityEncryptor{})

	_, err := d.resolveGoogleAdsDiscoveryClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrProviderConfigInvalid) {
		t.Fatalf("err = %v, want the login_customer_id defect", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v, want it attributed to the SYSTEM connection", err)
	}

	// The same defect on the project's own row stays the project's to fix.
	ownConn := usableConn(goodGoogleAdsCreds, "8666746580")
	ownConn.ProviderConfig = map[string]string{"login_customer_id": "974-698-3954"}
	d = NewGoogleAdsDispatcher(&scopedConnReader{
		rows: map[string]*model.Connection{"cncf": ownConn},
	}, identityEncryptor{})
	_, err = d.resolveGoogleAdsDiscoveryClient(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
	}
	if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v, must not be attributed to the system account", err)
	}
}

// failingDecryptor stands in for a rotated application key or a corrupted blob: authenticated
// decryption fails, which is neither a usability defect nor anything the caller can edit.
type failingDecryptor struct{}

func (failingDecryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (failingDecryptor) Decrypt([]byte) ([]byte, error) {
	return nil, fmt.Errorf("%w: decryption authentication failed", domain.ErrCredentialDecryptionFailed)
}

// TestSystemFallbackMarksOriginOnErrorsItDoesNotClassify: systemScoped only fires on
// ErrConnectionNotUsable, so before ErrSystemConnectionOrigin a decryption failure from the
// fallback arrived indistinguishable from one on the caller's own row — and the operator log
// for that arm names a row by project id.
func TestSystemFallbackMarksOriginOnErrorsItDoesNotClassify(t *testing.T) {
	sysRow := usableConn(goodGoogleAdsCreds, "8666746580")

	_, err := newCredsSource(&scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: sysRow},
	}, failingDecryptor{}).resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrCredentialDecryptionFailed) {
		t.Fatalf("err = %v, want the decryption failure", err)
	}
	if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v: a decryption failure is not a usability defect and must not be "+
			"classified as one — origin and classification are separate questions", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, want it to record that the SYSTEM row was the one read", err)
	}

	// The caller's own row must not pick up the marker, or every decryption failure is
	// attributed to the system account and the single-row cause becomes uninvestigable.
	_, err = newCredsSource(&scopedConnReader{
		rows: map[string]*model.Connection{"cncf": usableConn(goodGoogleAdsCreds, "8666746580")},
	}, failingDecryptor{}).resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrCredentialDecryptionFailed) {
		t.Fatalf("err = %v, want the decryption failure", err)
	}
	if errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, must not be attributed to the system row", err)
	}
}

// TestSystemScopedCoversEveryCallerNotJustDiscovery: systemScoped was applied by ONE caller —
// resolveGoogleAdsDiscoveryClient — so the three paths that resolve the same connection through
// validateGoogleAdsConnection (Dispatch, and the toggle/metrics resolveGoogleAdsClient) returned
// the identical LF-system-row defect untagged. A project running on the fallback then got a 400
// telling it to go edit a connection it does not own and cannot reach, while the operator who
// installed the LF credential was never paged.
//
// The defect used here is deliberately one only the VALIDATOR can see: resolve() itself already
// tagged everything it classified (TestUnusableSystemConnectionKeepsItsOrigin covers that), so a
// resolve-level defect would pass even with the tagging removed. An account-less system row
// resolves cleanly and fails later, in validateGoogleAdsConnection — which is precisely the
// window the caller-side arrangement left open.
func TestSystemScopedCoversEveryCallerNotJustDiscovery(t *testing.T) {
	// Two defect classes, one per defer this fix installs. The account-less row is
	// validateGoogleAdsConnection's OWN branch and does not apply to discovery, which exists
	// precisely to run without an account selected; the inactive row is
	// validateGoogleAdsCredentials' and applies to all three.
	defects := map[string]struct {
		conn         func() *model.Connection
		skipDiscover bool
	}{
		"no account selected": {
			conn:         func() *model.Connection { return usableConn(goodGoogleAdsCreds, "") },
			skipDiscover: true,
		},
		"connection not active": {
			conn: func() *model.Connection {
				c := usableConn(goodGoogleAdsCreds, "8666746580")
				c.Status = model.StatusInactive
				return c
			},
		},
	}

	callers := map[string]func(*GoogleAdsDispatcher) error{
		"create/Dispatch": func(d *GoogleAdsDispatcher) error {
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds,
				json.RawMessage(`{"googleAdsConfig":{"budget":50}}`))
			return err
		},
		"toggle+metrics/resolveGoogleAdsClient": func(d *GoogleAdsDispatcher) error {
			_, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds)
			return err
		},
		"discovery/resolveGoogleAdsDiscoveryClient": func(d *GoogleAdsDispatcher) error {
			// Kept alongside the other two so the path that always had the tagging cannot
			// regress while attention is on the two that did not.
			_, err := d.resolveGoogleAdsDiscoveryClient(context.Background(), "cncf", model.ProviderGoogleAds)
			return err
		},
	}

	for defectName, defect := range defects {
		for callerName, call := range callers {
			if defect.skipDiscover && strings.HasPrefix(callerName, "discovery/") {
				continue
			}
			t.Run(defectName+"/"+callerName, func(t *testing.T) {
				dispatcherFor := func(scope string) *GoogleAdsDispatcher {
					return NewGoogleAdsDispatcher(&scopedConnReader{
						rows: map[string]*model.Connection{scope: defect.conn()},
					}, identityEncryptor{})
				}

				err := call(dispatcherFor(model.SystemProjectID))
				if !errors.Is(err, domain.ErrConnectionNotUsable) {
					t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
				}
				if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
					t.Errorf("err = %v, want it attributed to the SYSTEM connection — this caller "+
						"sends the project to fix a row it does not own", err)
				}

				// And the mirror: the project's OWN broken row must not pick up the marker,
				// or every 400 that names a fixable connection becomes an operator page.
				err = call(dispatcherFor("cncf"))
				if !errors.Is(err, domain.ErrConnectionNotUsable) {
					t.Fatalf("own-row err = %v, want ErrConnectionNotUsable", err)
				}
				if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
					t.Errorf("own-row err = %v, must not be attributed to the system account", err)
				}
			})
		}
	}
}

// TestResolveDoesNotFallBackForNonAdProviders: the fallback is an ad-ACCOUNT fallback, and
// credsSource is shared beyond the ad paths — AudienceBuilder resolves ProviderHubSpot
// through the same function. What would fall back there is not a budget but a CRM portal, so
// a project with no HubSpot connection would have its contact lists written into the LF's own
// portal: real contact data in the wrong tenant, silently, and against the documented
// behaviour that the build fails. Spending LF ad budget on an LF-run campaign is the trade
// this fallback deliberately makes; mixing tenants' contacts is a different trade nobody made.
//
// The system row EXISTS here, so the test fails if the gate is removed rather than passing
// vacuously on a missing row.
func TestResolveDoesNotFallBackForNonAdProviders(t *testing.T) {
	sysRow := usableConn(`{"sys":true}`, "lf-portal")
	sysRow.Provider = model.ProviderHubSpot
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sysRow}}

	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderHubSpot)
	if err == nil {
		t.Fatalf("resolve = %+v, want an error: a project with no HubSpot connection must not write its contact lists into the LF portal", got)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want it to stay a plain absence (the project has no connection), not a system-scoped failure", err)
	}
	// The system scope must not even be CONSULTED: the gate is a classification question,
	// answerable without a database round-trip.
	for _, scope := range repo.gets {
		if scope == model.SystemProjectID {
			t.Errorf("scopes asked = %v, want the system scope never consulted for a non-ad provider", repo.gets)
		}
	}
}

// TestSystemFallbackIsGatedByClassificationNotByName pins that the gate asks Kind() rather
// than comparing against ProviderHubSpot. Every paid-ads provider must fall back, and every
// provider that is not classified as paid ads must not — so a provider added later is denied
// the LF credential by default until someone classifies it, instead of inheriting it.
func TestSystemFallbackIsGatedByClassificationNotByName(t *testing.T) {
	for _, p := range model.AllProviders() {
		t.Run(string(p), func(t *testing.T) {
			row := usableConn(`{"sys":true}`, "sys-account")
			row.Provider = p
			repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: row}}

			_, err := newCredsSource(repo, identityEncryptor{}).
				resolve(context.Background(), "cncf", p)

			if p.IsPaidAds() {
				if err != nil {
					t.Fatalf("resolve(%s): %v; every paid-ads provider falls back to the LF account", p, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolve(%s) succeeded; only paid-ads providers may use the LF system account", p)
			}
		})
	}
}

// TestADisconnectedProjectDoesNotFallBackToTheLFAccount covers the difference between a project
// that never said anything and one that said no.
//
// Delete SOFT-deletes (status = 'deleted') and Get filters those rows out, so both states reach
// resolve as the same domain.ErrNotFound. The fallback reads that as licence to run the
// project's campaigns on the LF-owned ad account — so an owner who deliberately disconnected
// their account got their spend moved onto the Linux Foundation's, with an INFO log for it and
// nothing else. Absence of a statement is what the fallback is for; a statement to the contrary
// is not absence.
//
// The narrowing half is the whole point of the fallback and must keep working: a project that
// never connected still gets the LF account.
func TestADisconnectedProjectDoesNotFallBackToTheLFAccount(t *testing.T) {
	sysRows := map[string]*model.Connection{model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account")}

	t.Run("a disconnected project is refused", func(t *testing.T) {
		repo := &scopedConnReader{rows: sysRows, tombstoned: map[string]bool{"cncf": true}}
		got, err := newCredsSource(repo, identityEncryptor{}).
			resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err == nil {
			t.Fatalf("resolve = %+v, want a refusal: this project disconnected its account, so "+
				"running its campaign on the LF account spends LF budget against an explicit no", got)
		}
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v, want it to keep ErrNotFound so read-only callers still answer 404", err)
		}
		for _, scope := range repo.gets {
			if scope == model.SystemProjectID {
				t.Errorf("scopes asked = %v, want the system scope never consulted", repo.gets)
			}
		}
	})

	t.Run("a project that never connected still falls back", func(t *testing.T) {
		repo := &scopedConnReader{rows: sysRows}
		got, err := newCredsSource(repo, identityEncryptor{}).
			resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve: %v — a project that never connected is exactly what the fallback is for", err)
		}
		if !got.fromSystem {
			t.Fatalf("resolved = %+v, want the system account", got)
		}
	})

	// An unanswered "was this disconnected?" is not a no. Failing open here would restore the
	// whole defect on any database blip, which is the shape a fallback fails in.
	t.Run("a probe failure fails closed", func(t *testing.T) {
		repo := &scopedConnReader{rows: sysRows, disconnectErr: errors.New("db down")}
		if _, err := newCredsSource(repo, identityEncryptor{}).
			resolve(context.Background(), "cncf", model.ProviderGoogleAds); err == nil {
			t.Fatal("resolve = nil error, want a refusal: the probe did not answer, so nothing " +
				"proves this project did not disconnect")
		}
	})
}

// TestAdoptionRefusesTheSystemFallback: the credential fallback is a feature for every path
// that names a campaign this service already has a project-scoped ROW for — the row is the
// authorization, so sharing one LF ad account across projects is safe. Adoption breaks that
// assumption: its caller names an ARBITRARY upstream id, so inside the shared account project A
// could bind project B's console-created campaign to its own brief and thereafter read its spend
// and pause it. Neither the account-mismatch guard nor the row-scoped guards on metrics/toggle
// help, because both projects resolve to the SAME customer id and the row A creates is A's own.
//
// The test pins the boundary in both directions: refuse under the fallback, proceed on a
// project-owned connection. Without the second half a blanket refusal would pass.
func TestAdoptionRefusesTheSystemFallback(t *testing.T) {
	usable := func() *model.Connection { return usableConn(goodGoogleAdsCreds, "8666746580") }

	t.Run("a project with no connection of its own cannot adopt", func(t *testing.T) {
		d := NewGoogleAdsDispatcher(&scopedConnReader{
			rows: map[string]*model.Connection{model.SystemProjectID: usable()},
		}, identityEncryptor{})

		_, err := d.LookupCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
		if !errors.Is(err, domain.ErrAdoptionRequiresOwnConnection) {
			t.Fatalf("err = %v, want ErrAdoptionRequiresOwnConnection — adoption under the shared "+
				"LF account lets any project bind another project's campaign there", err)
		}
	})

	// The case where NEITHER scope has a connection. It looks like the one above and is not:
	// there is no fallback to refuse, so resolve returns a wrapped domain.ErrNotFound and the
	// gate never sees a resolved value at all. Left untranslated, the adopt switch has no
	// ErrNotFound arm and answers 503 "could not be reached" — for a platform that was never
	// contacted, about a state no retry can change. The remedy is identical to the fallback
	// case (connect the project's own ad account), so the sentinel must be too.
	t.Run("a project with no connection anywhere gets the same permanent refusal", func(t *testing.T) {
		d := NewGoogleAdsDispatcher(&scopedConnReader{rows: map[string]*model.Connection{}}, identityEncryptor{})

		_, err := d.LookupCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
		if !errors.Is(err, domain.ErrAdoptionRequiresOwnConnection) {
			t.Fatalf("err = %v, want ErrAdoptionRequiresOwnConnection — without it this is a 503 "+
				"blaming the network for a connection that was never configured", err)
		}
		// The cause survives the translation: an operator reading the log still learns the
		// lookup missed rather than that some other resolve step failed.
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v, want the wrapped ErrNotFound cause preserved", err)
		}
	})

	t.Run("a project with its own connection gets past the gate", func(t *testing.T) {
		// Deliberately account-less rather than fully usable: that defect is raised by
		// validateGoogleAdsConnection, one step PAST the ownership gate and still short of
		// the network, so reaching it proves the gate did not fire without this unit test
		// making an outbound call.
		d := NewGoogleAdsDispatcher(&scopedConnReader{
			rows: map[string]*model.Connection{"cncf": usableConn(goodGoogleAdsCreds, "")},
		}, identityEncryptor{})

		_, err := d.LookupCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
		if errors.Is(err, domain.ErrAdoptionRequiresOwnConnection) {
			t.Fatalf("err = %v: this project owns its connection, so the gate must not fire", err)
		}
		if !errors.Is(err, domain.ErrAccountNotSelected) {
			t.Fatalf("err = %v, want the account-not-selected defect from the step after the gate", err)
		}
	})
}
