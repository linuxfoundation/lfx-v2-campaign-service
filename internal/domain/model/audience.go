// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// BuiltInPortalID is the HubSpot portal the lists above were created in — the portal the
	// TOKEN authenticated against at build time, not the operator-supplied portal_id config,
	// which a credential swap leaves untouched.
	//
	// It exists for the same reason campaigns records its creating account: a HubSpot list id is
	// a bare numeric that means nothing outside its portal, so without this the row cannot say
	// what its own ids refer to. Dispatch resolves credentials AFRESH and prefers a project
	// connection added since the build, so an audience built on the LF portal can meet a client
	// authenticated against a different one — and SetSendList would be handed ids that portal
	// cannot see.
	//
	// EMPTY means "not recorded", never "the LF portal". Rows written before this column existed
	// carry none and are deliberately not backfilled; the dispatch guard reads absence as
	// unprovable and refuses, rather than assuming the portal that happens to resolve today.
	BuiltInPortalID string
	// SuppressionListIDs are the platform suppression list ids applied to the master.
	SuppressionListIDs json.RawMessage
	// IncludeListIDs are EXISTING platform lists the send goes to directly, recorded when an
	// operator attached several lists instead of one master (a JSON string array, encoded like
	// SuppressionListIDs). Nil/empty means "send to PlatformMasterListID alone", which is every
	// composed or built audience and every row written before the column existed. When set,
	// PlatformMasterListID holds its FIRST id, so the built-needs-a-master invariant (and the
	// CHECK constraint behind it) still holds and a reader that knows only the master column
	// still names a real recipient list. Read it through SendListIDs, never directly.
	IncludeListIDs json.RawMessage
	// InclusionSummary is human-readable provenance: how the audience was built
	// (which past events, geo/topic segments), the part not visible from the list.
	InclusionSummary string
	Status           AudienceStatus
	Version          int64
	CreatedBy        json.RawMessage
	// UpdatedBy names whoever touched the row LAST. Both inserts stamp it alongside
	// CreatedBy (nobody has edited yet, so the creator is the last to touch it), and
	// each edit replaces it. Nil means "not recorded" — an unauthenticated write — and
	// never "nobody".
	UpdatedBy json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
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
//
// This app-level check gives a friendly 400; the same invariant is ALSO enforced at the
// datastore by a CHECK constraint (migration 000006) so the build worker and any direct
// write cannot violate it either.
func (a *CampaignAudience) Validate() error {
	if a.StatusOrDefault() == AudienceBuilt && strings.TrimSpace(a.PlatformMasterListID) == "" {
		return ErrAudienceBuiltNeedsMasterList
	}
	return nil
}

// ErrAudienceIncludeListIDsUnreadable is returned by SendListIDs when include_list_ids holds
// something other than a JSON string array. Surfaced rather than ignored: falling back to the
// master list would send to a SUBSET of the recipients the row records, silently.
var ErrAudienceIncludeListIDsUnreadable = errors.New("the audience's include_list_ids could not be decoded")

// SendListIDs returns the platform list ids a send to this audience targets: the decoded
// IncludeListIDs (trimmed, blanks and duplicates dropped, order kept) when any are recorded,
// otherwise PlatformMasterListID alone. It returns nil when neither names a list — the caller
// decides whether that is an error (it is, for a built audience).
func (a *CampaignAudience) SendListIDs() ([]string, error) {
	if len(a.IncludeListIDs) > 0 && string(a.IncludeListIDs) != "null" {
		var raw []string
		if err := json.Unmarshal(a.IncludeListIDs, &raw); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAudienceIncludeListIDsUnreadable, err)
		}
		out := make([]string, 0, len(raw))
		seen := make(map[string]struct{}, len(raw))
		for _, id := range raw {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if id := strings.TrimSpace(a.PlatformMasterListID); id != "" {
		return []string{id}, nil
	}
	return nil, nil
}
