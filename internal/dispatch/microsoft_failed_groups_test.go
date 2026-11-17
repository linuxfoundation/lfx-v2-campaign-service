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

// A group whose ad group Microsoft confirmed but whose ad or keyword step was UNCONFIRMED may
// already carry that ad or those keywords: it gets the verify-first wording, never "add the
// missing ad", while a DEFINITE ad failure still does (Greptile, #623).
func TestMicrosoftAdGroupExtras_UnconfirmedAdStepStaysUncertain(t *testing.T) {
	result := &microsoft.CampaignResult{CampaignID: "321", AdGroups: []microsoft.AdGroupResult{
		{NameSuffix: "A", AdGroupID: "654", AdID: "987", KeywordIDs: []string{"701"}, Status: microsoft.AdGroupStatusCreated},
		{NameSuffix: "B", AdGroupID: "655", Status: microsoft.AdGroupStatusFailed, Error: "ad creation UNCONFIRMED", StepUnconfirmed: true},
		{NameSuffix: "C", AdGroupID: "656", AdID: "989", Status: microsoft.AdGroupStatusFailed, Error: "keyword targeting UNCONFIRMED", StepUnconfirmed: true},
		{NameSuffix: "D", AdGroupID: "657", Status: microsoft.AdGroupStatusFailed, Error: "ad rejected"},
	}}
	if !applyMicrosoftAdGroupExtras(context.Background(), nil, result, microsoftConfig{}, nil) {
		t.Fatal("a campaign with failed groups must be degraded")
	}
	var uncertain, incomplete string
	for _, s := range result.Steps {
		switch {
		case strings.HasPrefix(s, "Ad groups created in Microsoft whose ad or keyword step is UNCONFIRMED: "):
			uncertain = s
		case strings.HasPrefix(s, "Ad groups created in Microsoft but NOT completed: "):
			incomplete = s
		}
	}
	for _, want := range []string{
		"entry 2 (B): ad group 655 exists but its ad may or may not exist",
		"entry 3 (C): ad group 656 exists but its keywords may or may not all exist",
		"verify in Microsoft Advertising before adding anything",
		"Do not create them again",
	} {
		if !strings.Contains(uncertain, want) {
			t.Errorf("uncertain step = %q, want it to contain %q", uncertain, want)
		}
	}
	if strings.Contains(uncertain, "add the missing ad") {
		t.Errorf("uncertain step tells the operator to add the ad: %q", uncertain)
	}
	for _, e := range []string{"entry 2", "entry 3"} {
		if strings.Contains(incomplete, e) {
			t.Errorf("NOT-completed step names %s, whose ad or keyword step was unconfirmed: %q", e, incomplete)
		}
	}
	for _, want := range []string{"entry 4 (D): ad group 657 exists but its ad (and so its keywords) was not created", "add the missing ad and/or keywords"} {
		if !strings.Contains(incomplete, want) {
			t.Errorf("incomplete step = %q, want it to contain %q", incomplete, want)
		}
	}
	if strings.Contains(incomplete, "entry 4") && strings.Contains(uncertain, "entry 4") {
		t.Errorf("entry 4 named as both uncertain and incomplete")
	}
}
