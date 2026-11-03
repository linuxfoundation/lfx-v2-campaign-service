// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The CLI prints conversions the way the API does (#175): the status always, the total only with
// `reported`, so an incomplete read and an unavailable channel are told apart in its output.
func TestToResult_PrintsConversionsStatus(t *testing.T) {
	zero := 0.0
	r := toResult(&model.CampaignMetrics{Clicks: 80, Conversions: &zero, ConversionsStatus: model.ConversionsIncomplete})
	if r.ConversionsStatus != model.ConversionsIncomplete || r.Conversions != nil {
		t.Errorf("got status %q, conversions %v; want incomplete with no total", r.ConversionsStatus, r.Conversions)
	}
	r = toResult(&model.CampaignMetrics{Clicks: 80, Conversions: &zero})
	if r.ConversionsStatus != model.ConversionsReported || r.Conversions == nil {
		t.Errorf("got status %q, conversions %v; want reported 0", r.ConversionsStatus, r.Conversions)
	}
}
