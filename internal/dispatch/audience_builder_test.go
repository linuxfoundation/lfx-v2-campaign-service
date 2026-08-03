// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/snowflake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingResolver captures the arguments the builder sends to the warehouse.
type recordingResolver struct {
	term, location, year string
	events               []snowflake.Event
}

func (r *recordingResolver) ResolvePastEventNames(_ context.Context, eventTerm, locationTerm, currentYear string) ([]snowflake.Event, error) {
	r.term, r.location, r.year = eventTerm, locationTerm, currentYear
	return r.events, nil
}

// TestResolvePastEditions_StripsTheYearFromTheSearchTerm pins the fix for a query that could
// never match. Event names normally CONTAIN their year ("KubeCon Korea 2026"), and the warehouse
// query is `ILIKE '%term%' AND NOT ILIKE '%year%'` — so passing the full name asks for rows
// containing 2026 that do not contain 2026. Sibling discovery silently returned zero for every
// returning event, degrading each one to a country-only audience.
func TestResolvePastEditions_StripsTheYearFromTheSearchTerm(t *testing.T) {
	r := &recordingResolver{}
	b := NewAudienceBuilder(nil, nil, r)

	_, err := b.ResolvePastEditions(context.Background(), "KubeCon Korea 2026", "Korea", "2026")
	require.NoError(t, err)

	assert.NotContains(t, r.term, "2026",
		"the search term must not contain the year it also excludes, or the query matches nothing")
	assert.Equal(t, "KubeCon Korea", r.term)
	assert.Equal(t, "2026", r.year)
}

// TestResolvePastEditions_DerivesTheYearFromTheEventName covers a brief whose details omit the
// year: it is recoverable from the name the brief already carries.
func TestResolvePastEditions_DerivesTheYearFromTheEventName(t *testing.T) {
	r := &recordingResolver{}
	b := NewAudienceBuilder(nil, nil, r)

	_, err := b.ResolvePastEditions(context.Background(), "KubeCon Korea 2026", "Korea", "")
	require.NoError(t, err)
	assert.Equal(t, "2026", r.year, "the year must come from the event, not the wall clock")
	assert.Equal(t, "KubeCon Korea", r.term)
}

// TestResolvePastEditions_NoYearMeansNoEditions pins the degrade. The warehouse query EXCLUDES
// names containing the supplied year, so a wall-clock fallback drops the wrong edition — on a
// 2027 brief read in 2026 it omits the 2026 edition, and on an older brief it lets that brief's
// OWN edition through as "past". Without a real year, return nothing and let the caller record
// a country-only audience.
func TestResolvePastEditions_NoYearMeansNoEditions(t *testing.T) {
	r := &recordingResolver{events: []snowflake.Event{{EventName: "should not be reached"}}}
	b := NewAudienceBuilder(nil, nil, r)

	names, err := b.ResolvePastEditions(context.Background(), "Some Event", "Korea", "")
	require.NoError(t, err, "a missing year degrades, it does not fail")
	assert.Empty(t, names)
	assert.Empty(t, r.year, "the warehouse must not be queried with a guessed year")
}

// TestResolvePastEditions_PreservesWarehouseNamesVerbatim pins that the authoritative name is
// NOT normalized. It is used as an EXACT HubSpot filter value, so trimming it would silently
// change the name and could match nothing.
func TestResolvePastEditions_PreservesWarehouseNamesVerbatim(t *testing.T) {
	r := &recordingResolver{events: []snowflake.Event{
		{EventName: "  KubeCon Korea 2025  "},
		{EventName: "   "}, // whitespace-only: dropped, not trimmed into existence
		{EventName: "KubeCon Korea 2024"},
	}}
	b := NewAudienceBuilder(nil, nil, r)

	names, err := b.ResolvePastEditions(context.Background(), "KubeCon Korea 2026", "", "2026")
	require.NoError(t, err)
	assert.Equal(t, []string{"  KubeCon Korea 2025  ", "KubeCon Korea 2024"}, names,
		"warehouse names must survive verbatim, including surrounding whitespace")
}

func TestYearIn(t *testing.T) {
	cases := map[string]string{
		"KubeCon Korea 2026": "2026",
		"2024 Open Summit":   "2024",
		"No year here":       "",
		"Event 123456":       "", // a longer digit run is not a year
		"Event 999":          "",
	}
	for in, want := range cases {
		assert.Equal(t, want, yearIn(in), "input %q", in)
	}
}
