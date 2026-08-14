// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"strings"
	"testing"
)

// Search and Demand Gen on the SAME brief must not compose the same budget name.
//
// This is the defect the channel split missed: only the CAMPAIGN name took the kind, while
// the budget stayed a bare "Budget". Every other ComposeName segment — Project, EventName,
// NameSuffix (the brief id) — is identical for two channels on one brief, so both composed
// byte-identical budget names.
//
// A non-shared budget's name is this client's idempotency key: a retry recomposes it and
// Google refuses with DUPLICATE_NAME. So Demand Gen on a brief that already had a Search
// campaign failed at the BUDGET step and never reached the campaign create at all — the
// user-visible symptom being that Demand Gen simply could not be created alongside Search.
func TestBudgetNamesDifferBetweenChannels(t *testing.T) {
	in := CampaignInput{
		Project:         "tlf",
		EventName:       "KubeCon EU 2026",
		NameSuffix:      "brief-1234", // the brief id: the SAME for both channels
		Budget:          500,
		RegistrationURL: "https://events.linuxfoundation.org/kubecon-eu-2026/",
	}

	// Through the REAL preflight, not ComposeName(budgetKindFor(...)) directly: calling the
	// helper is what the production path is SUPPOSED to do, so a test that calls it itself
	// passes even when preflight still uses the bare literal. Reverting the call site left
	// the direct-call version green -- it proved the helper works, not that anything uses it.
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	searchPf, err := c.preflightCampaignKind(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("preflight(search): %v", err)
	}
	demandGenPf, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err != nil {
		t.Fatalf("preflight(demand gen): %v", err)
	}
	searchBudget, demandGenBudget := searchPf.budgetName, demandGenPf.budgetName

	if searchBudget == demandGenBudget {
		t.Fatalf("both channels composed the same budget name %q — the second create fails with DUPLICATE_NAME at the budget step and never reaches the campaign create", searchBudget)
	}
}

// Search's budget name must NOT change. It is the idempotency key for every Search campaign
// already created upstream, whose budget carries the old name — a retry composing a new name
// would fail to collide and would create a SECOND budget for the same campaign.
//
// This is why the mapping is asymmetric, and this test is what stops a later "cleanup" from
// making it symmetric. It asserts the literal, not budgetKindFor's output, so a change to the
// function cannot move the goalposts with it.
func TestSearchBudgetNameIsUnchanged(t *testing.T) {
	in := CampaignInput{
		Project:         "tlf",
		EventName:       "KubeCon EU 2026",
		NameSuffix:      "brief-1234",
		Budget:          500,
		RegistrationURL: "https://events.linuxfoundation.org/kubecon-eu-2026/",
	}

	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	pf, err := c.preflightCampaignKind(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("preflight(search): %v", err)
	}
	got := pf.budgetName
	want := "LFX | Budget | tlf | KubeCon EU 2026 | brief-1234"
	if got != want {
		t.Errorf("Search budget name = %q, want %q — renaming it breaks DUPLICATE_NAME retry idempotency for every Search campaign already created upstream", got, want)
	}
}

// The budget and the campaign are separate entities with separate names, and Demand Gen's
// budget must not accidentally collide with its own campaign name either.
func TestDemandGenBudgetAndCampaignNamesAreDistinct(t *testing.T) {
	in := CampaignInput{
		Project:         "tlf",
		EventName:       "KubeCon EU 2026",
		NameSuffix:      "brief-1234",
		Budget:          500,
		RegistrationURL: "https://events.linuxfoundation.org/kubecon-eu-2026/",
	}

	budget := ComposeName(budgetKindFor(campaignKindDemandGen), in)
	campaign := ComposeName(campaignKindDemandGen, in)

	if budget == campaign {
		t.Errorf("Demand Gen budget and campaign share the name %q", budget)
	}
	// Both must still carry the brief id, which is what makes each deterministic per brief
	// and therefore usable as a retry key.
	for _, n := range []string{budget, campaign} {
		if !strings.Contains(n, "brief-1234") {
			t.Errorf("composed name %q lost the NameSuffix, so a retry cannot collide with it", n)
		}
	}
}
