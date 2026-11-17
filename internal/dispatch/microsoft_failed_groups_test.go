// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// A failed group with no confirmed ad group of its own is "NOT created" (create it); one whose ad
// group Microsoft confirmed before its ad or keyword step failed exists upstream, so the step
// says which step to finish there and never tells the operator to create it.
func TestMicrosoftAdGroupExtras_FailedGroupWording(t *testing.T) {
	result := &microsoft.CampaignResult{CampaignID: "321", AdGroups: []microsoft.AdGroupResult{
		{NameSuffix: "A", AdGroupID: "654", AdID: "987", KeywordIDs: []string{"701"}, Status: microsoft.AdGroupStatusCreated},
		{NameSuffix: "B", Status: microsoft.AdGroupStatusFailed, Error: "rejected"},
		{NameSuffix: "C", AdGroupID: "656", Status: microsoft.AdGroupStatusFailed, Error: "ad rejected"},
		{NameSuffix: "D", AdGroupID: "657", AdID: "990", KeywordIDs: []string{"704"}, Status: microsoft.AdGroupStatusFailed, Error: "keyword 2 rejected"},
		{NameSuffix: "E", AdGroupID: "654", Status: microsoft.AdGroupStatusFailed, Error: "same name"},
		{NameSuffix: "F", Status: microsoft.AdGroupStatusNotAttempted},
	}}
	if !applyMicrosoftAdGroupExtras(context.Background(), nil, result, microsoftConfig{}, nil) {
		t.Fatal("a campaign with failed groups must be degraded")
	}
	var notCreated, incomplete string
	for _, s := range result.Steps {
		switch {
		case strings.HasPrefix(s, "Ad groups NOT created: "):
			notCreated = s
		case strings.HasPrefix(s, "Ad groups created in Microsoft but NOT completed: "):
			incomplete = s
		}
	}
	if !strings.Contains(notCreated, "Ad groups NOT created: entry 2 (B), entry 5 (E), entry 6 (F).") {
		t.Errorf("not-created step = %q", notCreated)
	}
	for _, e := range []string{"entry 3", "entry 4"} {
		if strings.Contains(notCreated, e) {
			t.Errorf("not-created step names %s, whose ad group exists upstream: %q", e, notCreated)
		}
	}
	for _, want := range []string{
		"entry 3 (C): ad group 656 exists but its ad (and so its keywords) was not created",
		"entry 4 (D): ad group 657 exists but its keywords were not all created",
		"Do not create them again; finish them in Microsoft Advertising",
	} {
		if !strings.Contains(incomplete, want) {
			t.Errorf("incomplete step = %q, want it to contain %q", incomplete, want)
		}
	}
}

// With every failed group lacking an ad group id, only the "NOT created" step is written.
func TestMicrosoftAdGroupExtras_NoIncompleteStepWithoutAConfirmedGroup(t *testing.T) {
	result := &microsoft.CampaignResult{CampaignID: "321", AdGroups: []microsoft.AdGroupResult{
		{NameSuffix: "A", AdGroupID: "654", Status: microsoft.AdGroupStatusCreated},
		{NameSuffix: "B", Status: microsoft.AdGroupStatusFailed, Error: "rejected"},
	}}
	applyMicrosoftAdGroupExtras(context.Background(), nil, result, microsoftConfig{}, nil)
	if len(result.Steps) != 1 || !strings.HasPrefix(result.Steps[0], "Ad groups NOT created: entry 2 (B).") {
		t.Errorf("steps = %q", result.Steps)
	}
}
