// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package metrics

import (
	"regexp"
	"strings"
	"testing"
)

type fakeBudget struct{ inUse, total int64 }

func (f fakeBudget) InUse() int64 { return f.inUse }
func (f fakeBudget) Total() int64 { return f.total }

// TestUploadBudgetGauges pins the #179 gauges: the bytes of the per-pod upload memory budget in
// use, and its size, exported with the values the budget reports — and nothing at all while no
// budget is wired, because a zero would read as a measurement.
func TestUploadBudgetGauges(t *testing.T) {
	r := newTestRegistry(t)
	if body := scrape(t, r); strings.Contains(body, "campaign_upload_memory_budget") {
		t.Fatalf("upload budget gauges exported with no budget wired:\n%s", body)
	}

	r.SetUploadBudget(fakeBudget{inUse: 12345, total: 214748364})
	body := scrape(t, r)
	for name, want := range map[string]string{
		"campaign_upload_memory_budget_in_use_bytes": "12345",
		"campaign_upload_memory_budget_bytes":        "2.14748364e+08",
	} {
		re := regexp.MustCompile(`(?m)^` + name + `(\{[^}]*\})? (\S+)$`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("scrape output missing %s:\n%s", name, body)
			continue
		}
		if m[2] != want {
			t.Errorf("%s = %s, want %s", name, m[2], want)
		}
	}
}
