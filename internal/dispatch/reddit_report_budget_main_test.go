// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"math"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// This package's tests build Reddit dispatchers by the hundred with no database, and are about
// other behaviour; against the production budget — a shared ledger that refuses every report until
// Postgres is bound — every report read would fail closed. So the test binary's dispatchers spend
// from an unlimited process-local ledger, and reddit_report_ledger_test.go exercises the shared
// one (and its fail-closed default) on purpose.
func init() {
	newRedditReportBudgets = func() *reddit.ReportBudgets {
		return reddit.NewReportBudgets(time.Minute, math.MaxInt32, math.MaxInt32)
	}
}
