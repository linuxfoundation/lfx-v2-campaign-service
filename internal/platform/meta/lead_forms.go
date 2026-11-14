// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// OptimizationLeadGeneration is the ad set optimization goal of an instant-form lead ad: Meta
// delivers to people likely to submit the form (Ad Set `optimization_goal` enum).
const OptimizationLeadGeneration = "LEAD_GENERATION"

// DestinationOnAd is the ad set `destination_type` of an instant-form lead ad: the form opens on
// Facebook or Instagram rather than sending the click to a website.
const DestinationOnAd = "ON_AD"

// leadFormObjectiveParams are the Meta parameters of the leads objective when the caller names an
// existing Instant Form (metaConfig.leadFormId, #550 gap 8). Meta's Lead Ads guide for ODAX
// campaigns: campaign objective OUTCOME_LEADS; ad set optimization_goal LEAD_GENERATION,
// billing_event IMPRESSIONS, destination_type ON_AD and promoted_object {"page_id"}; the creative's
// link_data.call_to_action.value carries lead_gen_form_id. Without a form the leads objective keeps
// its interim website-traffic mapping (objectiveParams["leads"]).
var leadFormObjectiveParams = ObjectiveParams{
	CampaignObjective:  "OUTCOME_LEADS",
	OptimizationGoal:   OptimizationLeadGeneration,
	PromotedObjectType: PromotedObjectPageID,
}

// leadFormCallToActions are the buttons an instant-form lead ad may carry: the subset of
// callToActionTypes Meta's Lead Ads guide lists for lead ads (LEARN_MORE, SIGN_UP, APPLY_NOW,
// SUBSCRIBE, DOWNLOAD). BUY_TICKETS, BOOK_NOW, WATCH_MORE and CONTACT_US send the person somewhere,
// which a form does not, so they are refused before any request rather than at the ad.
var leadFormCallToActions = []string{"LEARN_MORE", "SIGN_UP", "APPLY_NOW", "SUBSCRIBE", "DOWNLOAD"}

// NormalizeLeadForm returns the Instant Form id metaConfig.leadFormId names: "" for a blank value
// (no form: the leads objective keeps its interim traffic mapping), else the trimmed id when the
// objective (as CreateCampaign reads it, empty meaning traffic) is leads and the id is a numeric
// string. A form on any other objective, or a malformed id, is a refusal raised before any request.
func NormalizeLeadForm(objective, formID string) (string, error) {
	id := strings.TrimSpace(formID)
	if id == "" {
		return "", nil
	}
	if obj := defaultObjective(strings.ToLower(strings.TrimSpace(objective))); obj != "leads" {
		return "", creativeOptionInvalid("leadFormId", "applies only to the leads objective (objective is %s); omit it", domain.CallerValue(obj))
	}
	if !numericIDRE.MatchString(id) || len(id) > maxMetaIDDigits {
		return "", creativeOptionInvalid("leadFormId", "%s is not a Meta Instant Form id (a numeric string of at most %d digits)", domain.CallerValue(formID), maxMetaIDDigits)
	}
	return id, nil
}

// maxMetaIDDigits bounds a numeric Graph object id the caller names. Graph ids are 64-bit integers
// (at most 20 decimal digits); a longer string cannot be an id, so it is refused before any request.
const maxMetaIDDigits = 20

// CheckLeadFormCallToAction refuses a call-to-action (already normalized) an instant-form lead ad
// cannot carry. A blank cta is the default, LEARN_MORE, which is allowed.
func CheckLeadFormCallToAction(cta string) error {
	v := strings.ToUpper(strings.TrimSpace(cta))
	if v == "" {
		return nil
	}
	for _, t := range leadFormCallToActions {
		if v == t {
			return nil
		}
	}
	return creativeOptionInvalid("callToAction", "%s is not a button an Instant Form lead ad can carry (one of %s); change callToAction or omit leadFormId", domain.CallerValue(cta), strings.Join(leadFormCallToActions, ", "))
}

// leadFormNode is the read of an Instant Form (Graph Leadgen Form node): its id, the Page that owns
// it and its status (ACTIVE, ARCHIVED, DELETED, DRAFT).
type leadFormNode struct {
	ID   string `json:"id"`
	Page *struct {
		ID string `json:"id"`
	} `json:"page"`
	Status string `json:"status"`
}

// checkLeadForm proves, before the first mutating call, that formID is an Instant Form of the
// connection's Page that can take leads: one GET /{form}?fields=id,page,status. A form of another
// Page, or one that is not ACTIVE, would be refused at the AD create (after the campaign and ad set
// exist), so it is a refusal here. A read that fails, or an answer that does not name the form and
// its Page, is an error. Either way nothing is created.
func (c *Client) checkLeadForm(ctx context.Context, formID string) error {
	if formID == "" {
		return nil
	}
	raw, err := c.getChecked(ctx, "/"+url.PathEscape(formID)+"?fields=id,page,status")
	if err != nil {
		return fmt.Errorf("meta lead form: Instant Form %s could not be read, so nothing was created: %w", domain.CallerValue(formID), err)
	}
	var node leadFormNode
	if err := json.Unmarshal(raw, &node); err != nil || strings.TrimSpace(node.ID) != formID || node.Page == nil || strings.TrimSpace(node.Page.ID) == "" {
		return fmt.Errorf("meta lead form: the read of Instant Form %s did not name the form and its Page, so nothing was created", domain.CallerValue(formID))
	}
	if strings.TrimSpace(node.Page.ID) != strings.TrimSpace(c.account.PageID) {
		return domain.PreflightRefusalf("meta lead form: Instant Form %s belongs to a different Page than this connection's, so its ads could not use it; nothing was created", domain.CallerValue(formID))
	}
	if !strings.EqualFold(strings.TrimSpace(node.Status), "ACTIVE") {
		return domain.PreflightRefusalf("meta lead form: Instant Form %s is not active (status %s); publish it in Meta Ads Manager or pick another form; nothing was created", domain.CallerValue(formID), domain.CallerValue(node.Status))
	}
	return nil
}

// withLeadForm points an object_story_spec's call-to-action at the Instant Form: value becomes
// {"lead_gen_form_id": formID}, replacing the click link (Meta's Lead Ads guide). The link_data
// link itself stays the tracked registration URL.
func withLeadForm(spec map[string]any, formID string) {
	linkData, _ := spec["link_data"].(map[string]any)
	if linkData == nil {
		return
	}
	cta, _ := linkData["call_to_action"].(map[string]any)
	if cta == nil {
		return
	}
	cta["value"] = map[string]any{"lead_gen_form_id": formID}
}
