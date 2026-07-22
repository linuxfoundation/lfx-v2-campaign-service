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

// CampaignStatusPending is the status of a dispatch CLAIM placeholder — a row that
// holds the single-flight claim for (brief, platform) before/while dispatch runs, and
// the state a partial upstream create is RETAINED in (a recoverable orphan). It is a
// LOAD-BEARING INVARIANT of the dispatch-claim machinery: ClaimCampaignDispatch's
// steal (`status = 'pending'`), DeleteDispatchClaim (`status='pending'`), and the
// orchestrator's retained-orphan write all pivot on this exact value AND on
// PlatformCampaignID being the EMPTY STRING (not NULL) for an un-completed claim. The
// SQL in campaign_repo.go hardcodes the literal `'pending'` (a Go constant can't be
// injected into a raw SQL string); this constant is the canonical spelling every Go
// writer must use so the two never drift. `campaigns.status` has no CHECK constraint,
// so this discipline is the only guard.
const CampaignStatusPending = "pending"

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
