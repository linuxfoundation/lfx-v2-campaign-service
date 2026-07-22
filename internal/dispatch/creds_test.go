// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

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
