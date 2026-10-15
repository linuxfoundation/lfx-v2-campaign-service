// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestNegativeKeywordPlatform_AdmitsExactlyTheSupportedFour pins the allowlist over EVERY
// provider, not just the ones that changed. Unlike the bid lever — which is reached by type
// assertion alone, so a dispatcher that implements it is served automatically — this list is
// hard-coded ahead of the orchestrator, so a platform whose adapter exists but whose entry is
// missing is refused with a 400 that says the capability does not exist.
func TestNegativeKeywordPlatform_AdmitsExactlyTheSupportedFour(t *testing.T) {
	for _, tc := range []struct {
		platform model.Provider
		want     bool
	}{
		{model.ProviderGoogleAds, true},
		{model.ProviderMicrosoftAds, true},
		{model.ProviderRedditAds, true},
		{model.ProviderTwitterAds, true},
		// No negative-keyword adapter: LinkedIn and Meta have no keyword surface at all, and
		// hubspot is an email channel.
		{model.ProviderLinkedInAds, false},
		{model.ProviderMetaAds, false},
		{model.ProviderHubSpot, false},
		{model.Provider("unknown"), false},
	} {
		if got := negativeKeywordPlatform(tc.platform); got != tc.want {
			t.Errorf("negativeKeywordPlatform(%q) = %t, want %t", tc.platform, got, tc.want)
		}
	}
}

// TestNegativeKeywordRefusal_NamesEveryAdmittedPlatform is the doc-vs-code contract on the one
// message a REFUSED caller reads. The sentence is hand-written next to the switch, so adding a
// platform to the allowlist and leaving the sentence alone tells every other platform's caller
// that the new one is not supported — which was true of Google Ads until this change, and is the
// exact drift this test exists to catch.
//
// The display names are their own vocabulary, NOT negativeKeywordPlatformLabel's output: that
// helper returns a lowercase mid-sentence fragment ("google ads", "x ads") for use inside the
// 404 and 409 messages, while this sentence capitalises and calls X's campaigns "X" rather than
// "X Ads". The table is therefore explicit, and the loop fails on an admitted platform that has
// no entry — which is what makes it catch the next platform added rather than only this one.
func TestNegativeKeywordRefusal_NamesEveryAdmittedPlatform(t *testing.T) {
	named := map[model.Provider]string{
		model.ProviderGoogleAds:    "Google Ads",
		model.ProviderMicrosoftAds: "Microsoft Advertising",
		model.ProviderRedditAds:    "Reddit",
		model.ProviderTwitterAds:   "X campaigns",
	}
	for _, platform := range []model.Provider{
		model.ProviderGoogleAds, model.ProviderLinkedInAds, model.ProviderMetaAds,
		model.ProviderRedditAds, model.ProviderTwitterAds, model.ProviderMicrosoftAds,
		model.ProviderHubSpot,
	} {
		if !negativeKeywordPlatform(platform) {
			continue
		}
		name, ok := named[platform]
		if !ok {
			t.Errorf("%q is admitted by the allowlist but this test names no display form for it; add it here and to the refusal sentence", platform)
			continue
		}
		if !strings.Contains(negativeKeywordUnsupportedMessage, name) {
			t.Errorf("the refusal message does not name %q (%s), so its callers are told a supported platform is not supported", platform, name)
		}
	}
}

// TestNegativeKeywordPlatformLabel_IsTheMidSentenceForm pins the OTHER vocabulary — the one the
// 404 and 409 messages interpolate. It is deliberately lowercase because it lands mid-sentence
// ("no google ads connection is configured for this project"), so a Title Case value here would
// read as a typo in every one of those messages.
func TestNegativeKeywordPlatformLabel_IsTheMidSentenceForm(t *testing.T) {
	for platform, want := range map[model.Provider]string{
		model.ProviderGoogleAds:    "google ads",
		model.ProviderMicrosoftAds: "microsoft advertising",
		model.ProviderRedditAds:    "reddit ads",
		model.ProviderTwitterAds:   "x ads",
	} {
		if got := negativeKeywordPlatformLabel(platform); got != want {
			t.Errorf("negativeKeywordPlatformLabel(%q) = %q, want %q", platform, got, want)
		}
	}
}
