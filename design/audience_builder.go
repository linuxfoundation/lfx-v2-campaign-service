// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — campaign audience-builder endpoints.
//
// These endpoints are the EXPLORATORY half of audience work, and they are deliberately
// separate from the audiences service in design/audience.go. That service manages the
// built-audience RECORD (a pointer + provenance under a brief, epic LFXV2-2770); this one
// answers "what could this audience be made of?" against the project's live HubSpot
// portal: which existing contact lists reference the event, which suppression lists
// apply, what a previous send targeted, how large the union would be, and — once the
// operator has reviewed all of that — creating the master list.
//
// Two consequences of that split shaped the DSL below.
//
// Scope is the PROJECT, not the brief. HubSpot credentials are stored per project as
// encrypted connections (internal/dispatch/creds.go), so every route needs a project_id
// to resolve a portal at all — but the builder is used BEFORE a brief's audience record
// exists, so requiring a brief_id would make the first, exploratory call impossible.
// compose-audience-master therefore creates lists and returns their ids; recording them
// against a brief is a separate create-audience/update-audience call by the caller.
//
// Nothing here streams. The UI renders discovery as a progress ticker and the LFX One BFF
// is what serves that as Server-Sent Events; Goa's HTTP transport has no SSE encoding, so
// discover-audience-lists is one synchronous request/response and the BFF adapts it.
// Keeping the adaptation in the BFF — rather than inventing a job_id + poll pair here —
// leaves this service's contract ordinary and the UI's event shape unchanged.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// ─── Shared audience-builder vocabularies ───

// audienceSignalEnum is the nine-value vocabulary for WHY an existing HubSpot list
// qualifies a contact for this event's audience. Seven are classifications the discovery
// pass assigns from a list's filterBranch; last_sent marks a list recovered from a
// previous send that matched no signal, and added marks one the operator attached by
// hand. They share one enum because a card carries exactly one of them.
//
// uncertain is a first-class value, NOT an error state. A list that references the event
// but whose filterBranch does not commit to a signal must surface for review rather than
// be dropped or guessed into a bucket — a wrongly classified list silently changes who
// receives an email, which is the failure the review step exists to prevent.
func audienceSignalEnum() {
	Enum(
		"last_sent",
		"event_registration",
		"event_speakers",
		"project_opt_in",
		"lf_newsletter_opt_in",
		"education_enrollment",
		"page_view",
		"uncertain",
		"added",
	)
}

// audienceSpeakerScopeEnum records which edition(s) of a recurring event a speaker list
// covers. Only meaningful when the signal is event_speakers: a "thanks for speaking"
// email targets past speakers and a logistics email current ones, so the scopes are
// independently selectable rather than one bucket.
//
// current_past is the safe default for an undeterminable list — it is a superset of
// either narrow scope, so an unknown list over-includes (visible, correctable) rather
// than silently dropping the speakers an operator meant to reach.
func audienceSpeakerScopeEnum() {
	Enum("current", "past", "current_past")
}

// audienceQaVerdictEnum is the per-check and overall QA verdict. "NEEDS VERIFY" is
// deliberately distinct from "FAIL": it marks a check whose inputs could not be resolved
// (a referenced list that no longer exists, say), which an operator must look at but
// which is not itself evidence of a wrong audience.
func audienceQaVerdictEnum() {
	Enum("PASS", "NEEDS VERIFY", "FAIL")
}

// ─── Audience-builder types ───

// AudienceBuilderCapabilities reports what the project's portal can actually support, so
// the UI can render one explanatory banner with actions disabled instead of letting the
// operator drive a tab whose every button will 503.
//
// It carries NO feature-flag field. The flag that gates the tab is an LFX One concern (a
// BFF env var); this service reports only what it can observe about the project — whether
// a usable HubSpot connection resolves. The BFF composes the two halves.
var AudienceBuilderCapabilities = Type("audience-builder-capabilities", func() {
	Attribute("hubspot_configured", Boolean, "Whether a usable HubSpot connection resolves for this project")
	Attribute("detail", String, "Human-readable reason when hubspot_configured is false", func() {
		Example("no HubSpot connection is configured for this project")
	})
	Required("hubspot_configured")
})

// AudienceEventIdentity is the event the discovery pass searched for. It is returned
// alongside the lists because every later call in the flow (suppression lists, last-sent,
// existing masters, the composed master's name) is keyed on these values, and the UI must
// not have to re-derive them from the page it already asked this service to read.
var AudienceEventIdentity = Type("audience-event-identity", func() {
	Attribute("event_name", String, "Event name as the page declares it", func() {
		Example("KubeCon Europe 2026")
	})
	Attribute("brand_short", String, "Short brand/family token used for list-name matching", func() {
		Example("KubeCon")
	})
	Attribute("event_dates", ArrayOf(String), "Event dates in ISO form; drives the master list's quarter segment")
	Required("event_name")
})

// AudienceDiscoveredList is one existing HubSpot list the discovery pass found and
// classified.
//
// size is optional and that is load-bearing: HubSpot exposes membership size on some list
// endpoints and not others, and a list whose size is genuinely unknown must render as
// unknown. Defaulting it to 0 would read as "this list is empty" — the one wrong answer
// an operator cannot distinguish from the truth.
var AudienceDiscoveredList = Type("audience-discovered-list", func() {
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name")
	Attribute("signal", String, "Why this list qualifies for the event's audience", audienceSignalEnum)
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Attribute("reason", String, "Human-readable justification for the assigned signal")
	Attribute("list_type", String, "HubSpot processing type (DYNAMIC, MANUAL, SNAPSHOT)")
	Attribute("scope", String, "Speaker edition scope; set only when signal is event_speakers", audienceSpeakerScopeEnum)
	Attribute("hubspot_url", String, "Deep link to the list in the HubSpot UI")
	Required("list_id", "name", "signal", "reason", "list_type", "hubspot_url")
})

// AudienceDiscoveryResult is the whole outcome of one discovery pass.
//
// missing_signals is the complement of what was found, and it is returned rather than
// computed by the client so that the set of signals discovery ACTUALLY looks for stays
// defined in one place. A signal absent from a portal is normal and actionable (the
// operator creates the qualifying list); a signal absent because discovery never
// considered it is a bug, and only the server can tell the two apart.
var AudienceDiscoveryResult = Type("audience-discovery-result", func() {
	Attribute("event", AudienceEventIdentity, "The event identity discovery resolved from the URL")
	Attribute("lists", ArrayOf(AudienceDiscoveredList), "Classified lists, newest-matching first")
	Attribute("missing_signals", ArrayOf(String, audienceSignalEnum), "Signals with no qualifying list in this portal")
	Attribute("inspected", Int64, "How many candidate lists were inspected before the bound was reached")
	Required("lists", "missing_signals")
})

// AudienceSuppressionList is one list offered as an exclusion.
//
// key is the stable identity of the ROW (the standard term that produced it, or the list
// id for a discovered one) and is what the UI's selection state is keyed on; label is
// display text. They are separate because a portal can resolve two different standard
// terms to the same list, and a selection keyed on the display label would collapse them.
var AudienceSuppressionList = Type("audience-suppression-list", func() {
	Attribute("key", String, "Stable row key for selection state")
	Attribute("label", String, "Display label")
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name")
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Attribute("category", String, "Which group the row belongs to", func() {
		Enum("standard", "brand", "event_specific")
	})
	Attribute("hubspot_url", String, "Deep link to the list in the HubSpot UI")
	Required("key", "label", "list_id", "name", "category", "hubspot_url")
})

// AudienceListSearchResult is one typeahead hit.
var AudienceListSearchResult = Type("audience-list-search-result", func() {
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name")
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Attribute("hubspot_url", String, "Deep link to the list in the HubSpot UI")
	Required("list_id", "name", "hubspot_url")
})

// AudienceListBrief names a list referenced by a previous send.
//
// missing is REQUIRED and distinct from an absent name: a previous send can reference a
// list that has since been deleted, and the operator needs to see that the reference
// existed and no longer resolves. Omitting the row entirely would misreport the prior
// send's audience as smaller than it was.
//
// resolved_from_legacy_id records that the id on the email was a legacy (v1) list id that
// had to be translated to reach the v3 list. Surfaced because an operator comparing the
// two ids in HubSpot would otherwise see a mismatch and distrust the whole row.
var AudienceListBrief = Type("audience-list-brief", func() {
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name; empty when the list no longer resolves")
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Attribute("missing", Boolean, "True when the referenced list could not be resolved")
	Attribute("resolved_from_legacy_id", String, "Legacy list id this row was translated from")
	Attribute("hubspot_url", String, "Deep link to the list in the HubSpot UI; absent when it no longer resolves")
	Required("list_id", "name", "missing")
})

// AudienceLastSentEmail is one previous marketing email, with the audience it targeted.
var AudienceLastSentEmail = Type("audience-last-sent-email", func() {
	Attribute("email_id", String, "HubSpot marketing email id")
	Attribute("email_name", String, "Email name")
	Attribute("sent_at", String, "When the email was published/sent, RFC 3339")
	Attribute("hubspot_url", String, "Deep link to the email in the HubSpot UI")
	Attribute("included_lists", ArrayOf(AudienceListBrief), "Lists the send included", func() {
		Example(audienceIncludedListsExample)
	})
	Attribute("suppression_lists", ArrayOf(AudienceListBrief), "Lists the send suppressed", func() {
		Example(audienceSuppressionListsExample)
	})
	// Without this, a failed selection read and a send that genuinely targeted nothing have the
	// IDENTICAL wire shape — two empty arrays — so a HubSpot 5xx renders as false precedent.
	// The email's name and link are still worth showing, so the row is kept and marked rather
	// than dropped or failing the whole listing.
	Attribute("lists_unavailable", Boolean,
		"True when this email's list selection could not be read. The two list arrays are then "+
			"empty because they are UNKNOWN, not because the send targeted nothing.")
	Required("email_id", "email_name", "hubspot_url", "included_lists", "suppression_lists")
	// TYPE-LEVEL so the published example is a possible one. Composed from the attribute
	// examples, Goa published populated list arrays beside lists_unavailable=true, which this
	// type defines as "the arrays are empty because they are unknown". A readable selection
	// (lists_unavailable false) with populated lists is the ordinary row.
	Example(map[string]any{
		"email_id":          "184312345678",
		"email_name":        "KubeCon NA 2026 - Early Bird",
		"sent_at":           "2026-09-15T16:00:00Z",
		"hubspot_url":       "https://app.hubspot.com/email/8675309/details/184312345678",
		"included_lists":    audienceIncludedListsExample,
		"suppression_lists": audienceSuppressionListsExample,
		"lists_unavailable": false,
	})
})

// audienceIncludedListsExample / audienceSuppressionListsExample are resolved list rows (missing
// false, so name and hubspot_url are present), shared by the attribute and type examples above.
var audienceIncludedListsExample = []map[string]any{
	{"list_id": "4821", "name": "KubeCon NA 2025 attendees", "size": 18250, "missing": false, "hubspot_url": "https://app.hubspot.com/contacts/8675309/lists/4821"},
}

var audienceSuppressionListsExample = []map[string]any{
	{"list_id": "1022", "name": "Global unsubscribes", "size": 3120, "missing": false, "hubspot_url": "https://app.hubspot.com/contacts/8675309/lists/1022"},
}

// AudienceMasterListBrief is one previously composed master list for this event family.
var AudienceMasterListBrief = Type("audience-master-list-brief", func() {
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name")
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Attribute("hubspot_url", String, "Deep link to the list in the HubSpot UI")
	Required("list_id", "name", "hubspot_url")
})

// AudiencePreviewCount is the size of the union of the selected lists.
//
// exact is what makes this type honest. Computing a true union means paging every list's
// membership, which is bounded — above the bound the count is abandoned and exact=false.
// The one thing this must never do is return a fabricated precise number for a union it
// did not finish counting, so count is meaningful only when exact is true.
//
// When exact is false, estimate is the SUM of the selected lists' sizes, which is an UPPER
// bound on the union: every contact in more than one list is counted once per list, so the
// real union can only be smaller. It is not a floor. An earlier version of this comment and
// the estimate attribute below both said "lower bound", which inverts the guarantee — a
// client trusting that would read "25,000+" as "at least 25,000 people" when the true reach
// may be far less, and size a send around it.
var AudiencePreviewCount = Type("audience-preview-count", func() {
	Attribute("exact", Boolean, "True when the union was counted in full")
	// It CARRIES the estimate when exact is false (OverCapPreviewCount and
	// DegradedPreviewCount both set Count = Estimate), so a client that ignores it entirely
	// is told to discard a value the implementation deliberately emits. What it must not do
	// is read it as a counted figure: only `exact` says whether it was counted.
	Attribute("count", Int64,
		"Union size. COUNTED only when exact is true; when exact is false this mirrors estimate "+
			"(an upper bound), or is 0 when no reliable total exists. Read `exact` before trusting it.")
	Attribute("estimate", Int64, "Upper bound on the union size (the sum of list sizes) when exact is false; 0 when no reliable total exists")
	Attribute("reason", String, "Why the count is exact or bounded")
	Required("exact", "count", "estimate", "reason")
})

// AudienceComposedList is a list this service created, echoed back with its deep link so
// the operator can verify it in HubSpot immediately.
var AudienceComposedList = Type("audience-composed-list", func() {
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name")
	Attribute("hubspot_url", String, "Deep link to the list in the HubSpot UI")
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Required("list_id", "name", "hubspot_url")
})

// AudienceComposeMasterInput is the compose payload.
//
// The naming inputs (brand_short, event_name, event_dates) are accepted rather than
// re-derived so the name reflects what the operator REVIEWED on screen. name overrides
// them outright, for the case where the derived name is wrong.
var AudienceComposeMasterInput = Type("audience-compose-master-input", func() {
	Attribute("list_ids", ArrayOf(String), "Lists whose union forms the master audience", func() {
		MinLength(1)
		// Same bound and reason as preview-count's list_ids: this becomes one AND branch
		// per id in the filter MasterListWithSuppressionFilter builds.
		MaxLength(200)
	})
	Attribute("exclude_list_ids", ArrayOf(String), "Lists to suppress from the master", func() {
		MaxLength(200)
	})
	Attribute("name", String, "Explicit master list name; overrides the derived name")
	Attribute("brand_short", String, "Short brand token for the derived name")
	Attribute("event_name", String, "Event name for the derived name")
	Attribute("event_dates", ArrayOf(String), "Event dates; drive the derived name's quarter segment")
	// brief_id turns compose from a pure create into a create-and-attach. It is optional
	// because the builder is also used exploratorily, with no campaign to attach to --
	// which is why these routes are scoped to the project rather than to a brief.
	Attribute("brief_id", String, "Record the composed master as this brief's built audience, stamped with the portal it was composed in", func() {
		Format(FormatUUID)
	})
	Attribute("inclusion_summary", String, "Operator-visible provenance for the recorded audience; derived from the source lists when omitted")
	Required("list_ids")
})

// AudienceComposeRecordedAudience is the audience row a compose created, when it was asked
// to record one.
//
// Deliberately slim: it carries what a caller needs to show the attachment and to address
// the row later, and NOT built_in_portal_id. The portal is provenance the service asserts
// against at dispatch; exposing it would invite a caller to send it back, and provenance a
// client can supply is provenance that proves nothing.
var AudienceComposeRecordedAudience = Type("audience-compose-recorded-audience", func() {
	Attribute("id", String, "Audience id")
	Attribute("status", String, "Audience status; always built for a recorded compose")
	Attribute("version", Int64, "Optimistic-concurrency version")
	Attribute("platform_master_list_id", String, "The master list this audience sends to; for a multi-list attach, the first of include_list_ids")
	Attribute("include_list_ids", ArrayOf(String), "Existing lists the send goes to directly, when the audience was attached to several lists instead of one master; absent for a single master")
	Required("id", "status", "version", "platform_master_list_id")
})

// AudienceComposeMasterResult reports what was created.
var AudienceComposeMasterResult = Type("audience-compose-master-result", func() {
	Attribute("master", AudienceComposedList, "The created master list")
	Attribute("suppression", AudienceComposedList, "The combined suppression list, when exclusions were requested")
	Attribute("source_list_ids", ArrayOf(String), "The inclusion lists the master unions")
	Attribute("audience", AudienceComposeRecordedAudience, "The audience row recorded for brief_id, when one was requested and written")
	// Required, and separate from `audience`, so no caller has to infer "attached" from an
	// absent object. An older client reading only `audience` and a newer one reading
	// `recorded` must never disagree about whether the send list is wired up.
	Attribute("recorded", Boolean, "Whether the master was recorded as the brief's audience; false when no brief_id was supplied")
	Required("master", "source_list_ids", "recorded")
})

// AudienceAttachExistingInput nominates lists that ALREADY exist as a brief's audience.
//
// The "use the same selections as this earlier send" path: the master a previous email
// sent to is reused as-is, so no new list is composed. The ids are client-supplied, which
// is why the service reads every one of them back from the project's portal before
// recording anything, and why the portal is still stamped server-side rather than taken
// from the caller.
//
// EXACTLY ONE of master_list_id or a non-empty include_list_ids is required, which the
// attribute types cannot express, so the service enforces it (400 otherwise).
// include_list_ids sends to several existing lists directly -- a HubSpot email's recipients
// are an array -- without composing a master that unions them; an id named in both
// include_list_ids and suppression_list_ids is refused 400 rather than resolved, because it
// would exclude that list's whole audience.
var AudienceAttachExistingInput = Type("audience-attach-existing-input", func() {
	Attribute("brief_id", String, "The brief to record the audience under", func() {
		Format(FormatUUID)
	})
	Attribute("master_list_id", String, "The existing contact list the send goes to. Exactly one of master_list_id or include_list_ids is required", func() {
		MinLength(1)
	})
	Attribute("include_list_ids", ArrayOf(String), "Several existing contact lists the send goes to directly, with no composed master. Exactly one of master_list_id or include_list_ids is required; the first id is recorded as platform_master_list_id", func() {
		MaxLength(200)
	})
	Attribute("suppression_list_ids", ArrayOf(String), "Existing lists the send suppresses", func() {
		MaxLength(200)
	})
	Attribute("inclusion_summary", String, "Operator-visible provenance for the recorded audience; derived from the master list when omitted")
	Required("brief_id")
	// An explicit example: exactly one of master_list_id / include_list_ids is accepted, and the
	// generated one filled in both, so the CLI usage it produced was a request the service refuses.
	Example(map[string]any{
		"brief_id":             "5b3c1f0e-8d2a-4c6b-9e1f-2a7d4c8b6e10",
		"include_list_ids":     []string{"31027", "31028"},
		"suppression_list_ids": []string{"201"},
		"inclusion_summary":    "2 lists: Prospects, Past attendees",
	})
})

// AudienceAttachExistingResult reports the verified lists and the row recorded for them.
var AudienceAttachExistingResult = Type("audience-attach-existing-result", func() {
	Attribute("master", AudienceComposedList, "The existing master list, as read back from the portal")
	Attribute("suppression_list_ids", ArrayOf(String), "The suppression list ids recorded beside it")
	Attribute("include_list_ids", ArrayOf(String), "Every include list the send goes to, when include_list_ids was supplied; absent for a single master_list_id")
	Attribute("audience", AudienceComposeRecordedAudience, "The audience row recorded for brief_id")
	Required("master", "suppression_list_ids", "audience")
})

// AudienceComposePartialError is the error body for a compose that created SOME platform
// state and then failed.
//
// It is a distinct error type rather than a message convention because the remedy differs
// from every other failure here: the caller must NOT retry (a retry would create a second
// contact list), and it must show the operator the list that DOES exist. A plain
// InternalServerError would leave an orphaned suppression list invisible.
var AudienceComposePartialError = Type("audience-compose-partial-error", func() {
	errorAttrs("500", "Compose failed after creating or possibly creating platform state that must be reconciled before retrying.")
	Attribute("suppression", AudienceComposedList, "The suppression list that WAS created and must be reconciled")
	Attribute("suppression_name", String,
		"The suppression list's deterministic name, set only when the suppression create itself is "+
			"unconfirmed (HubSpot may have created it) -- search for this name in HubSpot before composing again")
	Attribute("master_name", String,
		"The master list's deterministic name, set only when the master create itself is unconfirmed "+
			"(HubSpot may have created it) -- search for this name in HubSpot before composing again")
	// The fifth reachable shape: both lists exist and are usable, and only the attachment
	// failed. It is the one partial where the master can be named rather than merely
	// searched for, so it carries the list itself -- the operator can attach it by hand,
	// and must not compose again to get one they already have.
	Attribute("master", AudienceComposedList,
		"The master list that WAS created, set only when recording it as the brief's audience failed")
})

// AudienceQaFinding is one problem found by a QA check.
var AudienceQaFinding = Type("audience-qa-finding", func() {
	Attribute("severity", String, "How bad the finding is", func() {
		Enum("CRITICAL", "HIGH", "MEDIUM")
	})
	Attribute("message", String, "What is wrong")
	Attribute("fix", String, "What to do about it")
	Required("severity", "message", "fix")
})

// AudienceQaCheck is the base shape of every QA check: a verdict plus its findings.
var AudienceQaCheck = Type("audience-qa-check", func() {
	Attribute("verdict", String, "Check verdict", audienceQaVerdictEnum)
	Attribute("findings", ArrayOf(AudienceQaFinding), "Findings that produced the verdict")
	Required("verdict", "findings")
})

// AudienceQaSuppressionCheck adds which regulatory suppressions were detected. The two
// booleans are reported separately from the verdict because a send that legitimately
// targets neither region should not read as a failed GDPR check.
var AudienceQaSuppressionCheck = Type("audience-qa-suppression-check", func() {
	Reference(AudienceQaCheck)
	Attribute("verdict")
	Attribute("findings")
	Attribute("applied_gdpr", Boolean, "A GDPR/EU suppression list was applied")
	Attribute("applied_opt_out", Boolean, "An opt-out/unsubscribe suppression list was applied")
	Required("verdict", "findings", "applied_gdpr", "applied_opt_out")
})

// AudienceQaExclusionCheck adds the number of exclusion branches found, so a PASS with
// zero exclusions is distinguishable from a PASS with several.
var AudienceQaExclusionCheck = Type("audience-qa-exclusion-check", func() {
	Reference(AudienceQaCheck)
	Attribute("verdict")
	Attribute("findings")
	Attribute("exclusion_count", Int64, "How many exclusion branches the filter carries")
	Required("verdict", "findings", "exclusion_count")
})

// AudienceQaChecks is the full check set.
var AudienceQaChecks = Type("audience-qa-checks", func() {
	Attribute("signal_mapping", AudienceQaCheck, "Do the referenced lists map to recognisable signals?")
	Attribute("suppression", AudienceQaSuppressionCheck, "Are the expected regulatory suppressions applied?")
	Attribute("exclusion_completeness", AudienceQaExclusionCheck, "Are the exclusions present and well-formed?")
	// OPTIONAL in the result, because it is only produced when `event_name` was supplied.
	// Absent means the audit could not run, which is NOT the same as a pass -- a required
	// field carrying a zero-valued verdict would read as one.
	Attribute("current_registrants", AudienceQaCheck, "Is this edition's own registration list suppressed rather than included?")
	Required("signal_mapping", "suppression", "exclusion_completeness")
})

// AudienceQaCandidate is one list a resolution attempt matched.
var AudienceQaCandidate = Type("audience-qa-candidate", func() {
	Attribute("list_id", String, "HubSpot list id")
	Attribute("name", String, "HubSpot list name")
	Attribute("size", Int64, "Membership size, when HubSpot reported one")
	Required("list_id", "name")
})

// AudienceQaResult is the QA outcome.
//
// needs_disambiguation is REQUIRED and is the discriminator: when it is true the only
// other populated field is candidates, and none of the report fields are meaningful. Goa
// has no union type, so both arms share one struct — but the discriminator is mandatory
// precisely so a client cannot read an ambiguous result as if it carried verdicts. The
// BFF narrows this back into a real TypeScript discriminated union.
var AudienceQaResult = Type("audience-qa-result", func() {
	Attribute("needs_disambiguation", Boolean, "True when the list reference matched more than one list")
	Attribute("candidates", ArrayOf(AudienceQaCandidate), "Matches to choose between; set only when needs_disambiguation is true")
	Attribute("list_id", String, "The audited list's id")
	Attribute("name", String, "The audited list's name")
	Attribute("hubspot_url", String, "Deep link to the audited list")
	Attribute("checks", AudienceQaChecks, "Per-check results")
	Attribute("findings", ArrayOf(AudienceQaFinding), "Every finding across all checks, most severe first")
	Attribute("overall", String, "Worst verdict across the checks", audienceQaVerdictEnum)
	Required("needs_disambiguation")
})

// ─── Audience-builder service ───

var _ = Service("lfx-v2-campaign-service-audience-builder", func() {
	Description("Explore a project's HubSpot portal to assemble, size, compose and QA a campaign audience.")

	Security(JWTAuth)

	Method("get-audience-builder-capabilities", func() {
		Description("Report whether this project's portal can support audience building.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(AudienceBuilderCapabilities)
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/audience-builder/capabilities")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("discover-audience-lists", func() {
		Description("Read an event page, then find and classify the portal's existing contact lists that qualify for its audience. Creates nothing.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("event_url", String, "Event page URL to read the identity from", func() {
				Example("https://events.linuxfoundation.org/kubecon-europe/")
			})
			Required("project_id", "event_url")
		})
		Result(AudienceDiscoveryResult)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/audience-builder/discover")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("search-audience-lists", func() {
		Description("Typeahead over the portal's contact lists, for attaching a list by hand.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("q", String, "Search query", func() { MinLength(1) })
			Required("project_id", "q")
		})
		Result(func() {
			Attribute("lists", ArrayOf(AudienceListSearchResult), "Matching lists")
			Required("lists")
		})
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/audience-builder/lists/search")
			Header("bearer_token:Authorization")
			Param("q")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("get-audience-suppression-lists", func() {
		Description("Resolve the standard suppression terms plus any brand or event-specific suppression lists in the portal.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("brand_short", String, "Short brand token to match brand suppression lists")
			Attribute("event_name", String, "Event name to match event-specific suppression lists")
			Required("project_id")
		})
		Result(func() {
			Attribute("lists", ArrayOf(AudienceSuppressionList), "Suppression rows, standard first")
			Required("lists")
		})
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/audience-builder/suppression-lists")
			Header("bearer_token:Authorization")
			Param("brand_short")
			Param("event_name")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("get-audience-last-sent", func() {
		Description("Find the most recent marketing emails for this event family and report the lists each one targeted.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("event_name", String, "Event name to search emails for", func() { MinLength(1) })
			Attribute("brand_short", String, "Short brand token, used as a fallback search term")
			Attribute("limit", Int, "How many emails to report", func() {
				Minimum(1)
				Maximum(10)
				Default(3)
			})
			Required("project_id", "event_name")
		})
		Result(func() {
			Attribute("emails", ArrayOf(AudienceLastSentEmail), "Emails, most recently sent first")
			Required("emails")
		})
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/audience-builder/last-sent")
			Header("bearer_token:Authorization")
			Param("event_name")
			Param("brand_short")
			Param("limit")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("get-existing-audience-master-lists", func() {
		Description("Find master lists already composed for this event family, newest quarter first.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("event_name", String, "Event name to match", func() { MinLength(1) })
			Attribute("brand_short", String, "Short brand token to match")
			Required("project_id", "event_name")
		})
		Result(func() {
			Attribute("lists", ArrayOf(AudienceMasterListBrief), "Master lists, newest quarter first")
			Required("lists")
		})
		commonBriefErrors()
		HTTP(func() {
			GET("/projects/{project_id}/audience-builder/existing-master-lists")
			Header("bearer_token:Authorization")
			Param("event_name")
			Param("brand_short")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("preview-audience-count", func() {
		Description("Count the union of the selected lists' memberships — exactly when that is within bounds, as an UPPER-bound estimate (the sum of list sizes, which double-counts overlap) when it is not, and as no number at all when any selected list did not report a size. Creates nothing.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			// MaxLength bounds a fan-out, not a form field: the sweep is two
			// sequential HubSpot round-trips per id, so an unbounded array is a
			// slow-request lever. Mirrors audience.PreviewMaxLists; rejecting here
			// costs one 400 instead of a gateway timeout with nothing in the log.
			Attribute("list_ids", ArrayOf(String), "Lists to union", func() {
				MinLength(1)
				MaxLength(50)
			})
			Required("project_id", "list_ids")
		})
		Result(AudiencePreviewCount)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/audience-builder/preview-count")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
		})
	})

	Method("compose-audience-master", func() {
		Description("Create the combined suppression list and then the master list in the project's HubSpot portal, and when brief_id is supplied record the master as that brief's built audience. NOT idempotent.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("compose", AudienceComposeMasterInput, "What to compose the master list from")
			Required("project_id", "compose")
		})
		Result(AudienceComposeMasterResult)
		commonBriefErrors()
		// ComposePartial is separate from InternalServerError because the caller must NOT
		// retry it: the suppression list already exists upstream, and a retry would create a
		// second one. It carries that list so the UI can show what to reconcile.
		Error("ComposePartial", AudienceComposePartialError, "Platform state was partially created; do not retry")
		HTTP(func() {
			POST("/projects/{project_id}/audience-builder/compose-master")
			Header("bearer_token:Authorization")
			// 201 rather than 200: the successful outcome is a new upstream resource, not a
			// computed answer.
			Response(StatusCreated)
			briefErrorResponses()
			Response("ComposePartial", StatusInternalServerError)
		})
	})

	Method("attach-existing-audience", func() {
		Description("Record lists that already exist in the project's HubSpot portal as a brief's built audience, without composing a new master. Every id is read back from the portal first. Creates nothing in HubSpot.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("attach", AudienceAttachExistingInput, "The existing lists to attach")
			Required("project_id", "attach")
		})
		Result(AudienceAttachExistingResult)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/audience-builder/attach-existing")
			Header("bearer_token:Authorization")
			// 201: a new audience row is the outcome.
			Response(StatusCreated)
			briefErrorResponses()
		})
	})

	Method("run-audience-qa", func() {
		Description("Audit a composed master list's filters: signal mapping, regulatory suppression, exclusion completeness, and whether this edition's own registrants are included. Creates nothing.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("list_ref", String, "List id or name to audit", func() { MinLength(1) })
			Attribute("targets_eu", Boolean, "The send targets the EU, so a GDPR suppression is expected")
			Attribute("targets_ca", Boolean, "The send targets Canada, so a CASL opt-out suppression is expected")
			// OPTIONAL, and the check it enables is OMITTED without it rather than guessed.
			//
			// A registration list names its own edition ("26Q2 AGNTCon + MCPCon North America
			// 2026 Event Registration"), so deciding whether an INCLUDED registration list is
			// THIS event's needs the event's name. Absent, `current_registrants` is omitted
			// entirely and `overall` is unaffected -- see the godoc on the result attribute.
			// Reporting NEEDS VERIFY instead, which this comment used to describe, flipped every
			// existing caller's audit: no client sends `event_name` yet, so every previously
			// PASSing audit would have become NEEDS VERIFY and trained operators to ignore the
			// verdict.
			Attribute("event_name", String, "This edition's event name, so an included registration list for THIS event can be told from another event's", func() {
				MinLength(1)
			})
			Required("project_id", "list_ref")
		})
		Result(AudienceQaResult)
		commonBriefErrors()
		HTTP(func() {
			POST("/projects/{project_id}/audience-builder/qa/run")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			briefErrorResponses()
		})
	})
})
