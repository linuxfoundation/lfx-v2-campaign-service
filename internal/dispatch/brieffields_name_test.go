// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The UI writes the event name as `name`, not `eventName`. `event_details` is typed `Any`
// in the design, so nothing arbitrates the key, and every paid create decoded EventName as
// "" and was refused with "brief %s has no eventName in its details" before reaching the ad
// platform. Observed end-to-end against a real brief on 2026-08-13 (four consecutive job
// failures, zero campaigns created).
//
// The fixture is the ACTUAL stored shape from that brief, trimmed — not an invented one —
// so this binds to what the UI really persists rather than to what we wish it did.
func TestDecodeBriefFieldsReadsUIEventName(t *testing.T) {
	const uiEventDetails = `{
		"city": "",
		"name": "Agntcon Mcpcon Japan",
		"slug": "agntcon-mcpcon-japan",
		"dates": "",
		"themes": [],
		"audience": "",
		"speakers": [],
		"countryCode": "US",
		"formatNotes": "",
		"registrationUrl": "https://events.linuxfoundation.org/agntcon-mcpcon-japan/"
	}`

	brief := &model.CampaignBrief{
		ID:           "77f52f20-3088-4044-8b5e-871f51694235",
		URL:          "https://events.linuxfoundation.org/agntcon-mcpcon-japan/",
		EventDetails: json.RawMessage(uiEventDetails),
	}

	bf, err := decodeBriefFields(brief)
	if err != nil {
		t.Fatalf("decodeBriefFields returned an error for a brief the UI actually wrote: %v", err)
	}
	if bf.EventName != "Agntcon Mcpcon Japan" {
		t.Errorf("EventName = %q, want %q — the UI spells it `name` and the decoder must accept that",
			bf.EventName, "Agntcon Mcpcon Japan")
	}
}

// `eventName` must WIN when both spellings are present: `name` is a fallback, not an equal.
// Without this, a blob carrying the explicit key could be overridden by a generic one.
func TestDecodeBriefFieldsPrefersExplicitEventName(t *testing.T) {
	brief := &model.CampaignBrief{
		ID:           "b-1",
		URL:          "https://example.com/",
		EventDetails: json.RawMessage(`{"eventName":"Explicit Name","name":"Generic Name"}`),
	}

	bf, err := decodeBriefFields(brief)
	if err != nil {
		t.Fatalf("decodeBriefFields: %v", err)
	}
	if bf.EventName != "Explicit Name" {
		t.Errorf("EventName = %q, want %q — `eventName` must take precedence over `name`",
			bf.EventName, "Explicit Name")
	}
}

// A brief carrying NEITHER spelling must still error. Without this the fix could be
// "verified" by a decoder that returns a name for everything, which would push the failure
// to the ad platform instead of catching it here.
func TestDecodeBriefFieldsStillErrorsWithNoName(t *testing.T) {
	brief := &model.CampaignBrief{
		ID:           "b-2",
		URL:          "https://example.com/",
		EventDetails: json.RawMessage(`{"city":"Tokyo","slug":"x"}`),
	}

	if _, err := decodeBriefFields(brief); err == nil {
		t.Fatal("expected an error for a brief with no event name in any spelling, got nil")
	}
}
