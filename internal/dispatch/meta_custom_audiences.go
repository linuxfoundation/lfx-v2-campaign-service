// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.MetaCustomAudienceReader = (*MetaDispatcher)(nil)

// ListMetaCustomAudiences lists every custom audience of the Meta ad account a create for the
// project uses (#550), for the metaConfig.targeting pickers.
//
// It resolves with creds.resolve, the CREATE path's resolution (Dispatch's), because the ids are
// only useful if the create accepts them: the create reads each audience and refuses one outside
// its ad account.
//
// TENANCY: the LF Meta ad account is shared by every foundation, so this returns every
// foundation's audiences; the gateway admits only global_marketing_ops of the project to the
// route. called is false for every local refusal before the read (no connection, an unusable one,
// no ad account, a stored account id that is not act_<digits>).
func (d *MetaDispatcher) ListMetaCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (_ *model.MetaCustomAudiences, called bool, _ error) {
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, false, err
	}
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return nil, false, err
	}
	if verr := meta.ValidateAccountID(accountID); verr != nil {
		return nil, false, res.systemScoped(fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr))
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	rows, lerr := client.ListCustomAudiences(ctx, accountID)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no audience is a real, empty answer.
	out := make([]model.MetaCustomAudience, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.MetaCustomAudience{
			ID: r.ID, Name: r.Name, Subtype: r.Subtype, Description: r.Description,
			CountLower: r.CountLower, CountUpper: r.CountUpper,
		})
	}
	return &model.MetaCustomAudiences{AccountID: accountID, Audiences: out}, true, nil
}
