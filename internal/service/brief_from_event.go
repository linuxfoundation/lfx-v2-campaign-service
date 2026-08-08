// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
)

// BriefFromEventDetails maps parsed event details into a CampaignBrief,
// ready for persistence via CreateBrief.
//
// The parsed event details become the brief's EventDetails blob, which is
// later read by dispatchers (e.g., internal/dispatch/reddit.go's briefFields).
// Only fields extracted with high confidence from the event page are populated;
// fields requiring human judgment (copy, keywords, targeting) are left empty
// so a human can author them.
//
// EventName is required: the parser itself checks for it and the dispatcher
// (internal/dispatch/reddit.go's briefFields) rejects a brief without it.
// A missing EventName is a client error — the event page had no usable title.
//
// projectID must be a canonical LFX project slug (e.g., "cncf", "tlf").
// eventSlug is a URL-safe identifier derived by the caller from the event URL.
// Both are validated by the caller's CreateBrief handler; this function does not
// re-validate them.
func BriefFromEventDetails(projectID, eventSlug string, details eventurl.EventDetails, programType model.ProgramType) (*model.CampaignBrief, error) {
	// EventName is required: the parser checks for it, and the dispatcher will
	// reject a brief without it. Returning a clear error here prevents a brief
	// that will fail dispatch two steps later.
	if details.Name == "" {
		return nil, fmt.Errorf("event details missing required eventName")
	}

	// Marshal EventDetails into JSON for storage in the brief's EventDetails blob.
	// The blob is opaque to the brief service but read by dispatchers; callers
	// that need individual fields (like EventName) must decode it themselves.
	eventDetailsJSON, err := json.Marshal(details)
	if err != nil {
		// json.Marshal only fails on cyclic structures or types that cannot be
		// marshaled; EventDetails has neither, so this is a defensive guard
		// that should never fire in practice.
		return nil, fmt.Errorf("failed to marshal event details: %w", err)
	}

	// Build the brief. Fields that require human judgment are left empty:
	// - Copy: ad copy needs to match the target audience and campaign goal
	// - Keywords: keyword strategy depends on the marketing objective
	// - Targeting: targeting details depend on the campaign scope
	//
	// Platforms is left empty too: it is a "planning hint" (design/brief.go),
	// not a binding selection — the creator decides which platforms to use when
	// creating campaigns.
	brief := &model.CampaignBrief{
		ProjectID:    projectID,
		ProgramType:  programType,
		EventSlug:    eventSlug,
		URL:          details.URL, // The event page URL; dispatchers use it as RegistrationURL
		EventDetails: eventDetailsJSON,
		// Copy, Keywords, Targeting are left nil for human authoring.
		// Platforms is left nil too — it's a planning hint, not a binding selection.
		Status: model.BriefDraft,
		// Version and CreatedAt/UpdatedAt are set by the repository.
	}

	return brief, nil
}
