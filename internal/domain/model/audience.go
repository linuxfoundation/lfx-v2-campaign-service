// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrAudienceBuiltNeedsMasterList is returned when an audience is marked built but
// has no platform master-list id — an inconsistent state, since AudienceBuilt is
// DEFINED as "the master list exists in the platform".
var ErrAudienceBuiltNeedsMasterList = errors.New("a built audience must have a platform_master_list_id")

// AudienceStatus is the lifecycle of a built audience.
type AudienceStatus string

// Audience statuses.
const (
	// AudienceBuilding — the platform lists are being assembled.
	AudienceBuilding AudienceStatus = "building"
	// AudienceBuilt — the master list (and suppressions) exist in the platform.
	AudienceBuilt AudienceStatus = "built"
	// AudienceFailed — the build did not complete.
	AudienceFailed AudienceStatus = "failed"
)

// CampaignAudience is a built marketing audience, subordinate to a brief. It stores
// a POINTER + provenance to an audience that physically lives in the platform (a
// HubSpot master contact list), NOT the audience contents. It makes a built audience
// a first-class, inspectable, reusable, versioned LFX resource — the caller answers
// "what audience did we send to?" and can reference or rebuild it without going into
// the platform. A brief may have several audiences (over time / per platform).
type CampaignAudience struct {
	ID        string
	ProjectID string
	BriefID   string
	Platform  Provider
	// PlatformMasterListID is the pointer to the real audience in the platform (a
	// HubSpot master list id). Empty until the build succeeds.
	PlatformMasterListID string
	// SuppressionListIDs are the platform suppression list ids applied to the master.
	SuppressionListIDs json.RawMessage
	// InclusionSummary is human-readable provenance: how the audience was built
	// (which past events, geo/topic segments), the part not visible from the list.
	InclusionSummary string
	Status           AudienceStatus
	Version          int64
	CreatedBy        json.RawMessage
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// StatusOrDefault returns the status, defaulting an empty value to AudienceBuilding
// (matching the campaign_audiences DEFAULT) so a caller that omits it on create
// doesn't write an empty string that would violate the status CHECK constraint.
func (a *CampaignAudience) StatusOrDefault() AudienceStatus {
	if a.Status == "" {
		return AudienceBuilding
	}
	return a.Status
}

// Validate enforces the cross-field invariant that a BUILT audience must carry its
// platform master-list pointer — AudienceBuilt is defined as "the master list exists".
// It is called before persisting on both create and update (after the patch merge), so
// no path — a create with status=built and no id, a status-only patch to built on a row
// with no id, or clearing the id on an already-built row — can leave the stored row
// claiming a list that isn't pointed at. It evaluates the EFFECTIVE status
// (StatusOrDefault), so an omitted status on create (→ building) is fine.
func (a *CampaignAudience) Validate() error {
	if a.StatusOrDefault() == AudienceBuilt && strings.TrimSpace(a.PlatformMasterListID) == "" {
		return ErrAudienceBuiltNeedsMasterList
	}
	return nil
}
