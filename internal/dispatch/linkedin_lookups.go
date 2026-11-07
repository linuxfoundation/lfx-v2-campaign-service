// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// LinkedInAdSegmentKind is the one AudienceReader kind the LinkedIn dispatcher serves: the ad
// segments (matched and retargeting audiences) linkedInConfig.targeting names (#552).
const LinkedInAdSegmentKind = "ad_segment"

// createLookupClient resolves the client a LinkedIn create for the project would use and its
// account: creds.resolve, the CREATE path's resolution (the project's own connection, the LF
// system fallback when it has none, and the forced-system redirect), so the ids a picker offers
// are on the account the create sends them to. Nothing is sent to LinkedIn.
func (d *LinkedInDispatcher) createLookupClient(ctx context.Context, projectID string, platform model.Provider) (*resolved, *linkedin.Client, string, error) {
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, nil, "", err
	}
	accountID := strings.TrimSpace(res.accountID)
	// A malformed bound account is stored connection configuration the operator must repair, and
	// it is refused here, before any request, so the read is not recorded as an upstream call.
	// ValidAccountID, not ValidateAccountID: the id must be a canonical positive int64 (no
	// leading zero, no overflow), as ReadSettings requires, or LinkedIn would refuse the URN and
	// the refusal would read as an upstream failure.
	if !linkedin.ValidAccountID(accountID) {
		return nil, nil, "", linkedinAccountIDUnusable(res, projectID, "linkedin picker lookup")
	}
	return res, d.cachedLinkedInClient(projectID, platform, res, creds, accountID), accountID, nil
}

// linkedInLookupError classifies a picker lookup failure the way the other LinkedIn reads do: a
// credential defect is the connection's, everything else is the platform's (503 upstream). A
// malformed bound account never gets here: createLookupClient refuses it first.
func linkedInLookupError(op string, res *resolved, err error) error {
	if linkedinConnectionDefect(err) {
		return res.systemScoped(linkedinExpiry(err))
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ListAudiences implements service.AudienceReader for LinkedIn: the ad segments on the account a
// create uses (#552). Only LinkedInAdSegmentKind is served.
func (d *LinkedInDispatcher) ListAudiences(ctx context.Context, projectID string, platform model.Provider, kind string) (_ *model.AudienceResources, called bool, _ error) {
	if kind != LinkedInAdSegmentKind {
		return nil, false, fmt.Errorf("%w: linkedin-ads audience kind %q", domain.ErrTargetingLookupUnsupported, kind)
	}
	res, client, accountID, err := d.createLookupClient(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	segments, err := client.ListAdSegments(ctx)
	if err != nil {
		return nil, true, linkedInLookupError("list linkedin ad segments", res, err)
	}
	out := make([]model.AudienceResource, 0, len(segments))
	for _, s := range segments {
		out = append(out, model.AudienceResource{
			ResourceName: s.URN(), ID: s.ID, Name: s.Name, Subtype: s.Type, Status: s.Status,
			Facet: s.Facet, MemberCount: s.ApproximateMemberCount,
		})
	}
	return &model.AudienceResources{AccountID: accountID, Kind: kind, Audiences: out}, true, nil
}

// ListConversionActions implements service.ConversionActionReader for LinkedIn: the conversion
// rules on the account a create uses (#552), enabled or not. Status is ENABLED or DISABLED.
func (d *LinkedInDispatcher) ListConversionActions(ctx context.Context, projectID string, platform model.Provider) (_ *model.ConversionActions, called bool, _ error) {
	res, client, accountID, err := d.createLookupClient(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	rules, err := client.ListConversionRules(ctx)
	if err != nil {
		return nil, true, linkedInLookupError("list linkedin conversion rules", res, err)
	}
	out := make([]model.ConversionAction, 0, len(rules))
	for _, r := range rules {
		status := "DISABLED"
		if r.Enabled {
			status = "ENABLED"
		}
		out = append(out, model.ConversionAction{ID: r.ID, ResourceName: r.URN(), Name: r.Name, Status: status, Type: r.Type})
	}
	return &model.ConversionActions{AccountID: accountID, Actions: out}, true, nil
}
