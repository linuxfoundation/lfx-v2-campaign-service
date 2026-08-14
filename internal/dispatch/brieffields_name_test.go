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

// A WHITESPACE-ONLY `eventName` must not block the `name` fallback. Emptiness here has to be
// semantic, matching the TrimSpace validation at the end of decodeBriefFields: with a plain
// `== ""` test, `{"eventName":" ", "name":"…"}` skipped the fallback and was then rejected —
// a perfectly usable name discarded because the other key held a space.
func TestDecodeBriefFieldsTreatsWhitespaceEventNameAsAbsent(t *testing.T) {
	brief := &model.CampaignBrief{
		ID:           "b-3",
		URL:          "https://example.com/",
		EventDetails: json.RawMessage(`{"eventName":"   ","name":"Valid UI Name"}`),
	}

	bf, err := decodeBriefFields(brief)
	if err != nil {
		t.Fatalf("decodeBriefFields: %v", err)
	}
	if bf.EventName != "Valid UI Name" {
		t.Errorf("EventName = %q, want %q — a whitespace-only eventName must fall back to name", bf.EventName, "Valid UI Name")
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

// The value becomes the upstream campaign NAME, so it must be trimmed on assignment rather
// than only checked for emptiness. Raised by @dealako: decodeEventDetails trims on assignment
// and this one did not, so the two decoders disagreed on the same blob — "  Foo  " here vs
// "Foo" there — and the PR documents them as siblings to keep in step.
func TestDecodeBriefFieldsTrimsTheStoredEventName(t *testing.T) {
	for name, blob := range map[string]string{
		"explicit spelling": `{"eventName":"  KubeCon EU 2026  "}`,
		"ui spelling":       `{"name":"  KubeCon EU 2026  "}`,
	} {
		t.Run(name, func(t *testing.T) {
			bf, err := decodeBriefFields(&model.CampaignBrief{
				ID:           "b-trim",
				URL:          "https://example.com/",
				EventDetails: json.RawMessage(blob),
			})
			if err != nil {
				t.Fatalf("decodeBriefFields: %v", err)
			}
			if bf.EventName != "KubeCon EU 2026" {
				t.Errorf("EventName = %q, want it trimmed — this value becomes the upstream campaign name", bf.EventName)
			}
		})
	}
}

// lenientEventName labels the cloned email. It was lenient about PRESENCE (returns "" rather
// than erroring, so an email still stages without a name) but not about SPELLING, so it missed
// the UI's `name` and every cloned email fell back to the slug/brief-id label even when the
// brief carried a perfectly good name. Raised by @dealako, whose point was that the PR's
// comment implied this path was already covered — it was not.
func TestLenientEventNameReadsTheUISpelling(t *testing.T) {
	got := lenientEventName(&model.CampaignBrief{
		ID:           "b-1",
		EventDetails: json.RawMessage(`{"name":"  Agntcon Mcpcon Japan  ","countryCode":"US"}`),
	})
	if got != "Agntcon Mcpcon Japan" {
		t.Errorf("lenientEventName = %q, want the trimmed UI `name` — otherwise the cloned email is labelled from the fallback", got)
	}
}

// The explicit spelling still wins, and absence still yields "" rather than an error — the
// leniency this function has always had, which email staging depends on.
func TestLenientEventNamePrecedenceAndAbsence(t *testing.T) {
	if got := lenientEventName(&model.CampaignBrief{
		EventDetails: json.RawMessage(`{"eventName":"Explicit","name":"Generic"}`),
	}); got != "Explicit" {
		t.Errorf("got %q, want the explicit `eventName` to win", got)
	}
	if got := lenientEventName(&model.CampaignBrief{
		EventDetails: json.RawMessage(`{"city":"Tokyo"}`),
	}); got != "" {
		t.Errorf("got %q, want \"\" — a nameless brief must still stage its email", got)
	}
}
