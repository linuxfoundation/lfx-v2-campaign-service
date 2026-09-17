// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import "github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"

// validateMonitorDays is the dispatcher-side twin of
// internal/service/connection_monitor.go's validateMonitorDays — see domain.ErrMonitorDaysInvalid's
// doc comment for why the same 7..90 bound is re-checked here, next to each dispatcher's own
// Validate*AccountID call, rather than trusted from the caller. Shared by all four account-monitor
// dispatchers because they live in this one package.
func validateMonitorDays(days int) error {
	if days < domain.MonitorDaysMin || days > domain.MonitorDaysMax {
		return domain.ErrMonitorDaysInvalid
	}
	return nil
}
