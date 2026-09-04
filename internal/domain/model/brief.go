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

// DeliveryType is the surface a brief was authored for. Paid and email are PARALLEL channels on
// one event rather than alternatives, which is why it participates in the brief's unique key
// (000030) instead of merely describing it.
type DeliveryType string

// Delivery types.
const (
	DeliveryPaidMarketing DeliveryType = "paid-marketing"
	DeliveryEmail         DeliveryType = "email"
)

// Valid reports whether d is a known delivery type.
//
// The empty string is NOT valid. A brief that names no surface cannot be scoped to one, and the
// column is NOT NULL with a CHECK constraint, so an empty value is a write that would be refused
// by the database rather than a value with a sensible meaning. Callers that mean "paid" say so.
func (d DeliveryType) Valid() bool {
	switch d {
	case DeliveryPaidMarketing, DeliveryEmail:
		return true
	default:
		return false
	}
}

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
	ID          string
	ProjectID   string
	ProgramType ProgramType
	// EventSlug is PART of a brief's composite identity, not the whole of it. 000030 widened the
	// unique index to (project_id, event_slug, delivery_type, stage) because one event carries a
	// paid brief and an email SERIES at the same time; an earlier comment here claimed the slug
	// was unique with project_id alone, which stopped being true with that migration.
	EventSlug string
	// DeliveryType names the surface that authored this brief. Paid and email are parallel
	// channels on one event, so this is what keeps them from displacing each other -- a read
	// scoped to one surface must not return the other's brief, and a write must not replace it.
	// Rows predating 000030 carry "paid-marketing", which is a fact about them rather than a
	// default: the paid surface was the only one whose brief could be saved.
	DeliveryType DeliveryType
	// Stage places this brief within an email series (CFP Launch, Registration Push, ...). Empty
	// for paid, which has no series -- and empty rather than absent because it participates in a
	// unique index, where NULLs never collide and would let duplicates accumulate unchecked.
	Stage string
	// AssertDeliveryType / AssertStage carry an UPDATE caller's explicit claim about this brief's
	// identity, and only that. Nil means "the request did not mention it".
	//
	// Separate from the two fields above because presence and value are different questions here,
	// and the wire can express both: `stage` is a *string on BriefInput, so an explicit
	// `"stage": ""` is a real request -- "move this to the paid stage" -- and is NOT the same as
	// omitting the field. Reading the plain Stage field alone flattens the two into "", so an
	// email brief asked to become a paid one was answered 200 with its stage unchanged: the
	// caller was told a rejected identity change had succeeded. Verified against a live database
	// before this field existed.
	//
	// Only ReplaceBrief reads them, and only to REJECT. They are never written to a column --
	// delivery_type and stage are immutable under 000030's key -- so they carry no meaning on
	// create, where the fields above already say what the brief IS.
	AssertDeliveryType *DeliveryType
	AssertStage        *string
	URL                string
	Platforms          json.RawMessage // selected channels (a planning hint)
	EventDetails       json.RawMessage
	Copy               json.RawMessage
	Keywords           json.RawMessage
	Targeting          json.RawMessage
	Status             BriefStatus
	Version            int64
	ApprovedBy         *Actor
	ApprovedAt         *time.Time
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
