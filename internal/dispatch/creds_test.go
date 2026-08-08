// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
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

// TestResolveWithNoSystemAccountReportsTheProject: two misses, but the error names the caller's.
func TestResolveWithNoSystemAccountReportsTheProject(t *testing.T) {
	repo := &scopedConnReader{}
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "cncf") || strings.Contains(err.Error(), model.SystemProjectID) {
		t.Errorf("err = %q, want it to name the project and not the reserved scope", err)
	}
}

// TestResolveHoldsTheSystemAccountToTheSameStandard: refused, not trusted because it is ours.
func TestResolveHoldsTheSystemAccountToTheSameStandard(t *testing.T) {
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: {Provider: model.ProviderGoogleAds, Status: model.StatusActive},
	}}
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("err = %v, want ErrConnectionNotUsable for a system row with no credentials", err)
	}
}

// TestSystemLookupFailureIsNotAnAbsence: a DB error on the fallback is a 503, not a 404.
func TestSystemLookupFailureIsNotAnAbsence(t *testing.T) {
	repo := &scopedConnReader{errs: map[string]error{model.SystemProjectID: errors.New("connection refused")}}
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want a non-absence error", err)
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
