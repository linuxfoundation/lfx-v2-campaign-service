// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// EmailMonitorReader is an OPTIONAL dispatcher capability: the HubSpot email account monitor's
// read (LFXV2-2665) — the statistics counters of the marketing emails this service created for a
// project, over a trailing `days` window. HubSpot's dispatcher is the only implementation; like
// AccountMetricsReader the orchestrator type-asserts for it, and a platform without it answers
// ErrAccountMetricsUnsupported (400).
//
// It is a separate capability rather than AccountMetricsReader because the two scopes are
// opposite. The ad-platform monitors read a raw ad ACCOUNT — every campaign the credential
// reaches. A HubSpot portal is shared across projects and has no per-project account, so the
// only safe scope is the project's OWN recorded emails, which the orchestrator reads from this
// service's table and hands over: the dispatcher never chooses what is in scope.
//
// The contract a dispatcher must honour:
//   - resolve the project's OWN connection only (no LF system fallback);
//   - read only the emails of the campaigns passed, and only those whose recorded creating
//     portal is the portal the token now reaches (count the rest as unattributable);
//   - fail the whole read on any upstream failure — no partial result — except an email HubSpot
//     reports no send of inside the span, which is counted;
//   - return a non-nil Emails slice on success.
type EmailMonitorReader interface {
	ReadEmailMonitor(ctx context.Context, projectID string, platform model.Provider, campaigns []*model.Campaign, days int) (*model.HubSpotEmailMonitorRead, error)
}

// hubspotMonitorMaxCampaigns is how many of a project's recorded HubSpot campaigns one monitor
// read checks — the most recently recorded ones. Each costs one or two statistics requests (its
// email and any A/B variant), so this bounds the read at 100 requests plus one token-info call,
// inside the 20s budget and a private app's burst allowance. A project with more is answered with
// Truncated set rather than refused: the newest emails are the ones a trailing window of at most
// 90 days is about, and the flag says the rest were not checked.
const hubspotMonitorMaxCampaigns = 50

// opReadEmailMonitor is the upstream-operation token for the read (see the main token block in
// orchestrator.go for the bounding rule).
const opReadEmailMonitor = "read_email_monitor"

// ReadHubSpotEmailMonitor is the email account monitor's read. In order:
//
//  1. the dispatcher capability (a platform without it is "not supported", 400);
//  2. the project's recorded campaigns on the platform, from this service's own table by
//     project_id, newest first, cap plus one — the extra row only says "there were more";
//  3. an EMPTY scope answers an empty read with NO upstream call and no connection lookup, the
//     same early return the project-scoped keyword and audience reads make: a project that has
//     sent nothing has nothing to show, and there is no unscoped read to fall back to;
//  4. the dispatcher read, inside accountsCallTimeout, recorded as an upstream call.
func (o *Orchestrator) ReadHubSpotEmailMonitor(ctx context.Context, projectID string, platform model.Provider, days int) (*model.HubSpotEmailMonitorRead, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrAccountMetricsUnsupported, platform)
	}
	reader, ok := d.(EmailMonitorReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAccountMetricsUnsupported, platform)
	}
	campaigns, err := o.campaigns.ListRecentProjectPlatformCampaigns(ctx, projectID, platform, hubspotMonitorMaxCampaigns+1)
	if err != nil {
		return nil, fmt.Errorf("resolve email monitor scope for project %s on %s: %w", projectID, platform, err)
	}
	if len(campaigns) == 0 {
		return &model.HubSpotEmailMonitorRead{Emails: []model.HubSpotMonitorEmail{}}, nil
	}
	truncated := len(campaigns) > hubspotMonitorMaxCampaigns
	if truncated {
		campaigns = campaigns[:hubspotMonitorMaxCampaigns]
	}

	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	read, rerr := reader.ReadEmailMonitor(callCtx, projectID, platform, campaigns, days)
	o.recordUpstream(ctx, platform, opReadEmailMonitor, start, rerr)
	if rerr != nil {
		return nil, rerr
	}
	if read == nil || read.Emails == nil {
		// (nil, nil) or a nil slice is a contract violation, not "no emails": the caller could
		// not tell an authoritative empty answer from an adapter that fell through a branch.
		return nil, fmt.Errorf("%s email monitor reader returned a nil result with no error", platform)
	}
	read.Truncated = truncated
	return read, nil
}
