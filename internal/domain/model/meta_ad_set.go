// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// MetaAdSet is one ad set under a Meta campaign, read live (LFXV2-2665, list-meta-ad-sets). It is
// never persisted.
type MetaAdSet struct {
	ID string
	// Listed is false for an ad set that delivered in the window but that the ad-set listing did
	// not return (deleted or archived upstream since): only its counters are known.
	Listed          bool
	Name            *string
	Status          *string
	EffectiveStatus *string
	BidStrategy     *string
	// BudgetType is BudgetDaily or BudgetLifetime; nil when the ad set holds no budget of its own
	// (Campaign Budget Optimization) or was not listed.
	BudgetType *BudgetType
	// BudgetAmount is the ad set's budget in whole units of the account currency, rendered with
	// two decimals from the minor units Meta reports. nil when there is no ad-set budget or the
	// currency's minor-unit scale is not known — never a guessed scale.
	BudgetAmount *string
	Impressions  int64
	Clicks       int64
	// CostMicros is spend over the window in micros of MetaAdSets.Currency.
	CostMicros int64
	Ctr        float64
	// Recorded marks the ad set this service created for the campaign (the row's recorded ad set
	// id). An adopted row records none, so no ad set is marked.
	Recorded bool
}

// MetaAdSets is the ad-set read for one campaign.
type MetaAdSets struct {
	CampaignID         string
	PlatformCampaignID string
	Window             MetricsWindow
	Currency           string
	ReadAt             time.Time
	AdSets             []MetaAdSet
}

// Meta ad-set status-toggle outcomes (toggle-meta-ad-set-status). A definite failure is an
// error, never an outcome.
const (
	// MetaAdSetApplied — Meta confirmed the write.
	MetaAdSetApplied = "APPLIED"
	// MetaAdSetUnconfirmed — the single write may or may not have been applied.
	MetaAdSetUnconfirmed = "UNCONFIRMED"
	// MetaAdSetAlreadyInState — the pre-write read showed the requested status; nothing was sent.
	MetaAdSetAlreadyInState = "ALREADY_IN_STATE"
)

// Meta ad-set run states, in Meta's own vocabulary (the toggle's request enum).
const (
	MetaAdSetStatusActive = "ACTIVE"
	MetaAdSetStatusPaused = "PAUSED"
)

// MetaAdSetStatusResult is a toggle-meta-ad-set-status outcome that is not a definite failure.
type MetaAdSetStatusResult struct {
	AdSetID string
	// Outcome is MetaAdSetApplied or MetaAdSetAlreadyInState. (UNCONFIRMED travels as an error
	// implementing Unconfirmed() so it is never mistaken for success; the service renders it.)
	Outcome string
	// PreviousStatus is the configured status the pre-write read observed.
	PreviousStatus string
}
