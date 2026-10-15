// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"math"
	"time"
)

// The package's other tests make hundreds of report calls on one fixture account in well under a
// minute; against the production ledger they would wait out its window. They are about other
// behaviour, so the test binary's process-wide ledger is unlimited, and report_budget_test.go
// exercises the real limits on ledgers of its own.
func init() {
	defaultReportBudgets = newReportBudgets(reportBudgetWindow, math.MaxInt32, math.MaxInt32, time.Now)
}
