// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Conversion association on create (#552). A conversion rule is associated with a campaign by a
// Campaign Conversions record, created with a PUT on its compound key (Conversion Tracking ›
// Campaign Conversions › Create a Campaign Conversion, li-lms-2026-10:
// https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads-reporting/conversion-tracking):
//
//	PUT /rest/campaignConversions/(campaign:urn%3Ali%3AsponsoredCampaign%3A<c>,conversion:urn%3Alla%3AllaPartnerConversion%3A<v>)
//	{"campaign":"urn:li:sponsoredCampaign:<c>","conversion":"urn:lla:llaPartnerConversion:<v>"} → 204
//
// A PUT on the key is idempotent, so a retry, or a campaign reused by name, re-associates safely.

// MaxConversions bounds linkedInConfig.conversions. A local bound, not a documented LinkedIn
// limit: an event campaign tracks a handful of rules.
const MaxConversions = 20

// ErrConversionsInvalid marks a conversions list refused by ValidateConversions.
var ErrConversionsInvalid = errors.New("linkedin-ads: invalid conversions")

// ValidateConversions checks linkedInConfig.conversions and returns its de-duplicated copy.
// Each entry is a conversion rule id: a positive int64 without a leading zero (isPositiveLong,
// the rule ValidateCampaignID uses). The URN form is refused rather than parsed, so there is one
// accepted spelling. nil stays nil; an empty non-nil list is refused (omit the field instead).
func ValidateConversions(ids []string) ([]string, error) {
	if ids == nil {
		return nil, nil
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: the list is empty; omit conversions instead", ErrConversionsInvalid)
	}
	// Bounded before any per-entry work, so a long list costs nothing: room for every distinct id
	// LinkedIn allows plus as many duplicates again.
	if len(ids) > 2*MaxConversions {
		return nil, fmt.Errorf("%w: %d conversions; at most %d", ErrConversionsInvalid, len(ids), MaxConversions)
	}
	out := make([]string, 0, min(len(ids), MaxConversions+1))
	seen := make(map[string]struct{}, min(len(ids), MaxConversions+1))
	for _, id := range ids {
		if !isPositiveLong(id) {
			if strings.HasPrefix(id, "urn:") {
				return nil, fmt.Errorf("%w: %s is a URN; send the conversion rule's numeric id", ErrConversionsInvalid, domain.CallerValue(id))
			}
			return nil, fmt.Errorf("%w: %s is not a conversion rule id (a positive integer without a leading zero)", ErrConversionsInvalid, domain.CallerValue(id))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) > MaxConversions {
			return nil, fmt.Errorf("%w: more than %d distinct conversions; at most %d", ErrConversionsInvalid, MaxConversions, MaxConversions)
		}
	}
	return out, nil
}

func conversionURN(id string) string { return "urn:lla:llaPartnerConversion:" + id }

// associateConversion creates one Campaign Conversions record. Both ids have been validated
// (campaignID by associateConversions' ValidateCampaignID, conversionID by ValidateConversions),
// so the compound key is built from digits only.
func (c *Client) associateConversion(ctx context.Context, campaignID, conversionID string) error {
	campaign := "urn:li:sponsoredCampaign:" + campaignID
	conversion := conversionURN(conversionID)
	path := fmt.Sprintf("campaignConversions/(campaign:%s,conversion:%s)", encodeURNForPath(campaign), encodeURNForPath(conversion))
	body := map[string]any{"campaign": campaign, "conversion": conversion}
	_, err := c.doRequest(ctx, http.MethodPut, path, body, nil, nil, true)
	return err
}

// associateConversions associates every id with the campaign, in order, and reports which
// succeeded and which failed. A failure does not stop the others, and the caller reports a
// partial association as a degraded create rather than discarding a campaign that exists. A
// campaign this run created is PAUSED, so its step says to associate before activating; a
// campaign reused by name keeps its own status, which may be ACTIVE, so its step says it may
// already be serving without the conversion. No upstream text is copied into a step.
func (c *Client) associateConversions(ctx context.Context, campaignID string, ids []string, reused bool) (associated, failed, steps []string) {
	// The campaign id came from LinkedIn's create (or search) response, and createSponsoredCampaign
	// only refuses an empty one. It is checked here before it goes into the compound key, so a
	// malformed upstream id cannot turn part of the key into query text: every id is then
	// reported failed, with no request sent.
	if ValidateCampaignID(campaignID) != nil {
		for _, id := range ids {
			failed = append(failed, id)
			steps = append(steps, fmt.Sprintf("Conversion %s NOT associated: the campaign id LinkedIn returned is not a usable id; associate it in Campaign Manager before activating", id))
		}
		return nil, failed, steps
	}
	for _, id := range ids {
		if err := c.associateConversion(ctx, campaignID, id); err != nil {
			failed = append(failed, id)
			outcome := "FAILED"
			if IsOutcomeUnconfirmed(err) {
				outcome = "UNCONFIRMED"
			}
			advice := "associate it in Campaign Manager before activating"
			if reused {
				advice = "associate it in Campaign Manager; this campaign was reused and may already be serving without it"
			}
			steps = append(steps, fmt.Sprintf("Conversion %s association with campaign %s %s (%s); %s", id, campaignID, outcome, associationFailureClass(err), advice))
			continue
		}
		associated = append(associated, id)
		steps = append(steps, fmt.Sprintf("Conversion %s associated with campaign %s", id, campaignID))
	}
	return associated, failed, steps
}

// associationFailureClass names a failed association without any upstream text: the HTTP
// status when LinkedIn answered, else the transport class.
func associationFailureClass(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return fmt.Sprintf("LinkedIn answered %d", ae.StatusCode)
	}
	if errors.Is(err, ErrCredentialsExpired) {
		return "the connection's credentials were refused"
	}
	return "LinkedIn could not be reached"
}
