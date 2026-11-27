// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"os"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// monitorScopeAccount is the one value of the account monitors' scope attribute (#672).
const monitorScopeAccount = "account"

// SetMarketingOpsChecker injects the global_marketing_ops checker the account monitors'
// scope=account read requires; the container calls it. Nil leaves that read refused with 503.
func (s *ConnectionService) SetMarketingOpsChecker(c MarketingOpsChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marketingOps = c
}

func (s *ConnectionService) marketingOpsChecker() MarketingOpsChecker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.marketingOps
}

// accountMonitorAccountScopeEnabled reports whether ACCOUNT_MONITOR_ACCOUNT_SCOPE_ENABLED is
// exactly "true". Read per request, like the other deployment gates, so a test can flip it.
func accountMonitorAccountScopeEnabled() bool {
	return os.Getenv(constants.EnvAccountMonitorAccountScopeEnabled) == "true"
}

// admitAccountWideMonitor decides whether an account-monitor request reads account-wide (#672).
// No scope keeps the project-scoped read (false, nil). scope=account is refused with 400 while
// the deployment flag is off, and otherwise admitted only for a caller holding
// global_marketing_ops on the project: 403 on a definite denial, 503 when the role cannot be
// checked (no checker, no authenticated username, or the check failed). Fail closed throughout:
// only a definite grant returns true. The gateway checks the same relation first; this is the
// service's own check, so a route reached any other way is held to it too.
func (s *ConnectionService) admitAccountWideMonitor(ctx context.Context, projectID string, scope *string) (bool, error) {
	if scope == nil {
		return false, nil
	}
	if *scope != monitorScopeAccount {
		return false, &conn.BadRequestError{Code: "400", Message: "scope must be \"account\" when given"}
	}
	if !accountMonitorAccountScopeEnabled() {
		return false, &conn.BadRequestError{Code: "400", Message: "scope=account is not enabled on this deployment (" +
			constants.EnvAccountMonitorAccountScopeEnabled + "); omit scope to read this project's own campaigns"}
	}
	unavailable := &conn.ConnServiceUnavailableError{Code: "503", Message: "could not check the role scope=account requires; retry, or omit scope"}
	checker := s.marketingOpsChecker()
	if checker == nil {
		slog.ErrorContext(ctx, "refusing account-wide monitor: no marketing-ops checker is configured (NATS_URL empty?)", "project_id", projectID)
		return false, unavailable
	}
	actor := actorFromCtx(ctx)
	if actor == nil || actor.Username == "" {
		slog.ErrorContext(ctx, "refusing account-wide monitor: the request carries no authenticated username", "project_id", projectID)
		return false, unavailable
	}
	ok, err := checker.IsGlobalMarketingOps(ctx, projectID, actor.Username)
	if err != nil {
		slog.ErrorContext(ctx, "refusing account-wide monitor: the global_marketing_ops check failed", "project_id", projectID, "error", err)
		return false, unavailable
	}
	if !ok {
		slog.WarnContext(ctx, "refused account-wide monitor from a caller without global_marketing_ops", "project_id", projectID)
		return false, &conn.ForbiddenError{Code: "403", Message: "scope=account requires the global_marketing_ops role on this project; omit scope to read this project's own campaigns"}
	}
	return true, nil
}
