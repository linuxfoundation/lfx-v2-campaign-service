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

// TestSanitizeSnapshotURL: the path, query and fragment (any of which may carry a
// secret) must be stripped before a URL is stored in the unencrypted config_snapshot.
// The path was kept until the seventh review round pointed out that a magic-link or
// reset URL puts its token in a path segment, where a query-and-fragment strip never
// reaches it.
func TestSanitizeSnapshotURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"https://example.com/reg?token=SECRET&x=1", "https://example.com"},
		{"https://example.com/p#frag-SECRET", "https://example.com"},
		{"https://example.com/path", "https://example.com"},
		// The shape the path-drop exists for: nothing but the path is a secret.
		{"https://example.org/reset/SECRET", "https://example.org"},
		// The authority keeps its port, and a zone-scoped IPv6 literal must come
		// back re-escaped — url.URL.Host holds it DECODED, so concatenating it
		// would emit a bare '%' and produce a URL that no longer parses.
		{"https://[2001:db8::1]:8443/reg?t=SECRET", "https://[2001:db8::1]:8443"},
		{"https://[fe80::1%25eth0]/reg?t=SECRET", "https://[fe80::1%25eth0]"},
		// An http(s)-shaped value that will not reduce to scheme+host fails CLOSED.
		// Both of these used to fall through to the truncating branch and come back
		// with their paths intact — the exact exposure the reduction exists to close.
		// `https:///reset/SECRET` parses cleanly with an EMPTY host and has no '?',
		// '#' or '@' to truncate at; the second fails to parse on the bad escape.
		{"https:///reset/SECRET", ""},
		{"https://example.org/reset/SEC%zzRET", ""},
		{"HTTPS:///reset/SECRET", ""}, // the run regex is case-insensitive, so this test is too
		// A value that never claimed to be a URL keeps the conservative fallback:
		// there is no scheme+host to reduce it to, and it is not URL data.
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

// TestSanitizeSnapshotText: config_snapshot is persisted UNENCRYPTED, and X's
// tweetText is operator-authored prose that routinely carries a registration link
// pasted out of a logged-in browser — query string and all. Every http/https run in
// the text goes through sanitizeSnapshotURL, so the two paths cannot disagree about
// what "stripped" means; the surrounding prose is left exactly as written, because
// this redacts links and does not go looking for secrets in sentences.
func TestSanitizeSnapshotText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"no url", "Join us at KubeCon!", "Join us at KubeCon!"},
		{
			"token in an embedded url",
			"Register https://events.lf.org/reg?access_token=SECRET now",
			"Register https://events.lf.org now",
		},
		{
			"every url in the text is stripped",
			"See https://a.example/x?sid=SECRET and https://b.example/y?key=SECRET2",
			"See https://a.example and https://b.example",
		},
		{
			"fragment goes too",
			"https://events.lf.org/reg#token-SECRET",
			"https://events.lf.org",
		},
		{
			// A run sanitizeSnapshotURL fails closed on leaves nothing behind,
			// which is the same answer the single-URL path gives.
			"embedded userinfo fails closed",
			"link: https://user:pass@example.com/x?token=SECRET", // secretlint-disable-line -- fixture asserting userinfo fails closed
			"link: ",
		},
		{
			// A URL with no query at all still loses its path: the snapshot
			// cannot tell a routing segment from a one-time token, and it has no
			// reader who needs the difference. What survives is the host, which
			// is what makes the redacted value still say anything at all.
			"a clean url keeps only its host",
			"Details at https://events.lf.org/kubecon/register",
			"Details at https://events.lf.org",
		},
		{"uppercase scheme", "HTTPS://events.lf.org/r?token=SECRET", "https://events.lf.org"},
		{
			// The run must not stop at the ']' closing an IPv6 literal host. It
			// used to: ']' is a run terminator (it ends a markdown link), so the
			// match was "https://[2001:db8::1" and everything after it — the path
			// AND the query — stayed in the snapshot as plain prose.
			"ipv6 literal host",
			"see https://[2001:db8::1]/reg?ticket=SECRET now",
			"see https://[2001:db8::1] now",
		},
		{
			"ipv6 literal with a port",
			"https://[2001:db8::1]:8443/reg?ticket=SECRET",
			"https://[2001:db8::1]:8443",
		},
		{
			// A zone-scoped literal is the shape a hex/colon-only bracket class
			// rejects, sending it back through the general alternative and
			// truncating at the bracket — the leak the branch exists to close.
			"ipv6 zone-scoped literal host",
			"https://[fe80::1%25eth0]/reg?ticket=SECRET",
			"https://[fe80::1%25eth0]",
		},
		{
			// The ']' terminator still has to work where it means what it meant
			// before: a bracket closing around an ordinary URL.
			"bracketed ordinary url",
			"[https://events.lf.org/r?token=SECRET]",
			"[https://events.lf.org]",
		},
		{
			// An apostrophe is a sub-delimiter, legal inside a query, so it must
			// not end the run. It used to: the match stopped at the quote and
			// left `'api_token=SECRET` behind as bare prose in the snapshot.
			"apostrophe inside the query",
			"https://events.example/reg?x=discard'api_token=SECRET",
			"https://events.example",
		},
		{
			// The apostrophe that ends an English possessive still gets swept in
			// with the URL. Over-matching trailing punctuation costs a snapshot
			// nothing; under-matching leaks.
			"possessive apostrophe after a url",
			"see https://events.lf.org/r?token=SECRET's page",
			"see https://events.lf.org page",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSnapshotText(tc.in); got != tc.want {
				t.Errorf("sanitizeSnapshotText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(sanitizeSnapshotText(tc.in), "SECRET") {
				t.Errorf("sanitizeSnapshotText(%q) left a secret in the snapshot", tc.in)
			}
		})
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

	// errRows supplies a row value to return ALONGSIDE an errs entry. A repository is free to
	// hand back a partially populated value with a failure, and callers must key on the error
	// rather than on the value being nil — a fixture that only ever returned nil on failure
	// would let that confusion pass untested.
	errRows map[string]*model.Connection

	// nilNil names project ids for which Get returns (nil, nil) — the shape
	// domain.ConnectionReader permits and does not forbid. A fixture that can only report
	// absence as ErrNotFound makes a caller's nil-row handling untestable, so a missing guard
	// reads as covered right up until a repository chooses the other shape.
	nilNil map[string]bool
}

func (f *scopedConnReader) Disconnected(_ context.Context, projectID string, _ model.Provider) (bool, error) {
	if f.disconnectErr != nil {
		return false, f.disconnectErr
	}
	return f.tombstoned[projectID], nil
}

func (f *scopedConnReader) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	f.gets = append(f.gets, projectID)
	if f.nilNil[projectID] {
		return nil, nil
	}
	if err, ok := f.errs[projectID]; ok {
		return f.errRows[projectID], err
	}
	if c, ok := f.rows[projectID]; ok {
		return c, nil
	}
	return nil, domain.ErrNotFound
}

func usableConn(creds, accountID string) *model.Connection {
	return &model.Connection{
		// ID and Version model a real row rather than a zero value. The credential cache keys
		// entries on both, and every row Get can return has an id and a version of at least 1
		// (`version BIGINT NOT NULL DEFAULT 1`, migration 000001). A fixture left at the zero
		// value would let two DIFFERENT rows look identical to the cache within one credsSource
		// — a trap for whoever next reuses a source across a row swap, and one that no
		// production path can reach.
		ID:                   "conn-" + accountID,
		Version:              1,
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
//
// Runs over EVERY provider rather than one. Until the paid-ads gate was lifted from systemConn,
// only paid-ads providers could reach the system row at all, so a single Google Ads case
// covered the reachable set. HubSpot now resolves it too — which means the email channel can
// now hit this error path, and an untested attribution here is the difference between paging
// whoever installed the LF credential and telling a foundation to repair a connection it does
// not have and cannot see. Walking AllProviders() keeps that true for the next provider as well.
func TestUnusableSystemConnectionKeepsItsOrigin(t *testing.T) {
	broken := func() *model.Connection {
		c := usableConn(`{"sys":true}`, "sys-account")
		c.EncryptedCredentials = nil
		return c
	}

	for _, p := range model.AllProviders() {
		t.Run(string(p), func(t *testing.T) {
			sysRow := broken()
			sysRow.Provider = p
			_, err := newCredsSource(&scopedConnReader{
				rows: map[string]*model.Connection{model.SystemProjectID: sysRow},
			}, identityEncryptor{}).resolve(context.Background(), "cncf", p)
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("system fallback err = %v, want ErrConnectionNotUsable", err)
			}
			if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
				t.Errorf("system fallback err = %v, want it to name the SYSTEM connection", err)
			}

			// The project's own broken row must NOT pick up the system marker, or every 400
			// that tells an owner to fix their connection becomes a 500 that tells nobody
			// anything.
			ownRow := broken()
			ownRow.Provider = p
			_, err = newCredsSource(&scopedConnReader{
				rows: map[string]*model.Connection{"cncf": ownRow},
			}, identityEncryptor{}).resolve(context.Background(), "cncf", p)
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("project connection err = %v, want ErrConnectionNotUsable", err)
			}
			if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
				t.Errorf("project connection err = %v, must not be attributed to the system account", err)
			}
		})
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
			_, err := d.resolveGoogleAdsClient(context.Background(), "cncf", model.ProviderGoogleAds, nil)
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

// TestHubSpotResolvesTheLFPortalWhenTheProjectHasNoConnection is the email channel's half of the
// fallback, and the direct inverse of what this file asserted before LFXV2-3040 was re-scoped.
//
// It is the case the whole email channel rests on: LF runs one HubSpot portal with one org-wide
// private app token, so a foundation with no connection of its own must resolve that row —
// otherwise audience builds and email dispatch have no credential at all and the channel cannot
// run for any project but the one that happens to hold a row.
func TestHubSpotResolvesTheLFPortalWhenTheProjectHasNoConnection(t *testing.T) {
	sysRow := usableConn(`{"sys":true}`, "lf-portal")
	sysRow.Provider = model.ProviderHubSpot
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sysRow}}

	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderHubSpot)
	if err != nil {
		t.Fatalf("resolve(hubspot) = %v, want the LF portal row: every foundation shares one portal", err)
	}
	if !got.fromSystem {
		t.Errorf("fromSystem = false, want the resolution attributed to the system scope so a defect pages whoever installed the LF credential")
	}
	if got.accountID != "lf-portal" {
		t.Errorf("accountID = %q, want %q", got.accountID, "lf-portal")
	}
	// The system scope MUST be consulted here — that round-trip is the fallback doing its job.
	var askedSystem bool
	for _, scope := range repo.gets {
		if scope == model.SystemProjectID {
			askedSystem = true
		}
	}
	if !askedSystem {
		t.Errorf("scopes asked = %v, want the system scope consulted after the project's own miss", repo.gets)
	}
}

// TestSystemFallbackResolvesForEveryProvider pins the invariant that replaced the paid-ads gate.
//
// This test previously asserted the OPPOSITE for the email channel — that HubSpot must never
// resolve the LF system row (LFXV2-3040). That gate was scoped to the wrong axis. Its stated
// hazard was one tenant's contact lists landing in another tenant's CRM portal, which describes a
// per-tenant-portal topology; every LF foundation shares the one LF portal and a single org-wide
// token. The code already assumes exactly that: list names are PORTAL-GLOBAL and are disambiguated
// by event name plus build ref (internal/audience Plan.listName), never by tenancy. So the gate
// protected nothing and left the email channel unable to resolve any credential at all — bootstrap
// refused to install the very row the fallback refused to read.
//
// Walking AllProviders() is deliberate and unchanged in spirit: a provider added later is covered
// without anyone remembering to add a case, so the fallback and the provider list cannot drift.
func TestSystemFallbackResolvesForEveryProvider(t *testing.T) {
	for _, p := range model.AllProviders() {
		t.Run(string(p), func(t *testing.T) {
			row := usableConn(`{"sys":true}`, "sys-account")
			row.Provider = p
			repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: row}}

			res, err := newCredsSource(repo, identityEncryptor{}).
				resolve(context.Background(), "cncf", p)
			if err != nil {
				t.Fatalf("resolve(%s): %v; a project with no connection of its own falls back to the LF row", p, err)
			}
			if !res.fromSystem {
				t.Errorf("resolve(%s): fromSystem = false, want the resolution attributed to the system scope", p)
			}
		})
	}
}

// TestSystemFallbackRequiresAGenuineAbsence is the half of the old gate that still holds, and it
// is the one doing the real work: the fallback exists for a project that never said anything, not
// for one whose own row is present and unusable. Removing the paid-ads gate must not widen THIS.
func TestSystemFallbackRequiresAGenuineAbsence(t *testing.T) {
	for _, p := range model.AllProviders() {
		t.Run(string(p), func(t *testing.T) {
			own := usableConn(`{"own":true}`, "own-account")
			own.Provider = p
			sys := usableConn(`{"sys":true}`, "sys-account")
			sys.Provider = p
			repo := &scopedConnReader{rows: map[string]*model.Connection{
				"cncf":                own,
				model.SystemProjectID: sys,
			}}

			res, err := newCredsSource(repo, identityEncryptor{}).
				resolve(context.Background(), "cncf", p)
			if err != nil {
				t.Fatalf("resolve(%s): %v; the project's OWN connection must resolve", p, err)
			}
			if res.fromSystem {
				t.Errorf("resolve(%s): fromSystem = true; a project with its own connection must never be served the LF row", p)
			}
			if res.accountID != "own-account" {
				t.Errorf("resolve(%s): accountID = %q, want the project's own account", p, res.accountID)
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
// The refusal is implemented by declining to resolve the system scope at all (resolveOwned)
// rather than by rejecting a resolved value that came from it, so the sub-tests below pin both
// the outcome and the mechanism.
//
// The test pins the boundary in both directions: refuse when the project has no connection of
// its own, proceed when it does. Without the second half a blanket refusal would pass.
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

	// The system row's HEALTH must not reach this path. resolve loads, validates and decrypts
	// the fallback row before any caller can inspect where the credentials came from, so a
	// system connection with no credential blob fails resolution outright — the caller gets
	// ErrSystemConnectionNotUsable (a 500 paging whoever installed the LF credential) instead
	// of the 409 above, and never sees a resolved value to reject. Two things are wrong with
	// that: the reader's own remedy is unchanged (connect your own ad account), and the row
	// being complained about is one adoption would have refused in perfect health. resolveOwned
	// declines to look at all, which is what makes the refusal independent of the fallback.
	t.Run("an unusable system row does not change the answer", func(t *testing.T) {
		broken := usableConn(goodGoogleAdsCreds, "8666746580")
		broken.EncryptedCredentials = nil // the shape resolveConn rejects as permanently unusable
		d := NewGoogleAdsDispatcher(&scopedConnReader{
			rows: map[string]*model.Connection{model.SystemProjectID: broken},
		}, identityEncryptor{})

		_, err := d.LookupCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
		if !errors.Is(err, domain.ErrAdoptionRequiresOwnConnection) {
			t.Fatalf("err = %v, want ErrAdoptionRequiresOwnConnection — the caller's remedy does not "+
				"depend on the state of an LF row adoption refuses either way", err)
		}
		if errors.Is(err, domain.ErrSystemConnectionNotUsable) {
			t.Errorf("err = %v: the system row's defect leaked onto a path that never uses it", err)
		}
	})

	// Stronger than the assertions above, and the one that keeps them true: the fallback row is
	// never READ. Every future failure mode of the system scope is covered by this, whereas each
	// sentinel assertion only covers the one it names.
	t.Run("the system scope is never consulted", func(t *testing.T) {
		repo := &scopedConnReader{
			rows: map[string]*model.Connection{model.SystemProjectID: usable()},
		}
		d := NewGoogleAdsDispatcher(repo, identityEncryptor{})

		_, _ = d.LookupCampaign(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890")
		for _, got := range repo.gets {
			if got == model.SystemProjectID {
				t.Fatalf("Get(%q) was called; adoption must resolve the project scope only, so that "+
					"no state of the LF row can reach this path", model.SystemProjectID)
			}
		}
	})

	// NEITHER scope has a connection. Under resolveOwned this is now the same code path as the
	// cases above — which is the point, and is exactly why it stays: it pins that the answer is
	// the presence or absence of the PROJECT's row and nothing else, so the three cases cannot
	// drift apart again by someone reintroducing a fallback-shaped special case.
	//
	// What it pins on its own is the translation. resolveOwned reports the absence as a wrapped
	// domain.ErrNotFound; left untranslated the adopt switch has no ErrNotFound arm and answers
	// 503 "could not be reached" — for a platform that was never contacted, about a state no
	// retry can change.
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

// TestSanitizeSnapshotText_UnderscorePrefixedURL pins the boundary bug: Go's `\b` counts
// `_` as a word character, so `_https://…` matched nothing and the credential survived
// intact into the UNENCRYPTED config_snapshot.
func TestSanitizeSnapshotText_UnderscorePrefixedURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"underscore before the scheme",
			"_https://events.example/cb?access_token=SECRET",
			"_https://events.example",
		},
		{
			"markdown italics around the link",
			"see _https://events.example/cb?access_token=SECRET_ now",
			"see _https://events.example now",
		},
		{
			"scheme buried in a word still redacts",
			"foohttps://events.example/cb?access_token=SECRET",
			"foohttps://events.example",
		},
		{
			"one bounded and one underscored link in the same text",
			"see https://a.example/r?token=S1 and _https://b.example/r?sid=S2",
			"see https://a.example and _https://b.example",
		},
	} {
		if got := sanitizeSnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeSnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Round-12 review fixes

// TestSanitizeSnapshotText_SchemelessLink pins the asymmetry the round-11 commit opened:
// the twitter client learned that X linkifies and publishes scheme-less links, and this
// redactor still required a scheme — so the query and fragment of one survived whole into
// the UNENCRYPTED config_snapshot.
func TestSanitizeSnapshotText_SchemelessLink(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"bare host with a credential query",
			"register at events.example/cb?access_token=SECRET today",
			"register at events.example today",
		},
		{
			"www host with a fragment",
			"see www.events.example/r#sid=SECRET",
			"see www.events.example",
		},
		{
			"IPv4 host",
			"go to 198.51.100.7/r?access_token=SECRET",
			"go to 198.51.100.7",
		},
		{
			"punycode TLD",
			"go to events.xn--p1ai/r?access_token=SECRET",
			"go to events.xn--p1ai",
		},
		{
			"port survives, query does not",
			"events.example:8443/r?token=SECRET",
			"events.example:8443",
		},
		{
			"a scheme-ful link is reduced once, not twice",
			"see https://a.example/r?token=S1 and b.example/r?token=S2",
			"see https://a.example and b.example",
		},
		{
			"userinfo fails closed",
			"see user:pw@events.example/r?token=SECRET now",
			"see  now",
		},
		{
			"a dotted token with no query is left alone",
			"read agenda.md and Node.js v1.2 notes",
			"read agenda.md and Node.js v1.2 notes",
		},
	} {
		if got := sanitizeSnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeSnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Round-13 review fixes

// TestSanitizeSnapshotText_SchemelessUserinfo pins the snapshot half of the same gap: a
// scheme-less `user:password@host` has no query, so neither earlier pass touched it and
// the password persisted whole in the UNENCRYPTED config_snapshot.
func TestSanitizeSnapshotText_SchemelessUserinfo(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"no query at all",
			"see bob:SECRET@events.example now",
			"see  now",
		},
		{
			"with a path",
			"see bob:SECRET@events.example/portal now",
			"see  now",
		},
		{
			"IPv4 host with a port",
			"admin:SECRET@198.51.100.7:8443/portal",
			"",
		},
		{
			"an email address is left alone",
			"contact bob@events.example for details",
			"contact bob@events.example for details",
		},
		{
			"a time of day is not userinfo",
			"doors 9:30, ask bob@events.example",
			"doors 9:30, ask bob@events.example",
		},
		// Round-14: the rows above all put punctuation between the clock and the host,
		// which is what hid the false positive. Hard against the host is the shape an
		// events platform actually writes, and it is the userinfo production byte for
		// byte. Kept in step with userinfoRunIsClockShaped on the twitter side.
		{
			"a clock hard against a host is left alone",
			"keynote 14:00@events.example",
			"keynote 14:00@events.example",
		},
		{
			"a clock with a path is left alone",
			"session 9:30@main.stage/agenda",
			"session 9:30@main.stage/agenda",
		},
		{
			"one non-digit side is a credential again",
			"see 9:SECRET@events.example now",
			"see  now",
		},
	} {
		if got := sanitizeSnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeSnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Round-15 review fixes

// TestSanitizeSnapshot_MalformedHTTPAuthority pins the gap a single missing slash opened.
// `http:/reset/SECRET` announces the scheme, so it is a link by anyone's reading — but it
// parses with an EMPTY host, which failed the scheme+host reduction, and the `//`
// requirement on both the run pattern and isHTTPScheme then failed it out of the
// fail-closed branch too. It reached config_snapshot whole, path and all.
func TestSanitizeSnapshot_MalformedHTTPAuthority(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"one slash, as a field", "http:/reset/SECRET", ""},
		{"no slashes (opaque), as a field", "https:reset/SECRET", ""},
		{"uppercase scheme, as a field", "HTTP:/reset/SECRET", ""},
	} {
		if got := sanitizeSnapshotURL(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeSnapshotURL(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ name, in, want string }{
		{
			"one slash, inside free text",
			"reset here http:/reset/SECRET now",
			"reset here  now",
		},
		{
			"no slashes, inside free text",
			"reset here https:reset/SECRET now",
			"reset here  now",
		},
		{
			// The run pattern needs something hard against the colon, so prose that
			// merely ends a clause with the word is untouched.
			"a bare scheme word in prose is untouched",
			"over http: and https: alike",
			"over http: and https: alike",
		},
		{
			"a well-formed link beside a malformed one still reduces normally",
			"see https://a.example/r?t=S1 or http:/reset/S2",
			"see https://a.example or ",
		},
	} {
		if got := sanitizeSnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeSnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestSanitizeSnapshot_SchemelessPathOnlyLink pins the one URL shape that reached
// config_snapshot with its path intact: a scheme-less link whose secret is IN the path and
// which carries no query, fragment or userinfo for any earlier pass to fire on — the
// password-reset link sanitizeSnapshotURL's own doc comment names as the realistic case.
//
// It reduces to the host rather than blanking: the host is the load-bearing half for the
// human reading the snapshot, and blanking would make the same link redact differently
// depending on whether the operator typed `https://` in front of it.
func TestSanitizeSnapshot_SchemelessPathOnlyLink(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"path-only reset link",
			"reset at events.example/reset/SECRET today",
			"reset at events.example today",
		},
		{
			"www host",
			"see www.events.example/reset/SECRET",
			"see www.events.example",
		},
		{
			"IPv4 host with a port",
			"go to 198.51.100.7:8443/reset/SECRET",
			"go to 198.51.100.7:8443",
		},
		{
			"trailing slash only",
			"events.example/ is the site",
			"events.example is the site",
		},
		{
			// The earlier passes own these two, and this one must not second-guess
			// them: the userinfo pass already blanked every credential-carrying run, so
			// an '@' still standing was kept on purpose.
			"a clock with a path is still left alone",
			"session 9:30@main.stage/agenda",
			"session 9:30@main.stage/agenda",
		},
		{
			"an email with a path is left alone",
			"contact bob@events.example/team for details",
			"contact bob@events.example/team for details",
		},
		{
			"a reduced scheme-ful link is not reduced a second time",
			"see https://a.example/r?token=S",
			"see https://a.example",
		},
		{
			// A slash is the only discriminator here, so the TLD-shaped final label is
			// what holds the pass off ordinary prose.
			"prose with a slash and no dotted host",
			"and/or, 9.5/10, read agenda.md",
			"and/or, 9.5/10, read agenda.md",
		},
	} {
		if got := sanitizeSnapshotText(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeSnapshotText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// The field path reduces it the same way, through the same helper — a field and the
	// same link written inside tweetText must not redact differently.
	if got := sanitizeSnapshotURL("events.example/reset/SECRET"); got != "events.example" {
		t.Errorf("sanitizeSnapshotURL path-only link = %q, want %q", got, "events.example")
	}
	// A value that never claimed to be a link still falls through the truncating branch.
	if got := sanitizeSnapshotURL("t3_abc123"); got != "t3_abc123" {
		t.Errorf("sanitizeSnapshotURL(%q) = %q, want it untouched", "t3_abc123", got)
	}
}
