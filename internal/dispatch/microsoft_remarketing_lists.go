// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"os"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The remarketing-list picker read (#563 gap 4 follow-up) behind
// list-microsoft-ads-remarketing-lists: every remarketing list on the account a Microsoft create
// for the project uses, for picking microsoftConfig.audienceIds and excludedAudienceIds.
//
// Two gates, independent of each other, as for the Reddit and X pickers.
// MICROSOFT_REMARKETING_LISTS_ENABLED (here) keeps a read no live call has exercised off until an
// operator turns it on. The gateway's marketing-ops-api rule admits only global_marketing_ops of
// the project to the route, because the shared LF account holds every foundation's lists.

// microsoftRemarketingListsEnabled reports whether this deployment opted in. Read per call, like
// microsoftAudiencesEnabled, so it flips without a rebuild.
func microsoftRemarketingListsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftRemarketingListsEnabled) == "true"
}

// ListMicrosoftRemarketingLists lists the remarketing lists on the account a Microsoft create for
// the project uses. Credentials resolve as for a create (d.creds.resolve: the project's own
// connection, or the LF system account when the deployment forces it) and the connection is
// validated by validateMicrosoftConnection, the create's own check, so every id listed is one a
// create for this project can send and its pre-create check (CheckRemarketingLists, the same
// call with ids) accepts.
//
// called reports whether Microsoft was contacted: false for the deployment gate and for a local
// refusal before the request (no connection, an unusable one).
func (d *MicrosoftDispatcher) ListMicrosoftRemarketingLists(ctx context.Context, projectID string, platform model.Provider) (_ *model.MicrosoftRemarketingLists, called bool, _ error) {
	if !microsoftRemarketingListsEnabled() {
		return nil, false, fmt.Errorf("microsoft remarketing lists are disabled (%s is not \"true\") until the read is exercised against a live account: %w",
			constants.EnvMicrosoftRemarketingListsEnabled, domain.ErrTargetingLookupUnsupported)
	}
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	creds, accountID, err := validateMicrosoftConnection(projectID, res)
	if err != nil {
		return nil, false, err
	}
	client := d.cachedMicrosoftClient(projectID, platform, res, creds, accountID)
	rows, lerr := client.ListRemarketingLists(ctx)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no list is a real, empty answer.
	out := make([]model.MicrosoftRemarketingList, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.MicrosoftRemarketingList{
			ID: r.ID, Name: r.Name, Description: r.Description, Scope: r.Scope,
			MembershipDuration: r.MembershipDuration, SearchSize: r.SearchSize,
		})
	}
	return &model.MicrosoftRemarketingLists{AccountID: client.AccountID(), Lists: out}, true, nil
}
