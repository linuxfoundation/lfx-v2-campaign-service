// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"time"
)

// ProgramType is the funnel context a brief carries.
type ProgramType string

// Program types.
const (
	ProgramEvents     ProgramType = "events"
	ProgramEducation  ProgramType = "education"
	ProgramMembership ProgramType = "membership"
)

// Valid reports whether p is a known program type.
func (p ProgramType) Valid() bool {
	switch p {
	case ProgramEvents, ProgramEducation, ProgramMembership:
		return true
	default:
		return false
	}
}

// BriefStatus is the lifecycle status of a brief.
type BriefStatus string

// Brief statuses.
const (
	BriefDraft    BriefStatus = "draft"
	BriefApproved BriefStatus = "approved"
	BriefArchived BriefStatus = "archived"
)

// CampaignBrief is the funnel unit: it carries the program type and is shared
// across channels — one brief drives many Campaign rows (one per platform),
// all sharing brief_id. Briefs are indexed into the Query Service (unlike
// connections), so lists and revision history are served from there.
type CampaignBrief struct {
	ID           string
	ProjectID    string
	ProgramType  ProgramType
	EventSlug    string // UNIQUE with project_id
	URL          string
	Platforms    json.RawMessage // selected channels (a planning hint)
	EventDetails json.RawMessage
	Copy         json.RawMessage
	Keywords     json.RawMessage
	Targeting    json.RawMessage
	Status       BriefStatus
	Version      int64
	ApprovedBy   *Actor
	ApprovedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
