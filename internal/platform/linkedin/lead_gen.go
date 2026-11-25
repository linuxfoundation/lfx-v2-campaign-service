// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Lead Gen Form campaigns (#664, #552 gap 6): a Sponsored Content campaign under objectiveType
// LEAD_GENERATION whose creatives open a Lead Gen Form built in Campaign Manager.
//
// SOURCES. LinkedIn Marketing API, li-lms-2026-11, consulted 2026-11-25:
//
//   - "Create and Manage Campaigns", Lead Generation Campaigns: "A special type of Sponsored
//     Updates campaign is the Lead Generation campaign. To create one, set the objectiveType to
//     LEAD_GENERATION. You cannot set offsiteDeliveryEnabled to True when using Lead Gen." Its
//     objective validation table lists STANDARD_UPDATE and SINGLE_VIDEO (among others) for Lead
//     Generation, and no TEXT_AD.
//   - "Ad Budget Type Allowable Combinations", Objective Type Descriptions: LEAD_GENERATION allows
//     "NONE, ENHANCED_CONVERSION, MAX_LEAD, MAX_QUALIFIED_LEAD"; MAX_LEAD "maximize[s] the
//     campaign's number of leads and exhaust[s] the daily budget without advertiser specifying
//     bid"; "For auto-bidding campaigns ... the costType is always CPM."
//   - "Create and Manage Creatives", Creative schema: leadgenCallToAction is "Required if campaign
//     objective is LEAD_GENERATION", a LeadgenCreativeCallToAction of `destination` (AdFormUrn, "It
//     can only be modified when the creative is in DRAFT status") and `label` (CallToActionLabel,
//     the fourteen LeadGenCallToActionLabels).
//   - "Lead Syncing", Find Forms by Owner: GET /rest/leadForms?q=owner&owner=(sponsoredAccount:
//     <urn>)&start=&count=, each form carrying id, name, owner and state (DRAFT, PUBLISHED,
//     ARCHIVED); GET /rest/leadForms/{id} reads one. rw_ads and r_ads "Can manage and read ad
//     forms."
//
// DECISIONS (#664). Automatic bidding only (MAX_LEAD): a manual bid would be a NONE campaign the
// bid lever and the settings readback treat as Website conversions, so it is refused rather than
// half-supported. The creative's button label defaults to REGISTER (an event's form), and a
// variant's post-level callToAction is refused, because on a lead gen creative the button is the
// form's. NOT live-probed: the adForm URN built from a leadForms id, the MAX_LEAD body and the
// creative's leadgenCallToAction are all from the docs above; the PR records the probe owed.

// The lead generation objective and its automated target.
const (
	ObjectiveLeadGeneration   = "LEAD_GENERATION"
	OptimizationTargetMaxLead = "MAX_LEAD"
)

// Field names of the lead gen inputs, as linkedInConfig spells them.
const (
	FieldLeadGenForm         = "leadGenForm"
	FieldLeadGenCallToAction = "leadGenCallToAction"
)

// DefaultLeadGenCallToAction is the button label sent when the caller names none.
const DefaultLeadGenCallToAction = "REGISTER"

// LeadGenCallToActionLabels are the CallToActionLabel values a creative's leadgenCallToAction
// takes at li-lms-2026-11 ("Create and Manage Creatives", Call to Action Label Types).
var LeadGenCallToActionLabels = []string{
	"APPLY", "DOWNLOAD", "VIEW_QUOTE", "LEARN_MORE", "SIGN_UP", "SUBSCRIBE", "REGISTER",
	"REQUEST_DEMO", "JOIN", "ATTEND", "UNLOCK_FULL_DOCUMENT", "SHOP_NOW", "BUY_NOW", "SEE_MORE",
}

// adFormURNPrefix is the leadgenCallToAction destination's URN type.
const adFormURNPrefix = "urn:li:adForm:"

// ErrLeadGenInvalid marks a lead gen input this client refuses before any request.
var ErrLeadGenInvalid = errors.New("linkedin: lead gen form not accepted")

// LeadGen is a lead gen create's form and button label. FormID is the bare numeric form id.
type LeadGen struct {
	FormID       string
	CallToAction string
}

// FormURN is the creative's leadgenCallToAction destination.
func (l LeadGen) FormURN() string { return adFormURNPrefix + l.FormID }

// NormalizeLeadGenForm returns the bare form id of a leadGenForm given as the id itself or as
// urn:li:adForm:<id>. The id must be a canonical positive 64-bit integer, so nothing else can
// reach a request path or the creative body.
func NormalizeLeadGenForm(raw string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(raw), adFormURNPrefix)
	if !isPositiveLong(id) {
		return "", fmt.Errorf("%w: %s must be a Lead Gen Form id (a positive integer) or %s<id>", ErrLeadGenInvalid, FieldLeadGenForm, adFormURNPrefix)
	}
	return id, nil
}

// ValidateLeadGen checks a create's lead gen inputs against its objective, format, delivery
// options and variants, and returns the normalised copy (nil when the create is not lead gen).
// objective is CampaignInput.Objective; format the normalised format.
func ValidateLeadGen(lg *LeadGen, objective, format string, delivery DeliveryOptions, variants []CreativeVariant) (*LeadGen, error) {
	if lg == nil {
		if objective == ObjectiveLeadGeneration {
			return nil, fmt.Errorf("%w: objective %s requires a Lead Gen Form (%s)", ErrLeadGenInvalid, ObjectiveLeadGeneration, FieldLeadGenForm)
		}
		return nil, nil
	}
	if objective != ObjectiveLeadGeneration {
		return nil, fmt.Errorf("%w: %s is used only with objective %s", ErrLeadGenInvalid, FieldLeadGenForm, ObjectiveLeadGeneration)
	}
	id, err := NormalizeLeadGenForm(lg.FormID)
	if err != nil {
		return nil, err
	}
	label := lg.CallToAction
	if label == "" {
		label = DefaultLeadGenCallToAction
	}
	if !slices.Contains(LeadGenCallToActionLabels, label) {
		return nil, fmt.Errorf("%w: %s must be one of %s", ErrLeadGenInvalid, FieldLeadGenCallToAction, strings.Join(LeadGenCallToActionLabels, ", "))
	}
	if format == FormatText {
		return nil, fmt.Errorf("%w: a lead generation campaign is Sponsored Content; format %q is not available", ErrLeadGenInvalid, FormatText)
	}
	// A lead gen carousel opens one form from every card; that creative shape is not built (#665).
	if format == FormatCarousel {
		return nil, fmt.Errorf("%w: format %q is not available with objective %s yet", ErrLeadGenInvalid, FormatCarousel, ObjectiveLeadGeneration)
	}
	if delivery.OffsiteDeliveryEnabled != nil && *delivery.OffsiteDeliveryEnabled {
		return nil, fmt.Errorf("%w: %s true is not available with objective %s", ErrLeadGenInvalid, FieldOffsiteDeliveryEnabled, ObjectiveLeadGeneration)
	}
	for i, v := range variants {
		if v.CallToAction != "" {
			return nil, fmt.Errorf("%w: variants[%d].callToAction is not used on a lead generation create; the form's button is %s", ErrLeadGenInvalid, i, FieldLeadGenCallToAction)
		}
	}
	return &LeadGen{FormID: id, CallToAction: label}, nil
}

// leadgenCallToAction is the creative body's leadgenCallToAction for lg.
func (l LeadGen) leadgenCallToAction() map[string]any {
	return map[string]any{"destination": l.FormURN(), "label": l.CallToAction}
}

// leadFormBody is the part of a Lead Gen Form this client reads.
type leadFormBody struct {
	ID    flexibleID `json:"id"`
	Name  string     `json:"name"`
	State string     `json:"state"`
	Owner *struct {
		SponsoredAccount string `json:"sponsoredAccount"`
	} `json:"owner"`
}

// leadFormStateArchived is the one form state a create and the picker refuse: an archived form
// and its creatives do not serve ("Lead Syncing", Archive a Form).
const leadFormStateArchived = "ARCHIVED"

// checkLeadForm reads the form before any POST and refuses one that is not the ad account's own
// or is archived, so a wrong id fails with nothing created instead of on the first creative
// after the campaign group and campaign exist. A 404 reads as "not this account's form"; any
// other failure, a 403 included (it can equally be a token or role that cannot read forms), is
// returned as is (nothing was created either way).
func (c *Client) checkLeadForm(ctx context.Context, accountID string, lg LeadGen) error {
	raw, err := c.doEntityGET(ctx, "leadForms/"+lg.FormID)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: Lead Gen Form %s was not found on ad account %s", ErrLeadGenInvalid, lg.FormID, accountID)
		}
		return fmt.Errorf("read linkedin lead gen form %s: %w", lg.FormID, err)
	}
	var form leadFormBody
	if json.Unmarshal(raw, &form) != nil {
		return fmt.Errorf("read linkedin lead gen form %s: the response is not a form object", lg.FormID)
	}
	if form.Owner == nil || form.Owner.SponsoredAccount != accountURN(accountID) {
		return fmt.Errorf("%w: Lead Gen Form %s does not belong to ad account %s", ErrLeadGenInvalid, lg.FormID, accountID)
	}
	if form.State == leadFormStateArchived {
		return fmt.Errorf("%w: Lead Gen Form %s is archived", ErrLeadGenInvalid, lg.FormID)
	}
	return nil
}

// LeadForm is one Lead Gen Form on the ad account, as the picker lists it.
type LeadForm struct {
	ID    string
	Name  string
	State string
}

// URN is the creative's leadgenCallToAction destination for the form.
func (f LeadForm) URN() string { return adFormURNPrefix + f.ID }

// ListLeadForms lists the client's ad account's Lead Gen Forms that are not archived, sorted by
// name (ignoring case), then id. A form owned by anything but this account fails the read, as
// offsetWalk fails one of another account.
func (c *Client) ListLeadForms(ctx context.Context) ([]LeadForm, error) {
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return nil, err
	}
	owner := "(sponsoredAccount:" + encodeURNForPath(accountURN(accountID)) + ")"
	out := []LeadForm{}
	foreign := false
	err = c.offsetWalk(ctx, "leadForms", "lead gen forms", func(start int) map[string]string {
		return map[string]string{"q": "owner", "owner": owner, "start": strconv.Itoa(start), "count": strconv.Itoa(lookupPageSize)}
	}, accountID, func(el responseElement, id string) {
		if el.Owner != nil && el.Owner.SponsoredAccount != accountURN(accountID) {
			foreign = true
		}
		if el.State == leadFormStateArchived {
			return
		}
		out = append(out, LeadForm{ID: id, Name: strings.TrimSpace(el.Name), State: el.State})
	})
	if err != nil {
		return nil, err
	}
	if foreign {
		return nil, fmt.Errorf("list linkedin lead gen forms: an element belongs to another owner")
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
