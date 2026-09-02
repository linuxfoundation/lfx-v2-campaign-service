// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The UI writes `name`; this decoder read only `eventName`. BuildAudience is the gate the
// HubSpot dispatcher waits on, so the email channel could not stage at all.
//
// `countryCode` is deliberately NOT accepted — see briefEventDetails. It would pass an ISO-2
// code into HubSpot filters that only alias `us`/`uk`, turning a loud failure into a silent
// empty audience. So this fixture carries `country` as well; a brief with only `countryCode`
// still fails, which is the correct outcome until an ISO-2 mapping exists.
//
// The fixture is the REAL stored `event_details` from brief 77f52f20-… (trimmed), not an
// invented shape, so this binds to what the UI persists rather than to what we wish it did.
// The paid path had the same defect (LFXV2-3259); this decoder is a separate struct that the
// dispatch-side fix does not cover.
func TestDecodeEventDetailsReadsUIEventName(t *testing.T) {
	const uiEventDetails = `{
		"city": "",
		"name": "Agntcon Mcpcon Japan",
		"slug": "agntcon-mcpcon-japan",
		"dates": "",
		"themes": [],
		"audience": "",
		"speakers": [],
		"countryCode": "US",
		"country": "United States",
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
	if got.Country != "United States" {
		t.Errorf("Country = %q, want %q", got.Country, "United States")
	}
}

// `eventName` must WIN where both are present: `name` is a fallback, not an equal.
func TestDecodeEventDetailsPrefersExplicitEventName(t *testing.T) {
	got, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "b-1",
		EventDetails: json.RawMessage(`{"eventName":"Explicit","name":"Generic","country":"JP"}`),
	})
	if err != nil {
		t.Fatalf("decodeEventDetails: %v", err)
	}
	if got.EventName != "Explicit" {
		t.Errorf("EventName = %q, want %q — `eventName` must take precedence", got.EventName, "Explicit")
	}
	if got.Country != "JP" {
		t.Errorf("Country = %q, want %q", got.Country, "JP")
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

// The UI writes `countryCode` and never `country`, so this is the shape EVERY brief created
// through the campaigns page actually has. Before `CountryForCode` existed the decoder refused
// it deliberately -- a literal `CN` reaches HubSpot as an exact filter value, matches nobody, and
// the build would SUCCEED while storing an empty inclusion list. The refusal was correct; what
// was missing was the mapping, so the fix resolves the code rather than passing it through.
func TestDecodeEventDetailsResolvesUICountryCode(t *testing.T) {
	got, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "b-cn",
		EventDetails: json.RawMessage(`{"name":"MCP Dev Summit Shanghai","countryCode":"CN"}`),
	})
	if err != nil {
		t.Fatalf("a brief carrying only countryCode must decode, got: %v", err)
	}
	// The NAME, not the code. `audience.DisplayName` sends this into HubSpot as an exact
	// CONTAINS/IS_ANY_OF value, so "CN" would match no contact.
	if got.Country != "china" {
		t.Errorf("countryCode CN resolved to %q, want %q", got.Country, "china")
	}
}

// An explicit `country` outranks the code: a name the brief states is more trustworthy than one
// derived from two letters, and the two can disagree.
func TestDecodeEventDetailsPrefersExplicitCountryOverCode(t *testing.T) {
	got, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "b-both",
		EventDetails: json.RawMessage(`{"name":"Event","country":"Japan","countryCode":"CN"}`),
	})
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.Country != "Japan" {
		t.Errorf("explicit country lost to the code: got %q, want %q", got.Country, "Japan")
	}
}

// An UNMAPPED code must still fail loudly. This is the property the original refusal protected:
// a code the region map does not know cannot become a HubSpot filter value, because it would
// match nobody and report success on an empty list.
func TestDecodeEventDetailsRejectsUnmappedCountryCode(t *testing.T) {
	if _, err := decodeEventDetails(&model.CampaignBrief{
		ID:           "b-xx",
		EventDetails: json.RawMessage(`{"name":"Event","countryCode":"XX"}`),
	}); err == nil {
		t.Fatal("an unmapped country code must fail rather than build an empty inclusion list")
	}
}
