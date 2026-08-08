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
	// CreatedBy / UpdatedBy name the human behind the write. Nil means "not
	// recorded", which has THREE causes, and they are not equally benign:
	//
	//   1. the row predates actor attribution (historical, expected);
	//   2. the write was system-initiated with no person behind it (expected);
	//   3. a request principal WAS present but could not be decoded, so
	//      attributedActor returned nil after logging a warning.
	//
	// Case 3 is a regression signal, not a normal absence — a real person made this
	// write and the audit trail lost them — but NOTHING IN THIS SERVICE SEPARATES IT
	// FROM CASE 2. `attributedActor` logs one warning, "write attempted with no
	// authenticated actor", for every nil, and the request context carries only the
	// decoded actor: there is no record of whether a token was present and
	// undecodable versus absent entirely, so the warning cannot tell you which. Do
	// not go looking for a per-cause log line; there isn't one.
	//
	// What separates them is evidence from OUTSIDE this service — gateway or ingress
	// logs showing whether the request carried an Authorization header — plus the
	// SHAPE of the warning rate: a steady trickle is ordinary unauthenticated or
	// system traffic, a step change across every write is the auth path having
	// broken. Read a nil as "unattributed, cause not yet determined", never as proof
	// the row is simply old or system-written. Nil never means "nobody".
	//
	// These claims are decoded from the bearer token WITHOUT verifying its
	// signature; Heimdall validates the token at the gateway before the request
	// reaches this service (see service.JWTAuth). The trustworthiness of this
	// audit trail is therefore exactly the trustworthiness of that gateway — a
	// forged token that reached the service would produce a forged row and
	// nothing here would notice. In-app JWKS verification is a follow-up.
	CreatedBy *Actor
	UpdatedBy *Actor
	CreatedAt time.Time
	UpdatedAt time.Time
}
