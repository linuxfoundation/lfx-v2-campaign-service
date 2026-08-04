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
	// CampaignStatusDeleted is the terminal SOFT-DELETE state. The row is retained
	// (it holds platform_campaign_id, the only local pointer to a campaign that may
	// still exist and still be spending upstream) but is invisible to reads and
	// excluded from the (brief_id, platform) partial unique index, which frees the
	// slot for a re-dispatch. Deleting never touches the ad platform — see the
	// service layer.
	CampaignStatusDeleted = "deleted"
	// CampaignStatusGroupCreated and CampaignStatusUnconfirmed are the RETAINED PARTIAL
	// orphan statuses: the upstream create did not complete (only a sub-resource such as
	// the campaign group exists, or the outcome is ambiguous). The orchestrator preserves
	// them on the row so it records WHAT went wrong, and treats them as non-reusable so a
	// retry re-attempts the create. Mirrored from the service package's
	// partialOrphanStatuses / the dispatch package's literals, which are unexported;
	// declared here so packages that cannot import those (postgres) can still reason
	// about them. Drift-guarded by TestPartialOrphanStatusValues in internal/dispatch.
	CampaignStatusGroupCreated = "group_created"
	CampaignStatusUnconfirmed  = "unconfirmed"
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

// CampaignStatusNeedsReconciliation reports whether a campaign's status is a marker that an
// operator or a resume pass still has to resolve, rather than a settled outcome.
//
// The three statuses here all mean "the local row and the ad platform may disagree, and this
// string is the only record of how":
//
//   - pending: a bare dispatch claim. Either an in-flight dispatch owns it, or a dispatch
//     died mid-flight and may have created a campaign upstream that has no local id.
//   - group_created / unconfirmed: a partial orphan — a sub-resource exists, or the create
//     outcome is ambiguous.
//
// Acting on such a row OVERWRITES that marker and so destroys the signal. This is the same
// doctrine CampaignStatusToggleable enforces for the run-state toggle; it is a separate
// predicate because the two answer different questions and legitimately differ on
// created_degraded, which is fully created upstream (safe to retire, and to toggle only
// after reconciliation).
func CampaignStatusNeedsReconciliation(status string) bool {
	switch status {
	case CampaignStatusPending, CampaignStatusGroupCreated, CampaignStatusUnconfirmed:
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
