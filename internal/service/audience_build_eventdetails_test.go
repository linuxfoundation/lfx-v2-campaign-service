// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The UI writes `name` and `countryCode`; this decoder read only `eventName` and `country`.
// Both are REQUIRED (decodeEventDetails errors on either), so a brief the UI actually produced
// failed the audience build twice over — and BuildAudience is the gate the HubSpot dispatcher
// waits on, so the email channel could not stage at all.
//
// The fixture is the REAL stored `event_details` from brief 77f52f20-… (trimmed), not an
// invented shape, so this binds to what the UI persists rather than to what we wish it did.
// The paid path had the same defect (LFXV2-3259); this decoder is a separate struct that the
// dispatch-side fix does not cover.
func TestDecodeEventDetailsReadsUISpellings(t *testing.T) {
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

	got, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "77f52f20-3088-4044-8b5e-871f51694235",
		EventDetails: json.RawMessage(uiEventDetails),
	})
	if err != nil {
		t.Fatalf("decodeEventDetails rejected a brief the UI actually wrote: %v", err)
	}
	if got.EventName != "Agntcon Mcpcon Japan" {
		t.Errorf("EventName = %q, want %q — the UI spells it `name`", got.EventName, "Agntcon Mcpcon Japan")
	}
	if got.Country != "US" {
		t.Errorf("Country = %q, want %q — the UI spells it `countryCode`", got.Country, "US")
	}
}

// The explicit spellings must WIN: `name`/`countryCode` are fallbacks, not equals.
func TestDecodeEventDetailsPrefersExplicitSpellings(t *testing.T) {
	got, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "b-1",
		EventDetails: json.RawMessage(`{"eventName":"Explicit","name":"Generic","country":"JP","countryCode":"US"}`),
	})
	if err != nil {
		t.Fatalf("decodeEventDetails: %v", err)
	}
	if got.EventName != "Explicit" {
		t.Errorf("EventName = %q, want %q — `eventName` must take precedence", got.EventName, "Explicit")
	}
	if got.Country != "JP" {
		t.Errorf("Country = %q, want %q — `country` must take precedence", got.Country, "JP")
	}
}

// A brief carrying NEITHER spelling must still error. Without this the fix could be satisfied
// by a decoder that accepts anything, which would push the failure into the HubSpot list build
// instead of catching it here where the message names the missing field.
func TestDecodeEventDetailsStillErrorsWhenNamePresentButNoCountry(t *testing.T) {
	if _, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "b-2",
		EventDetails: json.RawMessage(`{"name":"Some Event"}`),
	}); err == nil {
		t.Fatal("expected an error for a brief with no country in any spelling, got nil")
	}
}
