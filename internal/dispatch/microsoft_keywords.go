// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// unconfirmedKeywordLeverError carries a Microsoft keyword-lever outcome that MAY have been
// applied to the service's verify-before-retry arm, which detects it by the Unconfirmed()
// method rather than by a sentinel crossing the package boundary.
type unconfirmedKeywordLeverError struct{ err error }

func (e *unconfirmedKeywordLeverError) Error() string {
	return "keyword change outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *unconfirmedKeywordLeverError) Unwrap() error     { return e.err }
func (e *unconfirmedKeywordLeverError) Unconfirmed() bool { return true }

// microsoftKeywordLeverClient runs the guards both keyword levers share, in the order every
// Microsoft mutation here uses, and returns the client to act with.
//
// PROVENANCE FAILS CLOSED, as WriteBudget's does and as the google-ads keyword actions do: a
// row that records no creating account cannot prove its ad-group or keyword ids name THIS
// campaign's entities (Microsoft ids are unique only within an account), and REMOVE cannot be
// undone. verifyMicrosoftAccountMatch's "unknown, proceed" is right for a read and wrong here.
// The absence is refused BEFORE credentials are decrypted; the mismatch after, through the
// shared helper so the wording stays common.
func (d *MicrosoftDispatcher) microsoftKeywordLeverClient(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, op string) (*microsoft.Client, error) {
	if microsoftCreationAccountID(campaign) == "" {
		return nil, fmt.Errorf("%s: campaign %s does not record which ad account it was created under, so its ids cannot be resolved safely: %w",
			op, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	client, err := d.resolveMicrosoftClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	if err := verifyMicrosoftAccountMatch(op, campaign, client); err != nil {
		return nil, err
	}
	return client, nil
}

// ApplyKeywordActions implements service.KeywordActioner for Microsoft Advertising: PAUSE via
// UpdateKeywords, REMOVE via DeleteKeywords.
//
// The CONTRACT is google-ads' (internal/dispatch/googleads.go ApplyKeywordActions) wherever
// Microsoft allows it to be, and every guard runs in the same order:
//
//  1. The batch is validated locally — a permanent input fault masks any contingent one.
//  2. The campaign must be provisioned: a platform campaign id and an ad group.
//  3. Every action must name THIS campaign's ad group, checked against the row before
//     Microsoft is contacted. A Microsoft campaign built here has exactly one ad group.
//  4. Provenance fails closed, then the account must match (microsoftKeywordLeverClient).
//  5. Every keyword id must be a live, non-deleted keyword IN that ad group — a READ
//     (GetKeywordsByAdGroupId), so a refusal here has changed nothing. Without it a keyword id
//     from any ad group in the account could be paused or deleted through a campaign the
//     caller owns; Microsoft's own 1528 (keyword does not belong to ad group) would catch only
//     some of it, after the fact, and per item.
//
// Where Microsoft does NOT allow it: the batch is not atomic. The client reports one outcome
// per action (APPLIED / FAILED / UNCONFIRMED), positionally, and returns an error only when no
// call was answered per item.
func (d *MicrosoftDispatcher) ApplyKeywordActions(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, actions []model.KeywordAction) ([]model.KeywordActionOutcome, error) {
	const op = "apply microsoft keyword actions"
	in := make([]microsoft.KeywordAction, 0, len(actions))
	for _, a := range actions {
		in = append(in, microsoft.KeywordAction{AdGroupID: a.AdGroupID, KeywordID: a.CriterionID, Action: a.Action})
	}
	validated, err := microsoft.ValidateKeywordActions(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrKeywordActionInvalid, err)
	}

	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, fmt.Errorf("%w: microsoft campaign has no platform campaign id, so it has no keywords to act on", domain.ErrCampaignNotProvisioned)
	}
	adGroupID, _ := microsoftChildIDs(campaign)
	adGroupID = strings.TrimSpace(adGroupID)
	if adGroupID == "" {
		return nil, fmt.Errorf("%w: microsoft campaign %s has no provisioned ad group, so it has no keywords to act on", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}
	for i, a := range validated {
		if a.AdGroupID != adGroupID {
			return nil, fmt.Errorf("%w: keyword action %d names ad group %s, which does not belong to campaign %s",
				domain.ErrKeywordActionInvalid, i, a.AdGroupID, campaign.PlatformCampaignID)
		}
	}

	client, err := d.microsoftKeywordLeverClient(ctx, projectID, platform, campaign, op)
	if err != nil {
		return nil, err
	}

	live, err := client.GetAdGroupKeywords(ctx, adGroupID)
	if err != nil {
		// A PURE READ, so its failure is DEFINITE: no mutation was built. Returned without
		// the unconfirmed wrapper so the caller is told to retry, not to verify a change that
		// was never attempted.
		return nil, fmt.Errorf("%s: read the ad group's keywords before acting: %w", op, err)
	}
	present := make(map[string]bool, len(live))
	for _, k := range live {
		if !k.IsDeleted() {
			present[k.ID] = true
		}
	}
	for i, a := range validated {
		if !present[a.KeywordID] {
			return nil, fmt.Errorf("%w: keyword action %d names keyword %s, which is not a live keyword in ad group %s of campaign %s",
				domain.ErrKeywordActionInvalid, i, a.KeywordID, adGroupID, campaign.PlatformCampaignID)
		}
	}

	outcomes, err := client.ApplyKeywordActions(ctx, adGroupID, validated)
	if err != nil {
		if microsoft.IsOutcomeUnconfirmed(err) {
			return nil, &unconfirmedKeywordLeverError{err: err}
		}
		return nil, err
	}
	out := make([]model.KeywordActionOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		out = append(out, model.KeywordActionOutcome{
			AdGroupID:   o.AdGroupID,
			CriterionID: o.KeywordID,
			Action:      o.Action,
			Outcome:     microsoftKeywordOutcome(o.Outcome),
			ErrorCode:   o.ErrorCode,
		})
	}
	return out, nil
}

// AddNegativeKeywords implements service.NegativeKeywordAdder for Microsoft Advertising:
// campaign-level negatives via AddNegativeKeywordsToEntities.
//
// Same guard order as ApplyKeywordActions — batch, provisioning, provenance (fail closed),
// account — all before Microsoft is contacted. There is no ownership read: the only id sent is
// the campaign's own, taken from the row, so nothing the caller supplied can address another
// campaign.
func (d *MicrosoftDispatcher) AddNegativeKeywords(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, keywords []model.NegativeKeyword) ([]model.NegativeKeywordOutcome, error) {
	const op = "add microsoft negative keywords"
	in := make([]microsoft.NegativeKeyword, 0, len(keywords))
	for _, k := range keywords {
		in = append(in, microsoft.NegativeKeyword{Text: k.Text, MatchType: k.MatchType})
	}
	validated, err := microsoft.ValidateNegativeKeywords(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrNegativeKeywordInvalid, err)
	}
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, fmt.Errorf("%w: microsoft campaign has no platform campaign id, so there is no campaign to add negative keywords to", domain.ErrCampaignNotProvisioned)
	}
	campaignID := strings.TrimSpace(campaign.PlatformCampaignID)
	if !microsoftCampaignIDRE.MatchString(campaignID) {
		return nil, fmt.Errorf("%w: the recorded platform campaign id %q is not a Microsoft campaign id", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}

	client, err := d.microsoftKeywordLeverClient(ctx, projectID, platform, campaign, op)
	if err != nil {
		return nil, err
	}
	outcomes, err := client.AddCampaignNegativeKeywords(ctx, campaignID, validated)
	if err != nil {
		if microsoft.IsOutcomeUnconfirmed(err) {
			return nil, &unconfirmedKeywordLeverError{err: err}
		}
		return nil, err
	}
	out := make([]model.NegativeKeywordOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		out = append(out, model.NegativeKeywordOutcome{
			Text:              o.Text,
			MatchType:         o.MatchType,
			Outcome:           microsoftKeywordOutcome(o.Outcome),
			NegativeKeywordID: o.NegativeKeywordID,
			ErrorCode:         o.ErrorCode,
		})
	}
	return out, nil
}

// microsoftKeywordOutcome maps the client's outcome vocabulary onto the model's. Anything the
// client did not name is UNCONFIRMED — never APPLIED, which would claim a change no one saw.
func microsoftKeywordOutcome(o string) string {
	switch o {
	case microsoft.OutcomeApplied:
		return model.KeywordOutcomeApplied
	case microsoft.OutcomeAlreadyPresent:
		return model.KeywordOutcomeAlreadyPresent
	case microsoft.OutcomeFailed:
		return model.KeywordOutcomeFailed
	default:
		return model.KeywordOutcomeUnconfirmed
	}
}

// microsoftLiveKeywordIDs filters the keyword ids a campaign row recorded at create down to
// the ones still live in its ad group, for the status cascade.
//
// It exists because keyword REMOVE (ApplyKeywordActions) deletes keywords upstream and this
// service persists nothing about it, so the row's keywordIds can name a deleted keyword. The
// cascade's UpdateKeywords would then report that keyword in PartialErrors, and every later
// toggle of the campaign would be a partially-applied cascade — UNCONFIRMED, forever.
func microsoftLiveKeywordIDs(ctx context.Context, client *microsoft.Client, adGroupID string, recorded []string) ([]string, error) {
	live, err := client.GetAdGroupKeywords(ctx, adGroupID)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(live))
	for _, k := range live {
		if !k.IsDeleted() {
			present[k.ID] = true
		}
	}
	out := make([]string, 0, len(recorded))
	for _, id := range recorded {
		if present[strings.TrimSpace(id)] {
			out = append(out, id)
		}
	}
	return out, nil
}
