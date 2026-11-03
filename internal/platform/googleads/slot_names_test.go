// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"net/url"
	"testing"
)

// AC 18 / N1: the utm_campaign fallback for an input with neither an event slug nor an event
// name. A second campaign's NameSuffix carries its slot ("brief-1-2"), but utm_campaign is
// event-level attribution, so the fallback stays the bare brief id the dispatcher passes as
// UTMCampaignFallback. An input that sets no UTMCampaignFallback keeps the pre-slot behaviour
// (NameSuffix), and a slug, when set, always wins.
func TestUTMCampaignFallbackIsTheBareBriefID(t *testing.T) {
	for _, tc := range []struct {
		name           string
		slug, event    string
		suffix, utmFal string
		want           string
	}{
		{"legacy input, no fallback field: NameSuffix, as before", "", "", "brief-1", "", "brief-1"},
		{"slot 1 from the dispatcher", "", "", "brief-1", "brief-1", "brief-1"},
		{"slot 2 from the dispatcher: still the bare brief id", "", "", "brief-1-2", "brief-1", "brief-1"},
		{"slug set, slot 1", "kubecon-na-2026", "", "brief-1", "brief-1", "kubecon-na-2026"},
		{"slug set, slot 2", "kubecon-na-2026", "", "brief-1-2", "brief-1", "kubecon-na-2026"},
		{"event name, no slug, slot 2", "", "KubeCon", "brief-1-2", "brief-1", "KubeCon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sampleInput()
			in.EventSlug, in.EventName, in.NameSuffix, in.UTMCampaignFallback = tc.slug, tc.event, tc.suffix, tc.utmFal
			// Caller copy, so an input without an event name still yields a valid ad.
			in.Headlines = []string{"Join the community", "Register today", "Cloud native event"}
			in.Descriptions = []string{"Three days of talks and workshops.", "Meet maintainers and end users."}
			finalURL, _, _, _, _, err := precomputeAdGroupAdInputs(in)
			if err != nil {
				t.Fatalf("precompute: %v", err)
			}
			u, err := url.Parse(finalURL)
			if err != nil {
				t.Fatalf("final URL %q: %v", finalURL, err)
			}
			if got := u.Query().Get("utm_campaign"); got != tc.want {
				t.Errorf("utm_campaign = %q, want %q", got, tc.want)
			}
		})
	}
}

// The composer's label segments (cs-2): a labelled input puts the stage token and the tactic in
// their own segments before the suffix; an unlabelled one composes the legacy 5-segment name.
// Both are sanitized like every other segment, so a pipe cannot inject a field.
func TestComposeName_LabelSegments(t *testing.T) {
	in := CampaignInput{Project: "cncf", EventName: "KubeCon", NameSuffix: "brief-1-2"}
	if got, want := ComposeName(CampaignKindSearch, in), "LFX | Search Campaign | cncf | KubeCon | brief-1-2"; got != want {
		t.Errorf("unlabelled = %q, want %q", got, want)
	}
	in.StageToken, in.Tactic = "MoFU", "retargeting"
	if got, want := ComposeName(CampaignKindSearch, in), "LFX | Search Campaign | cncf | KubeCon | MoFU | retargeting | brief-1-2"; got != want {
		t.Errorf("labelled = %q, want %q", got, want)
	}
	in.StageToken, in.Tactic = "Mo|FU", " keyword | x "
	if got, want := ComposeName(CampaignKindSearch, in), "LFX | Search Campaign | cncf | KubeCon | Mo FU | keyword x | brief-1-2"; got != want {
		t.Errorf("pipe-injected = %q, want %q", got, want)
	}
}

// The sitelink destination reads the same fallback as the ad's final URL (#456 review): a slot-2
// input with no event slug or name still tags its sitelinks with the bare brief id.
func TestValidateSitelinks_UTMCampaignFallbackIsTheBareBriefID(t *testing.T) {
	in := sampleInput()
	in.EventSlug, in.EventName, in.NameSuffix, in.UTMCampaignFallback = "", "", "brief-1-2", "brief-1"
	in.Sitelinks = []Sitelink{sampleSitelink()}
	got, err := validateSitelinks(in)
	if err != nil {
		t.Fatalf("validateSitelinks: %v", err)
	}
	if len(got) != 1 || len(got[0].FinalURLs) != 1 {
		t.Fatalf("got %+v, want one sitelink with one final URL", got)
	}
	u, err := url.Parse(got[0].FinalURLs[0])
	if err != nil {
		t.Fatalf("sitelink URL %q: %v", got[0].FinalURLs[0], err)
	}
	if c := u.Query().Get("utm_campaign"); c != "brief-1" {
		t.Errorf("sitelink utm_campaign = %q, want the bare brief id", c)
	}
}
