// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.MetaLeadFormReader = (*MetaDispatcher)(nil)

// ListMetaLeadForms lists every Instant Form of the Page on the Meta connection a create for the
// project uses (#550), for the metaConfig.leadFormId picker.
//
// It resolves with creds.resolve, the CREATE path's resolution (Dispatch's), and reads the same
// page_id Dispatch does, because a form is only useful if the create's own check
// (meta.checkLeadForm: the form's Page must be the connection's) accepts it.
//
// TENANCY: the LF system connection is the fallback of every project without its own, so its Page's
// forms can be other foundations'; the gateway admits only global_marketing_ops of the project to
// the route. called is false for every local refusal before the read (no connection, an unusable
// one, a connection without a page_id).
func (d *MetaDispatcher) ListMetaLeadForms(ctx context.Context, projectID string, platform model.Provider) (_ *model.MetaLeadForms, called bool, _ error) {
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, false, err
	}
	pageID := strings.TrimSpace(res.providerConfig["page_id"])
	if pageID == "" {
		return nil, false, res.systemScoped(fmt.Errorf("%w: %w: meta connection for project %s is missing page id",
			domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, projectID))
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	rows, lerr := client.ListLeadForms(ctx, pageID)
	if lerr != nil {
		return nil, true, res.systemScoped(lerr)
	}
	out := make([]model.MetaLeadForm, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.MetaLeadForm{ID: r.ID, Name: r.Name, Status: r.Status})
	}
	return &model.MetaLeadForms{PageID: pageID, Forms: out}, true, nil
}
