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
