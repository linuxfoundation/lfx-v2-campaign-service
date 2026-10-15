// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The unmeasured-rows counter is served with the count added, labelled by platform and class.
func TestMonitorUnmeasuredRowsAppearInScrape(t *testing.T) {
	r := newTestRegistry(t)
	r.RecordMonitorUnmeasuredRows(context.Background(), model.ProviderRedditAds, MonitorFailureThrottle, 3)
	body := scrape(t, r)
	want := `campaign_monitor_unmeasured_rows_total{class="throttle",otel_scope_name=`
	if !strings.Contains(body, want) {
		t.Fatalf("scrape output missing %q:\n%s", want, body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "campaign_monitor_unmeasured_rows_total{") {
			if !strings.Contains(line, `platform="reddit-ads"`) || !strings.HasSuffix(line, " 3") {
				t.Errorf("series = %q, want platform reddit-ads and value 3", line)
			}
		}
	}
}

// The class label is a closed set mirroring model.MonitorFailureClass: an unrecognised value
// collapses to unknown, and every model class passes through unchanged.
func TestMonitorFailureClassLabelIsClosed(t *testing.T) {
	r := newTestRegistry(t)
	r.RecordMonitorUnmeasuredRows(context.Background(), model.ProviderMetaAds, "token abc123 revoked", 1)
	body := scrape(t, r)
	if strings.Contains(body, "abc123") {
		t.Errorf("unbounded class reached the exposition output:\n%s", body)
	}
	if !strings.Contains(body, `class="`+PlatformUnknown+`"`) {
		t.Errorf("expected an unrecognised class to collapse to %q:\n%s", PlatformUnknown, body)
	}
	for _, c := range []model.MonitorFailureClass{
		model.MonitorFailureAuth, model.MonitorFailureThrottle, model.MonitorFailureUpstream5xx,
		model.MonitorFailureDecode, model.MonitorFailureNoReport, model.MonitorFailureOther,
		model.MonitorFailureUnknown,
	} {
		if got := safeMonitorFailureClass(string(c)); got != string(c) {
			t.Errorf("safeMonitorFailureClass(%q) = %q, want it passed through", c, got)
		}
	}
}

// A non-positive count records nothing, and a nil registry is safe.
func TestMonitorUnmeasuredRowsIgnoresEmptyCounts(t *testing.T) {
	r := newTestRegistry(t)
	r.RecordMonitorUnmeasuredRows(context.Background(), model.ProviderMetaAds, MonitorFailureDecode, 0)
	if body := scrape(t, r); strings.Contains(body, "campaign_monitor_unmeasured_rows_total{") {
		t.Errorf("a zero count minted a series:\n%s", body)
	}
	var nilReg *Registry
	nilReg.RecordMonitorUnmeasuredRows(context.Background(), model.ProviderMetaAds, MonitorFailureDecode, 1)
}
