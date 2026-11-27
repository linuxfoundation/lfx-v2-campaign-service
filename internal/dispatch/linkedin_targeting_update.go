// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// linkedinTargetingUpdateEnabled reports whether this deployment opted in to the targeting update
// lever (#666). Read per call so it flips without a rebuild. Off by default: no targetingCriteria
// PARTIAL_UPDATE has been sent to a live LinkedIn account.
func linkedinTargetingUpdateEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsTargetingUpdateEnabled) == "true"
}

// rejectedTargetingError marks a targeting update refused because of the REQUEST (the gate, the
// targeting rules, or LinkedIn's definite refusal), with the platform unchanged. err wraps
// domain.ErrTargetingRejected; reason is a service-authored sentence, never upstream text.
type rejectedTargetingError struct {
	reason string
	err    error
}

func (e *rejectedTargetingError) Error() string { return e.err.Error() }
func (e *rejectedTargetingError) Unwrap() error { return e.err }

// TargetingRejectedReason returns the client-safe explanation of the refusal.
func (e *rejectedTargetingError) TargetingRejectedReason() string { return e.reason }

func targetingRejected(reason string, cause error) error {
	return &rejectedTargetingError{reason: reason, err: fmt.Errorf("write linkedin campaign targeting: %w: %w", cause, domain.ErrTargetingRejected)}
}

// unconfirmedTargetingWriteError marks a targeting write whose outcome is unknowable: the
// PARTIAL_UPDATE was sent and may have applied. The service answers 503 "verify before retrying".
type unconfirmedTargetingWriteError struct{ err error }

func (e *unconfirmedTargetingWriteError) Error() string {
	return "targeting write outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *unconfirmedTargetingWriteError) Unwrap() error     { return e.err }
func (e *unconfirmedTargetingWriteError) Unconfirmed() bool { return true }

// errLinkedInTargetingUpdateDisabled is the 400 sentence while the gate is off.
var errLinkedInTargetingUpdateDisabled = fmt.Sprintf("LinkedIn targeting updates are disabled on this deployment (%s is not \"true\"); ask an operator to set it, or change the targeting in LinkedIn Campaign Manager",
	constants.EnvLinkedInAdsTargetingUpdateEnabled)

// WriteTargeting implements service.TargetingWriter for LinkedIn (#666, #552 gap 10): it replaces
// the campaign's targetingCriteria with change, built by the same builder and refusal rules as
// linkedInConfig.targeting at create (#557), after the same identity guards as the bid lever.
//
// THE ROW SUPPLIES WHAT THE REQUEST DOES NOT. The update merges the employerExclusions the create
// recorded in the row's service-owned result blob (the staff exclusions the self-serve BFF fills
// from its ConfigMap), never the caller-replaceable config snapshot, so an update cannot silently
// drop them. A row that does not record them is refused (409) rather than updated without them.
//
// ADOPTED ROWS ARE REFUSED, as by the bid lever: adoption verified identity, not the locale or the
// staff exclusions this update would assume.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, and the one mutation is sent ONCE. On success
// campaign.ConfigSnapshot records the targeting and locations sent (the service persists it).
func (d *LinkedInDispatcher) WriteTargeting(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, change model.TargetingChange) error {
	if !linkedinTargetingUpdateEnabled() {
		return targetingRejected(errLinkedInTargetingUpdateDisabled, errors.New("deployment gate off"))
	}
	created := linkedInCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	if linkedInAdopted(campaign) {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s was ADOPTED, and adoption verifies the campaign's identity, not the locale or staff exclusions an update would carry; change the targeting in LinkedIn Campaign Manager: %w",
			campaign.PlatformCampaignID, domain.ErrTargetingUnwritable)
	}
	// VERBATIM, never trimmed (see WriteBid).
	campaignID := campaign.PlatformCampaignID
	if err := linkedin.ValidateCampaignID(campaignID); err != nil {
		return fmt.Errorf("write linkedin campaign targeting: the campaign row's platform campaign id cannot address a linkedin request: %w: %w", err, domain.ErrTargetingUnwritable)
	}
	exclusions, err := linkedinRecordedEmployerExclusions(campaign)
	if err != nil {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s: %w: %w", campaignID, err, domain.ErrTargetingUnwritable)
	}
	snap := linkedinTargetingSnapshot(campaign.ConfigSnapshot)

	// THE REQUEST — a pure function of it and the row, refused before any call (400).
	update, err := linkedin.PrepareTargetingUpdate(&linkedin.Targeting{Include: change.Include, Exclude: change.Exclude}, change.GeoTargets, exclusions)
	if err != nil {
		reason, _ := linkedin.TargetingUpdateReason(err)
		return targetingRejected("targeting: "+reason, err)
	}

	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(res.accountID)
	if !linkedin.ValidAccountID(accountID) {
		return linkedinAccountIDUnusable(res, projectID, "write linkedin campaign targeting")
	}
	if err := verifyLinkedInAccountMatch("write linkedin campaign targeting", campaign, accountID); err != nil {
		return err
	}
	client := d.cachedLinkedInClient(projectID, platform, res, creds, accountID)

	current, err := client.GetCampaignSettings(ctx, campaignID)
	if err != nil {
		rerr := fmt.Errorf("write linkedin campaign targeting: read campaign: %w", err)
		if linkedinConnectionDefect(err) {
			return res.systemScoped(linkedinExpiry(rerr))
		}
		return rerr
	}
	if current == nil {
		return fmt.Errorf("%w: linkedin campaign %s", domain.ErrPlatformCampaignAbsent, campaignID)
	}
	if current.AccountID == "" {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s did not report its ad account, so it cannot be established that it is the connection's: %w",
			campaignID, domain.ErrTargetingUnwritable)
	}
	if current.AccountID != accountID {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s is reported under ad account %s, not the connection's account %s: %w",
			campaignID, current.AccountID, accountID, domain.ErrCampaignAccountMismatch)
	}
	if current.Status == nil {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s did not report its status, so it cannot be established that it can still serve: %w",
			campaignID, domain.ErrTargetingUnwritable)
	}
	if current.Terminal() {
		return fmt.Errorf("write linkedin campaign targeting: campaign %s is %s upstream and can never serve again: %w",
			campaignID, *current.Status, domain.ErrTargetingUnwritable)
	}

	if err := client.UpdateCampaignTargeting(ctx, campaignID, update); err != nil {
		werr := fmt.Errorf("write linkedin campaign targeting for campaign %s: %w", campaignID, err)
		if linkedin.IsOutcomeUnconfirmed(err) {
			return &unconfirmedTargetingWriteError{err: werr}
		}
		if reason, ok := linkedin.TargetingUpdateReason(err); ok {
			return targetingRejected(reason, werr)
		}
		if linkedinConnectionDefect(err) {
			return res.systemScoped(linkedinExpiry(werr))
		}
		return werr
	}

	// Confirmed: record what was sent. A snapshot re-marshal failure cannot happen for the
	// decoded map; if it did, the row keeps its old snapshot and the platform is the truth.
	if raw, ok := snap.withTargeting(update); ok {
		campaign.ConfigSnapshot = raw
	}
	return nil
}

// linkedinRecordedEmployerExclusions returns the employer exclusions the create recorded in the
// row's service-owned result blob (CampaignResult.EmployerExclusions). They are read from there,
// never from config_snapshot, which a caller's campaign update (PUT .../campaigns/{id}) can
// replace: a snapshot edited to {} would otherwise let the next update drop the staff exclusions
// silently. A row that does not record them (created before #666, or a result that does not
// decode) is an error, so the update is refused rather than sent without them.
func linkedinRecordedEmployerExclusions(campaign *model.Campaign) ([]string, error) {
	var blob struct {
		EmployerExclusions *[]string `json:"employerExclusions"`
	}
	if len(campaign.Result) == 0 || json.Unmarshal(campaign.Result, &blob) != nil || blob.EmployerExclusions == nil {
		return nil, errors.New("the campaign row does not record the employer exclusions it was created with (rows created before #666 do not), so an update cannot carry them; change the targeting in LinkedIn Campaign Manager")
	}
	return *blob.EmployerExclusions, nil
}

// linkedinRowSnapshot is the decoded config_snapshot the update rewrites. Only its geo labels are
// read, for display; nothing the write depends on comes from it.
type linkedinRowSnapshot struct {
	GeoTargets []linkedin.GeoTarget
	fields     map[string]json.RawMessage
}

// linkedinTargetingSnapshot decodes a LinkedIn row's config_snapshot for the rewrite. A missing or
// non-object snapshot (a caller may have replaced it) is treated as empty: the rewrite then records
// just the targeting and locations sent.
func linkedinTargetingSnapshot(raw json.RawMessage) *linkedinRowSnapshot {
	out := &linkedinRowSnapshot{fields: map[string]json.RawMessage{}}
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return out
	}
	out.fields = fields
	var typed struct {
		GeoTargets []linkedin.GeoTarget `json:"geoTargets"`
	}
	if json.Unmarshal(raw, &typed) == nil {
		out.GeoTargets = typed.GeoTargets
	}
	return out
}

// withTargeting returns the snapshot with targeting, geoTargets and employerExclusions replaced by
// what the update sent, and the profile keys a custom targeting leaves unread removed. A location the snapshot
// already named keeps its label; a new one has none. Every other key is kept.
func (s *linkedinRowSnapshot) withTargeting(u *linkedin.TargetingUpdate) (json.RawMessage, bool) {
	labels := make(map[string]string, len(s.GeoTargets))
	for _, g := range s.GeoTargets {
		labels[g.URN] = g.Label
	}
	geo := make([]linkedin.GeoTarget, 0, len(u.GeoURNs))
	for _, urn := range u.GeoURNs {
		geo = append(geo, linkedin.GeoTarget{URN: urn, Label: labels[urn]})
	}
	out := make(map[string]json.RawMessage, len(s.fields)+2)
	for k, v := range s.fields {
		out[k] = v
	}
	t, err := json.Marshal(u.Targeting)
	if err != nil {
		return nil, false
	}
	g, err := json.Marshal(geo)
	if err != nil {
		return nil, false
	}
	// The trusted create-time exclusions actually merged, not whatever a caller's campaign update
	// left in the snapshot (missing or empty), so the snapshot describes what LinkedIn serves.
	e, err := json.Marshal(append([]string{}, u.EmployerExclusions...))
	if err != nil {
		return nil, false
	}
	out["targeting"] = t
	out["geoTargets"] = g
	out["employerExclusions"] = e
	// A custom targeting replaces the profile clause and job functions, which create refuses beside
	// it; drop them so the snapshot stays a config the validator accepts and names only what serves.
	for _, k := range []string{"targetingProfile", "targetingProfiles", "jobFunctions"} {
		delete(out, k)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return raw, true
}
