// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Targeting update after create (#666, #552 gap 10)
//
// THE WRITE is a Rest.li PARTIAL_UPDATE of the campaign's `targetingCriteria`, sent ONCE
// (withSendOnce), like the bid and budget writes. Source: LinkedIn Marketing API, "Create and
// Manage Campaigns" (li-lms-2026-10), "Update a Campaign": POST
// adAccounts/{adAccountId}/adCampaigns/{id} with X-Restli-Method PARTIAL_UPDATE and a
// {"patch": {"$set": {...}}} body. A $set REPLACES the field it names, so the body carries the
// WHOLE criteria: the geo clause first, then the caller's include clauses, then exclude.or with
// the row's employer exclusions merged in — exactly what customTargetingCriteria renders for a
// create with linkedInConfig.targeting (#557). Nothing else is sent: never status, budget,
// bidding or the delivery options.
//
// interfaceLocales. The #552 gap review recorded that an update must name interfaceLocales. The
// create path never sends that facet: it sends the campaign `locale` en_US (client.go), from
// which LinkedIn derives it. So when no include clause names interfaceLocales, the update adds
// one clause {interfaceLocales: [UpdateLocale]}, the locale every create sends, which keeps the
// audience the create had rather than widening it to every interface language. A caller that
// names interfaceLocales in any clause gets exactly its own. A clause is added only below
// MaxTargetingClauses; a full include with no locale is refused with the remedy.
//
// NOT PROBED LIVE. The dispatcher offers it only behind LINKEDIN_ADS_TARGETING_UPDATE_ENABLED;
// this package does not read the flag.
//
// REFUSAL. The body names targetingCriteria and nothing else, so a definite 400 or 422 on the
// write is LinkedIn refusing the targeting: ErrTargetingUpdateRefused, with this package's own
// sentence. LinkedIn's text is never surfaced. A transport failure, 3xx, 429, 5xx or mutating 401
// may have applied, and is left for IsOutcomeUnconfirmed.
// ---------------------------------------------------------------------------

// UpdateLocale is the interfaceLocales value an update adds when the caller names none: the
// campaign locale every create sends ({country US, language en}).
const UpdateLocale = "urn:li:locale:en_US"

// FacetInterfaceLocales is the targeting facet the update must carry.
const FacetInterfaceLocales = "interfaceLocales"

// maxUpdateGeoTargets bounds an update's locations, LinkedIn's per-facet cap.
const maxUpdateGeoTargets = maxFacetValues

// ErrTargetingUpdateRefused marks a definite LinkedIn refusal of a targeting update; nothing was
// changed. TargetingUpdateReason returns the sentence a caller may see.
var ErrTargetingUpdateRefused = errors.New("linkedin: LinkedIn refused the targeting update")

// targetingRefusedSentence is the one sentence this package hands back for a LinkedIn refusal.
const targetingRefusedSentence = "LinkedIn refused this targeting for the campaign; check the facet values and the combinations LinkedIn accepts, then retry"

// TargetingUpdate is a validated targeting update, ready for UpdateCampaignTargeting.
type TargetingUpdate struct {
	// Targeting is the normalised targeting, with the interfaceLocales clause when one was added.
	Targeting *Targeting
	// GeoURNs are the de-duplicated location URNs, in request order.
	GeoURNs []string
	// EmployerExclusions are the row's configured exclusions, merged into exclude.employers.
	EmployerExclusions []string
	// LocaleAdded reports whether the update added the UpdateLocale clause.
	LocaleAdded bool
}

// PrepareTargetingUpdate validates an update and returns it ready to send. It makes no request,
// so the dispatcher refuses a bad update (400) before resolving credentials, and
// UpdateCampaignTargeting runs it again. Every refusal wraps ErrTargetingInvalid and carries
// caller values only through domain.CallerValue.
//
// The targeting meets ValidateTargeting's rules with employerExclusions merged, as a create's
// does. geoURNs must hold 1 to 100 distinct urn:li:geo:<id> values; duplicates are dropped.
// Running it on its own output changes nothing: the added locale clause satisfies the check.
func PrepareTargetingUpdate(t *Targeting, geoURNs, employerExclusions []string) (*TargetingUpdate, error) {
	if t == nil {
		return nil, targetingInvalid("targeting is required")
	}
	geo, err := cleanUpdateGeo(geoURNs)
	if err != nil {
		return nil, err
	}
	clean, err := ValidateTargeting(t, employerExclusions)
	if err != nil {
		return nil, err
	}
	out := &TargetingUpdate{Targeting: clean, GeoURNs: geo, EmployerExclusions: employerExclusions}
	if !namesFacet(clean, FacetInterfaceLocales) {
		if len(clean.Include) >= MaxTargetingClauses {
			return nil, targetingInvalid("include holds %d clauses and names no %s; an update must carry one, so add %s to a clause or use at most %d clauses and the update adds %s",
				len(clean.Include), FacetInterfaceLocales, FacetInterfaceLocales, MaxTargetingClauses-1, UpdateLocale)
		}
		clean.Include = append(clean.Include, map[string][]string{FacetInterfaceLocales: {UpdateLocale}})
		out.LocaleAdded = true
	}
	return out, nil
}

// namesFacet reports whether any include clause names key.
func namesFacet(t *Targeting, key string) bool {
	for _, clause := range t.Include {
		if len(clause[key]) > 0 {
			return true
		}
	}
	return false
}

// cleanUpdateGeo checks an update's locations: at least one, each exactly urn:li:geo:<id> (no
// surrounding space, no leading zero), at most maxUpdateGeoTargets distinct values.
func cleanUpdateGeo(geoURNs []string) ([]string, error) {
	if len(geoURNs) == 0 {
		return nil, targetingInvalid("geo_targets needs at least one urn:li:geo:<id>; a campaign without locations cannot serve")
	}
	out := make([]string, 0, min(len(geoURNs), maxUpdateGeoTargets+1))
	seen := make(map[string]struct{}, len(out))
	for _, g := range geoURNs {
		if !updateGeoURNRE.MatchString(g) {
			return nil, targetingInvalid("geo_targets value %s is not urn:li:geo:<id>", quoteValue(g))
		}
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
		if len(out) > maxUpdateGeoTargets {
			return nil, targetingInvalid("geo_targets holds more than %d distinct locations; LinkedIn allows at most %d", maxUpdateGeoTargets, maxUpdateGeoTargets)
		}
	}
	return out, nil
}

// updateGeoURNRE is a location URN with a canonical numeric id. Stricter than geoURNRE (which the
// create path keeps for compatibility): no leading zero, at most a long's 19 digits.
var updateGeoURNRE = regexp.MustCompile(`^urn:li:geo:` + numericIDTail + `$`)

// UpdateCampaignTargeting replaces the campaign's targetingCriteria via PARTIAL_UPDATE. The
// caller is responsible for having established that the campaign is the connection's and can
// still serve; this function writes what it is told, after re-validating u (it is exported, so
// "every caller prepared first" would otherwise be a convention).
//
// Errors are returned for the caller to classify: IsOutcomeUnconfirmed for a write that may have
// applied, ErrTargetingUpdateRefused for LinkedIn's definite refusal, ErrTargetingInvalid for an
// update refused before sending. A PARTIAL_UPDATE returns no body, so the 2xx IS the confirmation.
func (c *Client) UpdateCampaignTargeting(ctx context.Context, campaignID string, u *TargetingUpdate) error {
	if err := c.validateCredentialShape(); err != nil {
		return err
	}
	if err := ValidateCampaignID(campaignID); err != nil {
		return err
	}
	if u == nil {
		return targetingInvalid("targeting update is required")
	}
	prepared, err := PrepareTargetingUpdate(u.Targeting, u.GeoURNs, u.EmployerExclusions)
	if err != nil {
		return err
	}
	criteria, err := customTargetingCriteria(prepared.Targeting, prepared.GeoURNs, prepared.EmployerExclusions)
	if err != nil {
		return err
	}
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return fmt.Errorf("linkedin: %w", err)
	}
	if !ValidAccountID(accountID) {
		return fmt.Errorf("linkedin: ad account id %q is not a positive 64-bit integer", accountID)
	}
	path := fmt.Sprintf("adAccounts/%s/adCampaigns/%s", accountID, campaignID)
	body := map[string]any{"patch": map[string]any{"$set": criteria}}
	headers := map[string]string{"X-Restli-Method": "PARTIAL_UPDATE"}
	if _, err := c.doRequest(withSendOnce(ctx), http.MethodPost, path, body, nil, headers, true); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && isMutatingMethod(ae.Method) &&
			(ae.StatusCode == http.StatusBadRequest || ae.StatusCode == http.StatusUnprocessableEntity) {
			return fmt.Errorf("linkedin: update campaign %s targetingCriteria: %w (HTTP %d)", campaignID, ErrTargetingUpdateRefused, ae.StatusCode)
		}
		return fmt.Errorf("linkedin: update campaign %s targetingCriteria: %w", campaignID, err)
	}
	return nil
}

// TargetingUpdateReason reports whether err is a targeting refusal from this package — a rule
// PrepareTargetingUpdate applied, or LinkedIn's definite refusal — and returns the client-safe
// sentence. Every other error is not one, and its text must not reach a caller.
func TargetingUpdateReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrTargetingUpdateRefused):
		return targetingRefusedSentence, true
	case errors.Is(err, ErrTargetingInvalid):
		// targetingInvalid renders "<ErrTargetingInvalid>: <service sentence>"; return the
		// sentence alone. Caller values in it went through domain.CallerValue.
		msg := err.Error()
		if i := strings.Index(msg, ErrTargetingInvalid.Error()+": "); i >= 0 {
			return msg[i+len(ErrTargetingInvalid.Error())+2:], true
		}
		return msg, true
	}
	return "", false
}
