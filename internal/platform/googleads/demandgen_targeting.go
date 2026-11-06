// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Demand Gen audience and language criteria (#459).
//
// A Demand Gen ad group is AUDIENCE-GROUPED, and that decides the shape of this file.
// Three facts, each verified by a validateOnly probe on 2026-11-03 at v23:
//
//   - Google refuses a user list or a custom audience attached to a Demand Gen ad
//     group directly (CANNOT_ADD_AUDIENCE_SEGMENT_CRITERION_WHEN_AUDIENCE_GROUPED_IS_SET),
//     and setting the ad group's audienceSetting.useAudienceGrouped=false does not
//     change that. The segments live INSIDE a Google Ads Audience resource
//     (customers/<cid>/audiences/<id>), and the ad group references the Audience.
//   - The ad group accepts exactly ONE `audience` criterion; a second is refused
//     (ONE_AUDIENCE_ALLOWED_PER_AD_GROUP).
//   - The ad group accepts `language` criteria alongside the audience, while a
//     CAMPAIGN-level language on Demand Gen is refused.
//
// So CampaignInput.DemandGenAudience is one string, AudienceSegments stays refused on
// this channel with a message pointing at it, and Languages attach here rather than
// through createCampaignTargetingCriteria. This client never creates the Audience that
// DemandGenAudience names; the one Audience it does create holds DemandGenDemographics
// (#498, demandgen_demographics.go).
// ---------------------------------------------------------------------------

// demandGenAudienceRE is the only spelling DemandGenAudience accepts: a full Audience
// resource name. No bare-id shorthand, unlike conversion actions — AudienceSegments,
// the nearest neighbour, takes full resource names only, and the two are read side by
// side. The id half must be a positive, canonical base-10 number (no leading zero),
// which canonicalCampaignID checks after the match.
var demandGenAudienceRE = regexp.MustCompile(`^customers/(\d+)/audiences/(\d+)$`)

// audienceInfo is the `audience` criterion payload — AudienceInfo.audience, the
// resource name of an existing Audience.
type audienceInfo struct {
	Audience string `json:"audience"`
}

// adGroupCriterionDemandGenCreate is the create payload for the Demand Gen ad group's
// audience and language criteria. Exactly one of Audience/Language is set per
// operation.
//
// Its own type rather than a widened adGroupCriterionCreate (targeting.go) or
// adGroupCriterionLocationCreate (geo.go), for the reason geo.go gives for keeping
// those two apart: one type per criterion family, so a payload can never emit an arm
// its channel refuses. A keyword or userList arm here would advertise exactly the
// shapes Google refuses on this ad group. It sends no `status`, like the location
// payload on the same ad group, so Google's default (ENABLED) applies.
type adGroupCriterionDemandGenCreate struct {
	AdGroup  string        `json:"adGroup"`
	Audience *audienceInfo `json:"audience,omitempty"`
	Language *languageInfo `json:"language,omitempty"`
}

// validateDemandGenAudience checks CampaignInput.DemandGenAudience for the given kind,
// returning the trimmed resource name ("" when none was asked for).
//
// Refused on every channel but Demand Gen, rather than ignored: only the Demand Gen
// cascade reads it, so on any other channel it would be validated and then dropped,
// leaving an operator who named an Audience believing the campaign is targeted. The
// fence is `!= campaignKindDemandGen` so the next channel added inherits the refusal.
//
// A resource name under ANOTHER customer is refused here. Google refuses it too
// (INVALID_CUSTOMER_ID, verified by a validateOnly probe on 2026-11-03) — but at the
// criteria mutate, after the budget, the campaign and the ad group exist. This is the
// check validateConversionActions makes for conversion actions, for the same reason.
//
// The connected Google Ads account (same customer id) is the boundary this check enforces.
// That account is shared by every foundation, so create-campaigns also requires
// global_marketing_ops on the project for any audience (service.GoogleAdsMarketingOpsInputs).
// The dispatch adapter keeps the
// field, and every DemandGenAdGroup.Audience (#460), behind a per-deployment opt-in that is
// off by default (GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED, see
// dispatch.googleAdsDemandGenAudienceEnabled).
func validateDemandGenAudience(kind, customerID, audience string) (string, error) {
	return validateDemandGenAudienceField(kind, customerID, "DemandGenAudience", audience)
}

// validateDemandGenAudienceField is validateDemandGenAudience with the field named in its
// errors, so a DemandGenAdGroups entry's refusal names that group's Audience (#460)
// rather than the campaign-level field the caller did not set.
func validateDemandGenAudienceField(kind, customerID, field, audience string) (string, error) {
	if audience == "" {
		return "", nil
	}
	if kind != campaignKindDemandGen {
		return "", fmt.Errorf("google-ads: a Demand Gen audience is not supported on %s (only a Demand Gen ad group takes an Audience criterion here); omit %s, or create a Demand Gen campaign to target an Audience (Search takes user lists through AudienceSegments)", kind, field)
	}
	trimmed := strings.TrimSpace(audience)
	if trimmed == "" {
		return "", fmt.Errorf("google-ads: %s must not be blank; omit it, or name an existing customers/<id>/audiences/<id> Audience", field)
	}
	m := demandGenAudienceRE.FindStringSubmatch(trimmed)
	if m == nil || canonicalCampaignID(m[2]) == "" {
		return "", fmt.Errorf("google-ads: %s %q is not a customers/<id>/audiences/<id> Audience resource name with a positive canonical id (user lists and custom audiences go inside that Audience in the Google Ads UI)", field, capForError(trimmed))
	}
	if m[1] != customerID {
		return "", fmt.Errorf("google-ads: %s %q belongs to customer %q, not the campaign's account %q; Google refuses another customer's Audience on this account's ad group (INVALID_CUSTOMER_ID)", field, capForError(trimmed), m[1], customerID)
	}
	return trimmed, nil
}

// demandGenTargetingStep summarises what createDemandGenAdGroupTargeting attached, for
// the step list, naming only the parts that were asked for.
func demandGenTargetingStep(audience string, languages []languageInfo) string {
	parts := make([]string, 0, 2)
	if audience != "" {
		parts = append(parts, "audience "+audience)
	}
	if n := len(languages); n > 0 {
		parts = append(parts, fmt.Sprintf("%d language(s)", n))
	}
	return strings.Join(parts, "; ")
}

// demandGenTargetingMissing states what a definitely-failed audience/language mutate left
// the ad group without, naming only the halves that were asked for: a languages-only
// create never had an Audience to lose, and saying it did would send an operator looking
// for one.
func demandGenTargetingMissing(audience string, languages []languageInfo) string {
	switch {
	case audience != "" && len(languages) > 0:
		return "it has NO audience or language criteria, so if enabled it would serve without the named Audience and in every language"
	case audience != "":
		return "it has NO audience criterion, so if enabled it would serve without the named Audience"
	default:
		return "it has NO language criteria, so if enabled it would serve in every language"
	}
}

// createDemandGenAdGroupTargeting attaches the Audience and the language criteria to a
// just-created DEMAND GEN ad group as a single adGroupCriteria:mutate, the audience
// first and the languages after it, in the caller's order.
//
// ONE BATCH, so the two share one atomic outcome (partialFailure stays false, as
// everywhere in this client): an ad group whose languages committed and whose audience
// did not would serve to everyone who speaks the language, which is not the campaign
// that was asked for, and the reverse is no better.
//
// It is deliberately NOT folded into createAdGroupGeoTargeting's mutate, for the reason
// createCampaignTargetingCriteria is kept apart from geo on Search: a shared mutate makes
// either set's failure discard the other, and geo's failure sentence ("it has NO
// location criteria and would serve worldwide") would be false for a refused audience.
// Keeping it separate also leaves the geo request byte for byte what it was before #459.
//
// Called AFTER the ad group exists, so a failure is a partial result: the PAUSED
// campaign and its ad group exist either way, and must never be discarded by a
// (nil, err). Returns the audience criterion id (at most one) and the language ids.
func (c *Client) createDemandGenAdGroupTargeting(ctx context.Context, adGroupResource, adGroupID, audience string, languages []languageInfo) (audienceIDs, languageIDs []string, err error) {
	if audience == "" && len(languages) == 0 {
		return nil, nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting aborted before any request (context already done; ad group %s has no audience or language criteria yet): %w", adGroupID, ctxErr)
	}

	ops := make([]mutateOperation, 0, 1+len(languages))
	if audience != "" {
		ops = append(ops, mutateOperation{Create: adGroupCriterionDemandGenCreate{
			AdGroup:  adGroupResource,
			Audience: &audienceInfo{Audience: audience},
		}})
	}
	for i := range languages {
		ops = append(ops, mutateOperation{Create: adGroupCriterionDemandGenCreate{
			AdGroup:  adGroupResource,
			Language: &languages[i],
		}})
	}

	resp, mErr := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if mErr != nil {
		if createOutcomeAmbiguous(mErr) {
			return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting UNCONFIRMED (ad group %s; audience/language criteria may exist — verify in Google Ads before retrying): %w", adGroupID, mErr)
		}
		return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting failed (ad group %s created; %s): %w", adGroupID, demandGenTargetingMissing(audience, languages), mErr)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting UNCONFIRMED (ad group %s; 2xx with a malformed/short mutate response — audience/language criteria may exist — verify in Google Ads before retrying)", adGroupID)
	}

	audienceCount := len(ops) - len(languages)
	seen := newResultIDSet(len(mr.Results))
	for i, r := range mr.Results {
		returnedAdGroupID, critID := c.adGroupCriterionID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting UNCONFIRMED (ad group %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		if returnedAdGroupID != adGroupID {
			return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting UNCONFIRMED (ad group %s; adGroupCriterion resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, r.ResourceName, returnedAdGroupID)
		}
		if dup := seen.repeat(critID, i); dup != "" {
			return nil, nil, fmt.Errorf("google-ads demand gen audience/language targeting "+
				"UNCONFIRMED (ad group %s; %s — verify in Google Ads before retrying)",
				adGroupID, dup)
		}
		if i < audienceCount {
			audienceIDs = append(audienceIDs, critID)
		} else {
			languageIDs = append(languageIDs, critID)
		}
	}
	return audienceIDs, languageIDs, nil
}
