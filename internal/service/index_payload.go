// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
)

// BriefIndexPayload is the outbox payload for one brief: the exact message every brief write
// co-commits (see BriefService.briefIndexPayload). Exported so the reindex command enqueues
// the same document the service does, never a hand-built one. It carries no bearer token;
// the relay stamps a service credential at publish time.
func BriefIndexPayload(action string, b *model.CampaignBrief) ([]byte, error) {
	return json.Marshal(indexer.NewTransaction(
		action, indexer.ObjectTypeBrief,
		b.ID, b.ProjectID, "",
		briefDoc(briefResult(b)), b.EventSlug,
	))
}

// CampaignIndexPayload is the outbox payload for one campaign, shared by the request-path
// writes, the orchestrator's dispatch writes and the reindex command.
func CampaignIndexPayload(action string, c *model.Campaign) ([]byte, error) {
	return json.Marshal(indexer.NewTransaction(
		action, indexer.ObjectTypeCampaign,
		c.ID, c.ProjectID, "",
		campaignDoc(campaignResult(c)), c.CampaignName,
	))
}
