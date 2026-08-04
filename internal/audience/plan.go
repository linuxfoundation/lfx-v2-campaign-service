// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Group identifies one inclusion list in the regional-expansion model. The numbering matches
// the operational runbook so a built audience can be reconciled against it by eye.
type Group int

const (
	// GroupEducationEnrolled is the runbook's group 4: education enrollees in the event's own
	// country. Fully deterministic — needs only the country.
	GroupEducationEnrolled Group = 4
	// GroupEventRegistered is group 5: registrants of this event's PAST editions in its own
	// country. Needs past edition names resolved from Snowflake.
	GroupEventRegistered Group = 5
	// GroupRegionRegistrants is group 7: registrants of those past editions across the whole
	// region, widening reach beyond the host country.
	GroupRegionRegistrants Group = 7
)

// String names the group as it appears in the runbook and in list names.
func (g Group) String() string {
	switch g {
	case GroupEducationEnrolled:
		return "Education Enrolled"
	case GroupEventRegistered:
		return "Event Registered"
	case GroupRegionRegistrants:
		return "Region Event Registrants"
	default:
		return fmt.Sprintf("Group(%d)", int(g))
	}
}

// PlannedList is one HubSpot list to create: its name and the filter tree defining membership.
type PlannedList struct {
	Group  Group
	Name   string
	Filter json.RawMessage
}

// Plan is the set of inclusion lists to build for an event, plus the human-readable provenance
// that will be stored on the audience record.
type Plan struct {
	// EventName scopes generated list names. HubSpot list names are PORTAL-GLOBAL, so the
	// runbook's bare "Education Enrolled [Country]" collides between two events in the same
	// country — and a collision does not fail loudly: it either rejects the create or points
	// the audience at another event's list.
	EventName string
	// BuildRef discriminates this build's lists from a previous build's (see PlanInput).
	BuildRef string
	Country  string
	Region   Region
	// PastEditions are the VERBATIM past-edition names resolved from Snowflake. Empty when the
	// event has no prior edition — a first-time event, which is normal, not an error.
	PastEditions []string
	Lists        []PlannedList
	// Notes record what was deliberately NOT built and why, so the stored InclusionSummary
	// explains gaps rather than leaving them to be rediscovered. Rendered under "Not included".
	Notes []string
	// Caveats record something about a list that WAS built — a qualification, not a gap. They
	// are kept separate from Notes because the two render in different sections and mixing them
	// inverts their meaning: a caveat filed under "Not included" tells an operator a group is
	// missing when it exists, which is the opposite of what it is trying to say.
	Caveats []string
}

// PlanInput is what the planner needs about an event.
type PlanInput struct {
	// EventName is the current event's name, used in generated list names.
	EventName string
	// BuildRef discriminates THIS build's lists from a previous build's for the same brief.
	// Several audiences per brief are supported (rebuilds, revised targeting), and HubSpot list
	// names are portal-global — without it a rebuild emits the exact same names and either the
	// create is rejected or the new audience silently adopts the OLD build's lists, so the
	// master would point at stale membership.
	//
	// The caller passes the audience row id, which is unique per build.
	BuildRef string
	// Country is the event's host country. Required: every inclusion list is country-scoped.
	Country string
	// PastEditions are past-edition names from snowflake.ResolvePastEventNames, used VERBATIM.
	// A caller MUST NOT substitute guessed or remembered names — HubSpot matches them exactly,
	// and a wrong name yields an empty list that is indistinguishable from a correct empty one.
	PastEditions []string
	// PastEditionsErr is set when the warehouse lookup FAILED, as distinct from succeeding and
	// finding none. Both leave PastEditions empty, but they mean opposite things to an operator:
	// "this event has no history" versus "we could not read its history". The stored
	// InclusionSummary is the durable record that outlives the logs, so it has to say which.
	PastEditionsErr error
	// EditionsUnnarrowed reports that the warehouse lookup ran WITHOUT a location predicate,
	// because the brief carried no location. ResolvePastEventNames then matches the event family
	// alone, so for a multi-city family ("Open Source Summit") the resolved editions can include
	// other cities' events.
	//
	// This is recorded, not prevented. The resolved names only ever appear ANDed with the host
	// country (group 5) or the host region (group 7), so a stray edition cannot email outside the
	// target geography — it widens the audience to prior attendees of the family who are now in
	// that geography, which is usually desirable and always geographically contained. Refusing to
	// build would instead discard a correct returning-event audience whenever `location` is blank,
	// which is the worse trade. The operator gets the facts and can set a location and rebuild.
	EditionsUnnarrowed bool
}

// BuildPlan derives the inclusion lists for an event.
//
// SCOPE — this mechanises only the DETERMINISTIC part of the runbook:
//   - group 4 (education enrolled, host country) always,
//   - group 5 (past-edition registrants, host country) when past editions were resolved,
//   - group 7 (those registrants region-wide) when the country maps to a known region.
//
// Deliberately NOT built: group 6 ("Expanded Web Visitors") requires choosing the NEAREST
// sibling event by date and brand-family proximity, and the domain-fit buckets
// (HARDWARE vs SOFTWARE/AI) require judgement about which course topics suit an event. Both
// are recorded in Notes as not-built rather than approximated — a plausible-looking wrong
// audience is worse than an absent one, because it sends real email to the wrong people.
func BuildPlan(in PlanInput) (*Plan, error) {
	country := strings.TrimSpace(in.Country)
	if country == "" {
		return nil, fmt.Errorf("audience: cannot plan an audience without the event's country")
	}
	eventName := strings.TrimSpace(in.EventName)
	if eventName == "" {
		return nil, fmt.Errorf("audience: cannot plan an audience without the event name")
	}

	// Canonicalize to the form HubSpot stores BEFORE building any filter. Aliases were
	// previously resolved only for region lookup, so a brief saying "USA" produced
	// country-scoped lists filtering on the literal "USA" — which matches nobody in HubSpot,
	// silently, while group 7 still widened correctly via the mapped region.
	country = DisplayName(country)

	p := &Plan{Country: country, EventName: eventName, BuildRef: strings.TrimSpace(in.BuildRef)}

	// Group 4 — always available: it needs only the country.
	eduFilter, err := EducationEnrolledFilter(country)
	if err != nil {
		return nil, err
	}
	p.Lists = append(p.Lists, PlannedList{
		Group:  GroupEducationEnrolled,
		Name:   p.listName(GroupEducationEnrolled.String(), country),
		Filter: eduFilter,
	})

	// Groups 5 and 7 depend on past editions. A first-time event legitimately has none; that is
	// a NOTE, not an error — the audience is still buildable from group 4 alone.
	editions := nonBlank(in.PastEditions)
	p.PastEditions = editions
	switch {
	case len(editions) == 0 && in.PastEditionsErr != nil:
		// The warehouse could not be read. This audience is NARROWER THAN INTENDED and should be
		// rebuilt once the lookup works — the opposite conclusion from a first-time event, so it
		// must not share that note.
		p.Notes = append(p.Notes,
			"Past editions could NOT be resolved (warehouse error: "+in.PastEditionsErr.Error()+"). "+
				"Groups 5 and 7 (past-edition registrants) were not built, so this audience is "+
				"NARROWER THAN INTENDED for a returning event — rebuild it once the lookup succeeds.")
	case len(editions) == 0:
		p.Notes = append(p.Notes,
			"No past editions resolved: groups 5 and 7 (past-edition registrants) were not built. "+
				"Expected for a first-time event; otherwise verify the event term used to query Snowflake.")
	default:
		if in.EditionsUnnarrowed {
			// The lookup had no location predicate, so for a multi-city family these editions may
			// span cities. This is a CAVEAT, not a gap: groups 5 and 7 were built. It renders
			// beside the resolved edition names it qualifies, because filing it under
			// "Not included" would say those groups are missing when they exist.
			p.Caveats = append(p.Caveats,
				"The brief carried no location, so past editions were matched on the event family "+
					"alone and may include OTHER CITIES' editions of it. Groups 5 and 7 remain scoped "+
					"to the host country/region, so this widens the audience to prior attendees of the "+
					"family who are in that geography rather than reaching outside it. Verify the "+
					"editions listed above; set the brief's location and rebuild to narrow them.")
		}
		regFilter, ferr := EventRegisteredFilter(country, editions)
		if ferr != nil {
			return nil, ferr
		}
		p.Lists = append(p.Lists, PlannedList{
			Group:  GroupEventRegistered,
			Name:   p.listName(GroupEventRegistered.String(), country),
			Filter: regFilter,
		})

		region, ok := RegionFor(country)
		if !ok {
			// Fail SOFT, not closed: the country-scoped lists above are already valid, so a
			// missing region mapping costs reach, not correctness. Recording it means an
			// operator can add the country rather than wondering why the audience is small.
			p.Notes = append(p.Notes, fmt.Sprintf(
				"Country %q is not in the region map, so group 7 (region-wide registrants) was not built. "+
					"Add it to the region map to widen reach.", country))
		} else {
			p.Region = region
			countries := CountriesIn(region)
			regionFilter, rerr := RegionEventRegistrantsFilter(countries, editions)
			if rerr != nil {
				return nil, rerr
			}
			p.Lists = append(p.Lists, PlannedList{
				Group:  GroupRegionRegistrants,
				Name:   p.listName(GroupRegionRegistrants.String(), string(region)),
				Filter: regionFilter,
			})
		}
	}

	// Always record the judgement-based groups as not-built, so the stored summary is explicit
	// about the boundary rather than implying the audience is complete.
	p.Notes = append(p.Notes,
		"Group 6 (Expanded Web Visitors) not built: it requires selecting the nearest sibling event "+
			"by date and brand family, which is a judgement call rather than a derivation.",
		"Domain-fit narrowing (HARDWARE vs SOFTWARE/AI course topics) not applied: it requires "+
			"judgement about which topics suit this event.")

	return p, nil
}

// InclusionSummary renders the plan as the human-readable provenance stored on the audience
// record — the part that is NOT visible from the HubSpot lists themselves.
func (p *Plan) InclusionSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Event: %s\n", p.EventName)
	fmt.Fprintf(&b, "Country: %s\n", p.Country)
	if p.Region != "" {
		fmt.Fprintf(&b, "Region: %s\n", p.Region)
	}
	if len(p.PastEditions) > 0 {
		fmt.Fprintf(&b, "Past editions (verbatim from Snowflake): %s\n", strings.Join(p.PastEditions, "; "))
	}
	// Caveats sit immediately after the editions they qualify and BEFORE the inclusion lists,
	// so the qualification is read alongside the names it is about. They must not fall into the
	// "Not included" section below: these describe lists that WERE built.
	if len(p.Caveats) > 0 {
		b.WriteString("\nCaveats (these lists WERE built, with qualifications):\n")
		for _, c := range p.Caveats {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	b.WriteString("\nInclusion lists:\n")
	for _, l := range p.Lists {
		fmt.Fprintf(&b, "  - [group %d] %s\n", int(l.Group), l.Name)
	}
	if len(p.Notes) > 0 {
		b.WriteString("\nNot included:\n")
		for _, n := range p.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	return b.String()
}

// listName builds a HubSpot list name. It keeps the runbook's readable
// "<group> <scope>" prefix but SUFFIXES the event, because HubSpot list names are
// portal-global: without the suffix, two events in the same country would both want
// "Education Enrolled Korea", and the second build would either be rejected or silently
// adopt the first event's list.
func (p *Plan) listName(group, scope string) string {
	name := fmt.Sprintf("%s %s — %s", group, scope, p.EventName)
	if p.BuildRef == "" {
		return name
	}
	// Short suffix: enough to disambiguate rebuilds without making the name unreadable in the
	// HubSpot UI, where an operator has to recognise it.
	ref := p.BuildRef
	if len(ref) > 8 {
		ref = ref[:8]
	}
	return name + " (" + ref + ")"
}

// MasterName is the master list's name, discriminated the same way as its inclusion lists.
func (p *Plan) MasterName() string {
	name := p.EventName + " — Master"
	if p.BuildRef == "" {
		return name
	}
	ref := p.BuildRef
	if len(ref) > 8 {
		ref = ref[:8]
	}
	return name + " (" + ref + ")"
}

// nonBlank drops blank entries, preserving order AND the original strings.
//
// It trims only to TEST emptiness. The values are authoritative Snowflake event names used
// VERBATIM as exact HubSpot filter values, so returning the trimmed form would silently alter
// the name — and a warehouse value with surrounding whitespace would then match nothing.
func nonBlank(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
