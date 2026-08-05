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

// MetricsWindow is a platform-agnostic reporting window for a live metrics read. It is a
// closed vocabulary (not a platform-defined literal) so the API surface never leaks one
// platform's dialect — each MetricsReader adapter maps these values to its own platform's
// query vocabulary (e.g. Google Ads' GAQL DURING literals, Meta's Insights date_preset).
type MetricsWindow string

// Metrics windows in the platform-agnostic API vocabulary. A MetricsReader adapter may
// support only a subset of these and report ErrMetricsWindowUnsupported for unsupported values.
const (
	MetricsWindowToday      MetricsWindow = "today"
	MetricsWindowYesterday  MetricsWindow = "yesterday"
	MetricsWindowLast7Days  MetricsWindow = "last_7_days"
	MetricsWindowLast14Days MetricsWindow = "last_14_days"
	MetricsWindowLast30Days MetricsWindow = "last_30_days"
	MetricsWindowThisMonth  MetricsWindow = "this_month"
	MetricsWindowLastMonth  MetricsWindow = "last_month"
)

// IsValidMetricsWindow reports whether w is one of the closed set of supported windows. The
// Goa HTTP layer already enforces the enum on requests that arrive over HTTP, but the service
// layer validates independently — the same defense-in-depth as
// IsCampaignRunStatus/CampaignStatusToggleable — so a direct/test caller can't pass an
// unmapped value through to a platform adapter.
func IsValidMetricsWindow(w MetricsWindow) bool {
	switch w {
	case MetricsWindowToday, MetricsWindowYesterday, MetricsWindowLast7Days, MetricsWindowLast14Days,
		MetricsWindowLast30Days, MetricsWindowThisMonth, MetricsWindowLastMonth:
		return true
	default:
		return false
	}
}

// CampaignMetrics is a platform-agnostic, live read-through performance
// snapshot for one campaign over one window. It is never persisted — a
// MetricsReader dispatcher call populates it fresh on every read, the same
// way StatusToggler's ToggleStatus call is always live rather than
// DB-cached.
type CampaignMetrics struct {
	CampaignID  string
	Window      MetricsWindow
	Impressions int64
	Clicks      int64
	CostMicros  int64
	// Ctr is Clicks/Impressions, 0 when Impressions is 0 (never divides by zero).
	Ctr float64
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
