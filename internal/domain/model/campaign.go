// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"time"
)

// BudgetType is the pacing model for a campaign's budget.
type BudgetType string

// Budget types.
const (
	BudgetDaily    BudgetType = "daily"
	BudgetLifetime BudgetType = "lifetime"
)

// Campaign is one platform's campaign, subordinate to a brief. A brief drives
// many campaigns (one per platform), discriminated by Platform and sharing
// BriefID. The row is updated in place (not recreated) when a brief changes
// after campaigns exist.
type Campaign struct {
	ID                 string
	ProjectID          string
	BriefID            string
	JobID              *string // creation job that produced this row (soft ref; no FK)
	Platform           Provider
	PlatformCampaignID string // ID returned by the ad platform
	CampaignName       string
	Status             string
	BudgetAmount       *float64
	BudgetType         *BudgetType
	StartDate          *time.Time
	EndDate            *time.Time
	ConfigSnapshot     json.RawMessage
	Result             json.RawMessage
	Version            int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Campaign.Status is a plain string that carries TWO kinds of value: a provisioning state
// stamped by the create/dispatch flow (pending / created / created_degraded) and a run state
// set by the status toggle (active / paused).
//
// Run states — the two a caller can toggle a live campaign between (match the design enum,
// mapped to each platform's own vocabulary by its dispatcher):
const (
	CampaignRunActive = "active"
	CampaignRunPaused = "paused"
)

// IsCampaignRunStatus reports whether status is one of the two RUN states (active/paused) a
// caller sets via the platform status toggle — as opposed to a provisioning state. The DB-only
// update path uses it to refuse a run-state change that would bypass the ad platform.
func IsCampaignRunStatus(status string) bool {
	return status == CampaignRunActive || status == CampaignRunPaused
}

// Provisioning states — stamped during creation. Mirrors the dispatch package's
// campaignStatusCreated/CreatedDegraded and the orchestrator's "pending" placeholder. The
// status toggle keys off these: it is safe to toggle a fully-created campaign, but a
// "pending" (ambiguous orphan) or "created_degraded" (a sub-step still needs reconciliation)
// campaign must NOT be toggled — doing so would activate an incomplete campaign and/or erase
// the reconciliation marker.
const (
	CampaignStatusPending         = "pending"
	CampaignStatusCreated         = "created"
	CampaignStatusCreatedDegraded = "created_degraded"
)

// CampaignStatusToggleable reports whether a campaign in the given status may have its run
// state toggled: only a cleanly-created campaign (or one already in a run state) is safe. A
// pending/degraded/other provisioning state is not — see the provisioning-state constants.
func CampaignStatusToggleable(status string) bool {
	switch status {
	case CampaignStatusCreated, CampaignRunActive, CampaignRunPaused:
		return true
	default:
		return false
	}
}

// JobStatus is the status vocabulary shared by campaign_jobs and the API's
// JobCreateResponse/JobPollResponse.
type JobStatus string

// Job statuses. 'partial' = some platforms succeeded, some failed.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobPartial   JobStatus = "partial"
	JobFailed    JobStatus = "failed"
)

// Terminal reports whether the job has reached a final state.
func (s JobStatus) Terminal() bool {
	switch s {
	case JobSucceeded, JobPartial, JobFailed:
		return true
	default:
		return false
	}
}

// CampaignJob is the async multi-platform dispatch record. One job per brief
// submission dispatches to multiple Campaign rows (one per platform).
type CampaignJob struct {
	ID        string
	BriefID   string
	Status    JobStatus
	Result    json.RawMessage
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

// MetricsWindow is a platform-agnostic time window for campaign metrics reporting.
type MetricsWindow string

// Metrics windows supported by platform dispatchers.
const (
	MetricsWindowToday      MetricsWindow = "today"
	MetricsWindowLast7Days  MetricsWindow = "last_7_days"
	MetricsWindowLast30Days MetricsWindow = "last_30_days"
	MetricsWindowThisMonth  MetricsWindow = "this_month"
	MetricsWindowLastMonth  MetricsWindow = "last_month"
)

// CampaignMetrics is a platform-agnostic snapshot of campaign performance metrics
// for a given time window. All platforms returning metrics must map their responses
// to this shape.
type CampaignMetrics struct {
	// Impressions is the total number of ad impressions served during the window.
	Impressions int64
	// Clicks is the total number of ad clicks during the window.
	Clicks int64
	// CostMicros is the total spend in micros (amount * 1,000,000) of the platform's
	// currency during the window.
	CostMicros int64
	// CTR is the click-through rate (clicks / impressions), a decimal between 0 and 1.
	// If impressions is 0, CTR is 0.
	CTR float64
}
