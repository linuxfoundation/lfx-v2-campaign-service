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
	// "event_specific", NOT "event": design/audience_builder.go declares
	// Enum("standard", "brand", "event_specific") and the UI's AudienceSuppressionCategory
	// matches it. Emitting "event" put every event-specific row outside the published enum,
	// so a consumer grouping on the generated contract dropped exactly the rows that outrank
	// the other two — the per-event suppression list is the highest-value exclusion here.
	SuppressionCategoryEvent = "event_specific"
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
	// HubSpotURL links to the list; empty when it no longer resolves.
	HubSpotURL string
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
	// ListsUnavailable marks a row whose selection could not be read. Without it the two empty
	// arrays are indistinguishable from a send that genuinely targeted nothing — false precedent.
	ListsUnavailable bool
}

// ComposeInput is what a master list is composed from.
type ComposeInput struct {
	ListIDs        []string
	ExcludeListIDs []string
	Name           string
	BrandShort     string
	EventName      string
	EventDates     []string
	// RecordUnderBriefID asks the caller's service layer to record the composed master as
	// this brief's built audience. It is empty for the exploratory use of the builder, which
	// composes lists without a campaign to attach them to.
	//
	// The orchestration itself records nothing — it only resolves the PORTAL when this is set,
	// because the portal must be read from the same build-scoped client the lists are created
	// with. Resolving it anywhere else would stamp provenance that does not provably describe
	// the ids it is attached to.
	RecordUnderBriefID string
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
	// PortalID is the HubSpot portal both lists were created in, read from the token of the
	// build-scoped client that created them. It is set only when ComposeInput.RecordUnderBriefID
	// was set, because it is only ever needed to stamp provenance on a recorded audience — and
	// the lookup is a network round trip the exploratory path should not pay for.
	PortalID string
	// AttachedSuppressionIDs are EXISTING suppression lists recorded beside the master by an
	// attach (AttachExisting), which composes no combined suppression of its own. Empty for a
	// compose, whose single suppression is Suppression above.
	AttachedSuppressionIDs []string
	// Attached marks an outcome that created nothing: the lists already existed and were
	// only verified. It changes the recorded row's default summary, nothing else.
	Attached bool
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
	// CurrentRegistrants is check 4, and is only populated when the caller supplied an event
	// name. Its zero value is an empty verdict, which CombineVerdicts ignores -- so an audit
	// that could not run neither passes nor fails the overall result.
	CurrentRegistrants Check
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
