// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func groupsOf(p *Plan) []Group {
	var out []Group
	for _, l := range p.Lists {
		out = append(out, l.Group)
	}
	return out
}

// TestBuildPlan_FullyResolvedEvent covers the normal case: a returning event in a mapped
// country builds all three deterministic groups.
func TestBuildPlan_FullyResolvedEvent(t *testing.T) {
	p, err := BuildPlan(PlanInput{
		EventName:    "KubeCon Korea 2026",
		Country:      "South Korea",
		PastEditions: []string{"KubeCon Korea 2025", "KubeCon Korea 2024"},
	})
	require.NoError(t, err)

	assert.Equal(t, []Group{GroupEducationEnrolled, GroupEventRegistered, GroupRegionRegistrants}, groupsOf(p))
	assert.Equal(t, RegionAPAC, p.Region)
	assert.Equal(t, []string{"KubeCon Korea 2025", "KubeCon Korea 2024"}, p.PastEditions)

	for _, l := range p.Lists {
		assert.NotEmpty(t, l.Filter, "every planned list must carry a filter tree")
		// HubSpot list names are portal-global, so each must be scoped to this event.
		assert.Contains(t, l.Name, "KubeCon Korea 2026",
			"list names must be event-scoped or two events in one country collide")
	}
}

// TestBuildPlan_FirstTimeEventStillBuilds pins that a first-time event is NOT an error. It has
// no past editions, so groups 5 and 7 are impossible — but group 4 is still valid, and refusing
// to build would leave the HubSpot dispatcher permanently unable to send for new events.
func TestBuildPlan_FirstTimeEventStillBuilds(t *testing.T) {
	p, err := BuildPlan(PlanInput{EventName: "Brand New Summit 2026", Country: "Japan"})
	require.NoError(t, err)

	assert.Equal(t, []Group{GroupEducationEnrolled}, groupsOf(p))
	assert.Empty(t, p.PastEditions)

	joined := strings.Join(p.Notes, "\n")
	assert.Contains(t, joined, "No past editions resolved",
		"the gap must be recorded, not silently omitted")
}

// TestBuildPlan_WarehouseErrorGetsTheOutageNote pins the note that distinguishes the two empty
// results. "No past editions" and "could not read past editions" both leave PastEditions empty
// but mean OPPOSITE things to an operator: the first is expected and final, the second means the
// audience is narrower than intended and must be rebuilt. The log line rotates away; this note is
// the durable record, so it has to say which one happened.
func TestBuildPlan_WarehouseErrorGetsTheOutageNote(t *testing.T) {
	p, err := BuildPlan(PlanInput{
		EventName:       "KubeCon Korea 2026",
		Country:         "South Korea",
		PastEditionsErr: errors.New("snowflake is configured but unusable: bad private key"),
	})
	require.NoError(t, err, "a warehouse outage must still yield the country-only audience")

	assert.Equal(t, []Group{GroupEducationEnrolled}, groupsOf(p))

	joined := strings.Join(p.Notes, "\n")
	assert.Contains(t, joined, "NARROWER THAN INTENDED",
		"an operator must be told to rebuild; without this the audience looks final")
	assert.Contains(t, joined, "bad private key",
		"the underlying cause must survive into the record, not just the boot log")
	assert.NotContains(t, joined, "Expected for a first-time event",
		"a warehouse outage must NOT borrow the first-time-event note: that note tells the "+
			"operator nothing is wrong, which is the exact wrong conclusion here")
}

// TestBuildPlan_UnmappedCountryFailsSoft pins the deliberate asymmetry: an unknown country
// costs REACH (no region-wide list) but not correctness, because the country-scoped lists are
// still valid. Failing the whole build would be worse than building a narrower audience.
func TestBuildPlan_UnmappedCountryFailsSoft(t *testing.T) {
	p, err := BuildPlan(PlanInput{
		EventName:    "Edge Summit 2026",
		Country:      "Atlantis",
		PastEditions: []string{"Edge Summit 2025"},
	})
	require.NoError(t, err, "an unmapped country must not fail the build")

	assert.Equal(t, []Group{GroupEducationEnrolled, GroupEventRegistered}, groupsOf(p))
	assert.Empty(t, p.Region, "no region may be guessed for an unmapped country")
	assert.Contains(t, strings.Join(p.Notes, "\n"), "not in the region map")
}

// TestBuildPlan_AlwaysRecordsJudgementGaps pins that the summary never implies completeness.
// Group 6 and domain-fit narrowing are judgement calls that are deliberately not mechanised,
// and an operator reading the audience record must be able to see that.
func TestBuildPlan_AlwaysRecordsJudgementGaps(t *testing.T) {
	p, err := BuildPlan(PlanInput{
		EventName:    "KubeCon Korea 2026",
		Country:      "South Korea",
		PastEditions: []string{"KubeCon Korea 2025"},
	})
	require.NoError(t, err)

	joined := strings.Join(p.Notes, "\n")
	assert.Contains(t, joined, "Group 6")
	assert.Contains(t, joined, "Domain-fit")

	summary := p.InclusionSummary()
	assert.Contains(t, summary, "Not included:", "the summary must state what was NOT built")
	assert.Contains(t, summary, "KubeCon Korea 2025", "past editions belong in the provenance")
	assert.Contains(t, summary, "APAC")
}

// TestBuildPlan_RequiresCountryAndEventName pins the two inputs with no safe default. A missing
// country cannot be inferred, and a missing event name would produce colliding list names.
func TestBuildPlan_RequiresCountryAndEventName(t *testing.T) {
	_, err := BuildPlan(PlanInput{EventName: "Some Event"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "country")

	_, err = BuildPlan(PlanInput{Country: "Japan"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "event name")
}

// TestBuildPlan_DropsBlankEditions guards against a blank name reaching HubSpot, where it would
// match nobody and silently shrink the audience.
func TestBuildPlan_DropsBlankEditions(t *testing.T) {
	p, err := BuildPlan(PlanInput{
		EventName:    "KubeCon Korea 2026",
		Country:      "South Korea",
		PastEditions: []string{"  ", "KubeCon Korea 2025", ""},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"KubeCon Korea 2025"}, p.PastEditions)
}

// TestBuildPlan_BuildRefDisambiguatesRebuilds pins that two builds of the SAME brief do not
// produce identical list names. Several audiences per brief are supported, and HubSpot list
// names are portal-global — without a discriminator a rebuild either has its create rejected or
// silently adopts the previous build's lists, leaving the master pointing at stale membership.
func TestBuildPlan_BuildRefDisambiguatesRebuilds(t *testing.T) {
	in := PlanInput{EventName: "KubeCon Korea 2026", Country: "South Korea", PastEditions: []string{"KubeCon Korea 2025"}}

	in.BuildRef = "aud-11111111-aaaa"
	first, err := BuildPlan(in)
	require.NoError(t, err)

	in.BuildRef = "aud-22222222-bbbb"
	second, err := BuildPlan(in)
	require.NoError(t, err)

	require.Equal(t, len(first.Lists), len(second.Lists))
	for i := range first.Lists {
		assert.NotEqual(t, first.Lists[i].Name, second.Lists[i].Name,
			"two builds of one brief must not emit the same list name")
	}
	assert.NotEqual(t, first.MasterName(), second.MasterName(), "the master name must differ too")

	// The event still leads the name, so an operator can recognise it in the HubSpot UI.
	assert.Contains(t, first.Lists[0].Name, "KubeCon Korea 2026")
	assert.Contains(t, first.MasterName(), "KubeCon Korea 2026")

	// With no BuildRef the names stay clean (the first build for a brief).
	in.BuildRef = ""
	plain, err := BuildPlan(in)
	require.NoError(t, err)
	assert.NotContains(t, plain.Lists[0].Name, "(")
}
