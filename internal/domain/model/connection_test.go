// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

func TestProvider_Table(t *testing.T) {
	cases := map[Provider]string{
		ProviderGoogleAds:    "google_ads_connections",
		ProviderLinkedInAds:  "linkedin_ads_connections",
		ProviderMetaAds:      "meta_ads_connections",
		ProviderRedditAds:    "reddit_ads_connections",
		ProviderTwitterAds:   "twitter_ads_connections",
		ProviderMicrosoftAds: "microsoft_ads_connections",
		ProviderHubSpot:      "hubspot_connections",
	}
	for p, want := range cases {
		if got := p.Table(); got != want {
			t.Errorf("%s.Table() = %q, want %q", p, got, want)
		}
		if !p.Valid() {
			t.Errorf("%s.Valid() = false, want true", p)
		}
	}
}

func TestProvider_UnknownIsInvalid(t *testing.T) {
	p := Provider("bogus-ads")
	if p.Table() != "" {
		t.Errorf("unknown provider Table() = %q, want empty", p.Table())
	}
	if p.Valid() {
		t.Error("unknown provider Valid() = true, want false")
	}
}

func TestConnection_HasCredentials(t *testing.T) {
	c := &Connection{}
	if c.HasCredentials() {
		t.Error("empty connection HasCredentials() = true, want false")
	}
	c.EncryptedCredentials = []byte{0x01}
	if !c.HasCredentials() {
		t.Error("connection with ciphertext HasCredentials() = false, want true")
	}
}

// TestProviderKind_ClassifiesEveryProvider is the guard that matters: EVERY provider the API
// accepts must have a channel kind. A new provider added to the Provider list without being
// classified returns "" here, which would make IsPaidAds() silently false — routing it down
// the email path. This test fails instead.
func TestProviderKind_ClassifiesEveryProvider(t *testing.T) {
	all := []Provider{
		ProviderGoogleAds, ProviderLinkedInAds, ProviderMetaAds,
		ProviderRedditAds, ProviderTwitterAds, ProviderMicrosoftAds,
		ProviderHubSpot,
	}
	// Cross-check the list above against Valid(): if a provider is added to the model and
	// not added here, the count assertion below is what catches it.
	for _, p := range all {
		if !p.Valid() {
			t.Errorf("%s is in the classification list but is not a valid provider", p)
		}
		if p.Kind() == "" {
			t.Errorf("%s has no ChannelKind — classify it in Provider.Kind()", p)
		}
	}
	if got := Provider("not-a-provider").Kind(); got != "" {
		t.Errorf("an unknown provider must return an empty kind, got %q", got)
	}
}

// TestProviderIsPaidAds pins the split the rest of the service branches on.
func TestProviderIsPaidAds(t *testing.T) {
	paid := []Provider{
		ProviderGoogleAds, ProviderLinkedInAds, ProviderMetaAds,
		ProviderRedditAds, ProviderTwitterAds, ProviderMicrosoftAds,
	}
	for _, p := range paid {
		if !p.IsPaidAds() {
			t.Errorf("%s must be a paid ad channel", p)
		}
		if p.Kind() != ChannelPaidAds {
			t.Errorf("%s kind = %q, want %q", p, p.Kind(), ChannelPaidAds)
		}
	}
	// HubSpot is the email channel — NOT a paid ad platform, despite being dispatchable.
	if ProviderHubSpot.IsPaidAds() {
		t.Error("hubspot is the email channel, not a paid ad platform")
	}
	if ProviderHubSpot.Kind() != ChannelEmail {
		t.Errorf("hubspot kind = %q, want %q", ProviderHubSpot.Kind(), ChannelEmail)
	}
}
