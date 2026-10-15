// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package bootstrap

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/dispatch"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// TestAccountDiscoveryProvidersIsASubsetOfAccountListers pins the RELATIONSHIP between the two
// rosters rather than either membership, which is the part that stays true across tickets.
//
// This is the companion to TestAccountListerProseMatchesTheInterface in internal/dispatch, and
// together they are the structural answer to a comment that had been corrected three times and
// falsified three times. The split matters:
//
//   - AccountLister membership is a COMPILE-TIME fact, so the dispatch-side test derives it from
//     the interface and holds the chart and the knowledge docs to it.
//   - accountDiscoveryProviders membership additionally requires the second eligibility half —
//     "Dispatch itself calls the validator that tags ErrAccountNotSelected" — which is a
//     call-graph judgement no assertion can derive. Every dispatcher, Reddit included, mentions
//     that sentinel somewhere, so a grep could not tell LinkedIn before its create path joined
//     the resolver (tagged in a resolver Dispatch did not call) from X (whose Dispatch calls the
//     tagging validator itself).
//
// So the second half stays prose, and what is pinned here is the INVARIANT that survives it:
// holding both halves implies holding the first, hence subset.
//
// Deliberately NOT an equality assertion. The sets are unequal today — Reddit and LinkedIn
// implement AccountLister, hold both halves and are still excluded, which is a sequencing
// decision rather than a capability gap — and asserting equality would turn that judgement into
// a test failure every time discovery lands for a new provider. That is precisely the pressure
// that produced three successive hand-corrections of the roster prose.
//
// Both rosters are DERIVED, not hand-listed (issue #181). The candidate providers come from
// model.AllProviders() filtered by IsPaidAds(), so a paid-ads provider added later fails here
// until its dispatcher is in the table below rather than silently going unchecked; the lister
// roster is read off the method set; and the admitted roster is the map itself. The dispatcher
// table is the one hand-written part, and the coverage check is what holds it to the enum.
func TestAccountDiscoveryProvidersIsASubsetOfAccountListers(t *testing.T) {
	// nil dependencies: the assertion is about the METHOD SET, a compile-time property of the
	// type. Nothing is called, so nothing is dereferenced.
	dispatchers := map[model.Provider]any{
		model.ProviderGoogleAds:    dispatch.NewGoogleAdsDispatcher(nil, nil),
		model.ProviderMetaAds:      dispatch.NewMetaDispatcher(nil, nil),
		model.ProviderLinkedInAds:  dispatch.NewLinkedInDispatcher(nil, nil),
		model.ProviderMicrosoftAds: dispatch.NewMicrosoftDispatcher(nil, nil),
		model.ProviderTwitterAds:   dispatch.NewTwitterDispatcher(nil, nil),
		model.ProviderRedditAds:    dispatch.NewRedditDispatcher(nil, nil),
	}

	if len(accountDiscoveryProviders) == 0 {
		t.Fatal("accountDiscoveryProviders is empty — every check below would pass vacuously")
	}

	// Coverage, derived from the enum: every paid-ads provider must have its dispatcher here,
	// so the lister roster below is the WHOLE first half, not the part someone remembered.
	listers := map[model.Provider]bool{}
	for _, p := range model.AllProviders() {
		if !p.IsPaidAds() {
			continue
		}
		d, known := dispatchers[p]
		if !known {
			t.Errorf("%s is a paid-ads provider but this test does not know its dispatcher; "+
				"add it above so its AccountLister membership is derived rather than assumed", p)
			continue
		}
		if _, ok := d.(service.AccountLister); ok {
			listers[p] = true
		}
	}
	if len(listers) == 0 {
		t.Fatal("no paid-ads dispatcher satisfied service.AccountLister — the derivation itself " +
			"is broken, which would make the subset check below fail for the wrong reason")
	}

	for p := range accountDiscoveryProviders {
		if !p.IsPaidAds() {
			t.Errorf("%s is in accountDiscoveryProviders but is not a paid-ads provider: the map "+
				"means \"the ad account can be chosen later\", and requireAccountID already exempts "+
				"the email channel by classification", p)
			continue
		}
		if !listers[p] {
			t.Errorf("%s is in accountDiscoveryProviders — so the bootstrap CLI installs it with "+
				"no -account-id — but its dispatcher does NOT implement service.AccountLister, so "+
				"nothing in this API can tell an operator which account to choose. The row is "+
				"installable and dead, which is the state that map exists to prevent.", p)
		}
	}
}
