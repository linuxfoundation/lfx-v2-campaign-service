// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

// ---------------------------------------------------------------------------
// Transport-neutral results (LFXV2-2770)
//
// What the orchestration in internal/dispatch returns and the Goa handlers map to
// the wire. They are defined here rather than in either of those packages so the
// orchestration can be tested against them without the generated types, and so a
// change to the generated contract cannot silently change what the orchestration
// promises.
//
// `Size` is a *int64 throughout for one reason: HubSpot does not report a size on
// every endpoint, and an absent size is not zero. Rendering "0 contacts" for a list
// whose size was never returned tells an operator the list is empty when it may hold
// fifty thousand people.
// ---------------------------------------------------------------------------

// ExploreCapabilities reports whether this project can do audience work at all.
//
// Detail is non-empty exactly when HubSpotConfigured is false, and it is written for
// an operator rather than a log: the tab's whole degraded state is this one sentence,
// so "no active HubSpot connection for this project" has to be actionable as it
// stands.
type ExploreCapabilities struct {
	HubSpotConfigured bool
	Detail            string
}

// ListRow is a list as a picker shows it — the shape shared by typeahead hits and
// existing-master rows.
type ListRow struct {
	ListID     string
	Name       string
	Size       *int64
	HubSpotURL string
}

// DiscoveredList is a classified candidate: the list, plus WHY discovery believes it
// belongs in this event's audience.
//
// Reason is not decoration. Discovery infers a signal from filter shapes and naming
// convention, and an operator about to mail ten thousand people is entitled to see
// the evidence rather than a bare label — a wrong `event_registration` is obvious
// once its reason is read.
type DiscoveredList struct {
	ListRow
	Signal   Signal
	Reason   string
	ListType string
	// Scope is set only for SignalEventSpeakers, where "current", "past" and
	// "current + past" are genuinely different audiences.
	Scope SpeakerScope
}

// DiscoveryOutcome is one discovery run.
//
// Inspected is reported so a capped run is visible as capped. Discovery stops after
// DiscoveryMaxInspections candidates, and a silent cap looks exactly like a portal
// that holds nothing more.
type DiscoveryOutcome struct {
	Event          EventIdentity
	Lists          []DiscoveredList
	MissingSignals []Signal
	Inspected      int
}

// SuppressionRow is one row of the suppression picker.
//
// Key is a stable identifier for the ROW, not the list: the standard rows keep their
// key even when nothing in the portal resolves for them, so an unresolved row can be
// shown as unavailable rather than omitted. A silently missing GDPR row is a
// suppression an operator never learns is unavailable.
type SuppressionRow struct {
	Key        string
	Label      string
	ListID     string
	Name       string
	Size       *int64
	Category   string
	HubSpotURL string
}

// Suppression row categories, mirroring the picker's groups.
const (
	SuppressionCategoryStandard = "standard"
	SuppressionCategoryBrand    = "brand"
	SuppressionCategoryEvent    = "event"
)

// ListBrief names a list referenced from somewhere else — a prior send's include or
// suppression selection.
//
// Missing is carried explicitly because a deleted list still leaves its id in the
// email's record. Dropping the row would silently understate what the last send
// targeted; showing the id with Missing set says what actually happened.
type ListBrief struct {
	ListID               string
	Name                 string
	Size                 *int64
	Missing              bool
	ResolvedFromLegacyID string
}

// LastSentEmail is a prior send and the lists it used — the best available precedent
// for what this event's audience should be.
type LastSentEmail struct {
	EmailID          string
	EmailName        string
	SentAt           string
	HubSpotURL       string
	IncludedLists    []ListBrief
	SuppressionLists []ListBrief
}

// ComposeInput is what a master list is composed from.
type ComposeInput struct {
	ListIDs        []string
	ExcludeListIDs []string
	Name           string
	BrandShort     string
	EventName      string
	EventDates     []string
}

// ComposedList is a list this service just created.
type ComposedList struct {
	ListRow
}

// ComposeOutcome is a successful composition: the master, and the suppression wrapper
// it excludes when exclusions were requested.
//
// SourceListIDs echoes the inclusions actually used, which is not always what was
// requested — ExclusionIDs resolves an id selected both ways in favour of inclusion,
// and an operator reading the result needs to see which ids the master really unions.
type ComposeOutcome struct {
	Master        ComposedList
	Suppression   *ComposedList
	SourceListIDs []string
}

// QaCandidate is one of several lists a typed name matched.
type QaCandidate struct {
	ListID string
	Name   string
	Size   *int64
}

// QaChecks groups the three audits.
type QaChecks struct {
	SignalMapping         Check
	Suppression           SuppressionCheck
	ExclusionCompleteness ExclusionCheck
}

// QaOutcome is a QA run, or the disambiguation that prevented one.
//
// A discriminated result rather than a partly-filled one: when NeedsDisambiguation is
// true there is no audited list and no verdict, and a shape that carried a zero-valued
// verdict alongside the candidates could be read as a PASS on a list QA never looked
// at.
type QaOutcome struct {
	NeedsDisambiguation bool
	Candidates          []QaCandidate

	ListID     string
	Name       string
	HubSpotURL string
	Checks     QaChecks
	Findings   []Finding
	Overall    Verdict
}
