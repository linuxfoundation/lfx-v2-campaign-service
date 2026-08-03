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

// TestValidFromRequiresBothTableAndKind pins the coupling that the whole ChannelKind safety
// claim rests on: an unclassified provider must be INVALID, so it is rejected at the API
// boundary instead of silently taking a default branch.
//
// It drives the predicate directly rather than through a Provider constant. That is not
// indirection for its own sake — no real provider has a table without a kind (that is exactly
// the invariant being enforced), so a test written against the constants cannot construct the
// failing case and would still pass if Valid() stopped consulting Kind(). An earlier version
// of this test did precisely that and was vacuous.
func TestValidFromRequiresBothTableAndKind(t *testing.T) {
	cases := []struct {
		name  string
		table string
		kind  ChannelKind
		want  bool
	}{
		{"table and kind", "reddit_ads_connections", ChannelPaidAds, true},
		{"table and email kind", "hubspot_connections", ChannelEmail, true},
		// The case that matters: a provider was added with a Table() case but never
		// classified in Kind(). It must NOT be valid.
		{"table but unclassified", "brave_ads_connections", "", false},
		{"kind but no table", "", ChannelPaidAds, false},
		{"neither", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validFrom(tc.table, tc.kind); got != tc.want {
				t.Errorf("validFrom(%q, %q) = %v, want %v", tc.table, tc.kind, got, tc.want)
			}
		})
	}
}

// TestProviderValidityHoldsForEveryProvider checks the invariant end-to-end for the providers
// that actually exist: each is classified, and therefore valid.
func TestProviderValidityHoldsForEveryProvider(t *testing.T) {
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
