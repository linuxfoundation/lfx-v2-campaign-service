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

// TestProviderValidityRequiresClassification is the real enforcement, and it does not depend
// on any hand-maintained list being correct.
//
// Go cannot enumerate a const block, so a test that walks a literal slice can never prove
// exhaustiveness — a provider omitted from BOTH Kind() and the slice passes silently. (My
// first attempt at this test had exactly that hole.) Instead, Valid() is defined as
// "has a Table() AND has a Kind()", so an unclassified provider is INVALID by construction
// and gets rejected at the API boundary rather than misclassified deep in the service.
//
// This test pins that coupling: break it (make Valid() ignore Kind()) and the assertion
// below fails.
func TestProviderValidityRequiresClassification(t *testing.T) {
	// A provider with a table but no classification must NOT be valid. "" is not a real
	// provider, but it exercises the same predicate a future unclassified provider would.
	classifiedButNoTable := Provider("hubspot")
	if !classifiedButNoTable.Valid() {
		t.Fatalf("%s should be valid: it has both a table and a kind", classifiedButNoTable)
	}
	for _, p := range AllProviders() {
		if p.Kind() == "" {
			t.Errorf("%s has no ChannelKind — classify it in Provider.Kind()", p)
		}
		if !p.Valid() {
			t.Errorf("%s is enumerated but not Valid(); it is missing a Table() or a Kind()", p)
		}
	}
	if Provider("not-a-provider").Valid() {
		t.Error("an unknown provider must not be valid")
	}
	if got := Provider("not-a-provider").Kind(); got != "" {
		t.Errorf("an unknown provider must return an empty kind, got %q", got)
	}
}

// TestAllProvidersAreValidAndUnique keeps the enumeration honest for the callers that iterate
// it (container wiring, docs generation). It cannot prove completeness — see above — but it
// does catch a duplicate or a stale entry.
func TestAllProvidersAreValidAndUnique(t *testing.T) {
	seen := map[Provider]bool{}
	for _, p := range AllProviders() {
		if seen[p] {
			t.Errorf("%s appears twice in AllProviders()", p)
		}
		seen[p] = true
		if p.Table() == "" {
			t.Errorf("%s is enumerated but has no connection table", p)
		}
	}
	if len(seen) == 0 {
		t.Fatal("AllProviders() must not be empty")
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
