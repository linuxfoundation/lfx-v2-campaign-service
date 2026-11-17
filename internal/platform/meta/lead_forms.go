// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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

// Instant Form picker read (#550, gap 8): the forms of the connection's Page, so the UI can offer
// metaConfig.leadFormId values instead of the operator pasting an id from Ads Manager.

// LeadForm is one Instant Form of the Page: its id (what metaConfig.leadFormId takes), its name, and
// Meta's status verbatim (ACTIVE, ARCHIVED, DELETED, DRAFT, …). Only an ACTIVE form passes the
// create's own check (checkLeadForm).
type LeadForm struct {
	ID     string
	Name   string
	Status string
}

// Bounds of the lead form walk: 100 per page, at most 10 pages. A Page with more forms fails the read
// (ErrLeadFormListTooLarge) rather than silently truncating it, as the custom-audience walk does.
const (
	leadFormListPageSize = 100
	leadFormListMaxPages = 10
	maxLeadFormNameBytes = 1024
)

// ErrLeadFormListTooLarge reports a Page with more Instant Forms than the read walks.
var ErrLeadFormListTooLarge = errors.New("meta: the Page has more instant forms than the read lists")

// ListLeadForms lists every Instant Form of pageID, all or nothing:
// `GET /{page}/leadgen_forms?fields=id,name,status&limit=100`, following the opaque `after` cursor
// (never Meta's token-bearing paging.next URL) for at most leadFormListMaxPages pages. Every page
// passes identityjson on its raw bytes; every row must carry a numeric id and a status token and
// appear once. A missing data field, a next page without a cursor, a repeated cursor, or more forms
// than the bound fails the read. Errors never echo an upstream value.
//
// NOT live-verified: the Page reference documents only POST on this edge; reading it is the Lead
// Ads retrieval guide's call, and may need a Page access token with pages_manage_ads and
// leads_retrieval rather than the connection's token.
func (c *Client) ListLeadForms(ctx context.Context, pageID string) ([]LeadForm, error) {
	pageID = strings.TrimSpace(pageID)
	if !numericIDRE.MatchString(pageID) || len(pageID) > maxMetaIDDigits {
		return nil, errors.New("list meta lead forms: the connection's page_id is not a numeric Page id")
	}
	out := []LeadForm{}
	ids := map[string]struct{}{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < leadFormListMaxPages; page++ {
		path := "/" + pageID + "/leadgen_forms?fields=id,name,status&limit=" + strconv.Itoa(leadFormListPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		raw, err := c.getChecked(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("list meta lead forms: %w", err)
		}
		p, err := decodePage(raw, path)
		if err != nil {
			return nil, fmt.Errorf("list meta lead forms: %w", err)
		}
		for i, rowRaw := range *p.Data {
			f, rerr := decodeLeadForm(rowRaw)
			if rerr != nil {
				return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: %w", page, i, rerr)}
			}
			if _, dup := ids[f.ID]; dup {
				return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: the same form is listed twice", page, i)}
			}
			if len(out) == leadFormListPageSize*leadFormListMaxPages {
				return nil, fmt.Errorf("list meta lead forms: more than %d forms: %w", leadFormListPageSize*leadFormListMaxPages, ErrLeadFormListTooLarge)
			}
			ids[f.ID] = struct{}{}
			out = append(out, f)
		}
		next, done, cerr := nextCursor(p, cursors)
		if cerr != nil {
			return nil, fmt.Errorf("list meta lead forms: %w", cerr)
		}
		if done {
			return out, nil
		}
		after = next
	}
	return nil, fmt.Errorf("list meta lead forms: more than %d pages of %d: %w", leadFormListMaxPages, leadFormListPageSize, ErrLeadFormListTooLarge)
}

// decodeLeadForm validates one listed form. Errors never echo an upstream value.
func decodeLeadForm(raw json.RawMessage) (LeadForm, error) {
	var w struct {
		ID     *graphID `json:"id"`
		Name   *string  `json:"name"`
		Status *string  `json:"status"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return LeadForm{}, errors.New("row is not an instant form object of the expected shape")
	}
	if w.ID == nil || !numericIDRE.MatchString(string(*w.ID)) {
		return LeadForm{}, errors.New("row has no canonical form id")
	}
	if w.Status == nil || !statusTokenRE.MatchString(*w.Status) {
		return LeadForm{}, errors.New("row has no form status token")
	}
	f := LeadForm{ID: string(*w.ID), Status: *w.Status}
	if w.Name != nil {
		f.Name = *w.Name
	}
	if len(f.Name) > maxLeadFormNameBytes {
		return LeadForm{}, fmt.Errorf("name exceeds %d bytes", maxLeadFormNameBytes)
	}
	return f, nil
}
