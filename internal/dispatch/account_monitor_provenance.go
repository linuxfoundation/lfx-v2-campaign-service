// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The account-wide monitor's row labels (#672) rank records by provenance with these, one per
// shared-tenancy platform. Each uses the extractor and normalisation its MonitorScopeForAccount
// compares with, so the two cannot disagree about which account a record names; what they leave
// out is the scoper's policy for an unrecorded record (see service.AccountMonitorProvenanceClassifier).
var (
	_ service.AccountMonitorProvenanceClassifier = (*GoogleAdsDispatcher)(nil)
	_ service.AccountMonitorProvenanceClassifier = (*MetaDispatcher)(nil)
	_ service.AccountMonitorProvenanceClassifier = (*RedditDispatcher)(nil)
	_ service.AccountMonitorProvenanceClassifier = (*TwitterDispatcher)(nil)
	_ service.AccountMonitorProvenanceClassifier = (*LinkedInDispatcher)(nil)
)

// monitorProvenance classifies a record's creation account against the monitored one, both
// already in the platform's comparable form; "" is a record that names none.
func monitorProvenance(created, monitored string) service.MonitorProvenance {
	switch created {
	case "":
		return service.ProvenanceUnknown
	case monitored:
		return service.ProvenanceMatch
	default:
		return service.ProvenanceForeign
	}
}

// MonitorProvenanceOf classifies by the creating customer, as googleAdsScopeForCustomer compares it.
func (d *GoogleAdsDispatcher) MonitorProvenanceOf(e model.ProjectCampaignScope, accountID string) service.MonitorProvenance {
	return monitorProvenance(googleAdsCreationCustomerID(&model.Campaign{Result: e.Result}), accountID)
}

// MonitorProvenanceOf classifies by the creating ad account, normalised as metaScopeForAccount does.
func (d *MetaDispatcher) MonitorProvenanceOf(e model.ProjectCampaignScope, accountID string) service.MonitorProvenance {
	return monitorProvenance(metaCreationAccountID(&model.Campaign{Result: e.Result}), normalizeMetaAccountID(accountID))
}

// MonitorProvenanceOf classifies by the creating ad account, as redditScopeForAccount compares it.
func (d *RedditDispatcher) MonitorProvenanceOf(e model.ProjectCampaignScope, accountID string) service.MonitorProvenance {
	return monitorProvenance(redditCreationAccountID(&model.Campaign{Result: e.Result}), strings.TrimSpace(accountID))
}

// MonitorProvenanceOf classifies by the creating ads account, as twitterScopeForAccount compares it.
func (d *TwitterDispatcher) MonitorProvenanceOf(e model.ProjectCampaignScope, accountID string) service.MonitorProvenance {
	return monitorProvenance(twitterCreationAccountID(&model.Campaign{Result: e.Result}), strings.TrimSpace(accountID))
}

// MonitorProvenanceOf classifies by the creating ad account, as linkedInScopeForAccount compares it.
func (d *LinkedInDispatcher) MonitorProvenanceOf(e model.ProjectCampaignScope, accountID string) service.MonitorProvenance {
	return monitorProvenance(linkedInCreationAccountID(&model.Campaign{PlatformCampaignID: e.PlatformCampaignID, Result: e.Result}), strings.TrimSpace(accountID))
}
