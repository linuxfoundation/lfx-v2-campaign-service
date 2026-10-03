// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Pre-send QA on a HubSpot contact list (LFXV2-2770)
//
// Three checks, each answering a question that has cost a real send: does this
// list select on anything a contact actually DID, are the consent suppressions
// genuinely applied as exclusions, and does it exclude anything at all.
//
// Everything is inferred from the list's own filterBranch plus the NAMES of the
// lists it references, because this portal has no machine-readable marker for
// "this is the GDPR list" — naming convention is the only signal there is. That
// makes every verdict here evidence for a human decision, never an automated gate:
// `NEEDS VERIFY` is the honest and most common answer, and no caller may treat a
// `PASS` as authorization to send.
// ---------------------------------------------------------------------------

// Verdict is one check's outcome, or the whole list's.
type Verdict string

const (
	VerdictPass        Verdict = "PASS"
	VerdictNeedsVerify Verdict = "NEEDS VERIFY"
	VerdictFail        Verdict = "FAIL"
)

// Severity ranks a finding by legal exposure, not tidiness.
type Severity string

const (
	SeverityCritical Severity = "CRITICAL"
	SeverityHigh     Severity = "HIGH"
	SeverityMedium   Severity = "MEDIUM"
)

// Finding is one problem and the remediation that resolves it. `Fix` is never
// empty: a finding an operator cannot act on trains them to ignore the report.
type Finding struct {
	Severity Severity
	Message  string
	Fix      string
}

// Check is one check's verdict and its findings.
type Check struct {
	Verdict  Verdict
	Findings []Finding
}

// engagementHints indicate a real engagement signal — something the contact did.
//
// Matched as SUBSTRINGS against a flattened filter signature, so `event_` catches
// `event_registration` and every sibling property without enumerating them. The
// stem is `registra` rather than `registrat` for the same reason: the most common
// engagement list in this portal is named "<event> Registrants", which the longer
// stem does not match — a PASS-worthy list would come back NEEDS VERIFY.
// Deliberately broad: a false PASS here costs a NEEDS VERIFY that a human would
// have resolved anyway, while a false FAIL on a perfectly good list trains
// operators to ignore the whole report.
var engagementHints = []string{
	"custom_event", "customevent", "event_", "registra", "attend", "page_view",
	"pageview", "web_analytics", "webanalytics", "visited", "email_subscription",
	"opt_in", "optin", "enrollment", "hs_analytics", "web visit", "subscri",
	"training", "webinar", "download", "engagement", "speaker",
}

// firmographicOnlyHints describe who a contact IS, not what they did.
//
// A list built only from these is the specific failure this check exists to catch:
// "everyone in Germany with a director title" is a purchased-list-shaped audience
// with no consent story, and it looks entirely reasonable in the HubSpot UI.
var firmographicOnlyHints = []string{
	"country", "state", "region", "jobtitle", "job_title", "company", "industry",
	"hs_country", "address",
}

var (
	gdprHints               = []string{"gdpr"}
	optOutHints             = []string{"opt out", "opt-out", "optout", "global opt"}
	genericSuppressionHints = []string{"suppress", "exclusion", "unsubscribe", "do not email", "casl", "opt out", "opt-out"}
)

// listURLIDRE extracts the list id from a HubSpot app URL. Both path spellings are
// accepted: `objectLists` is what the modern list editor uses and what an operator
// copies from the address bar, `lists` is what older links and this service's own
// AppURL builder produce.
var listURLIDRE = regexp.MustCompile(`(?:objectLists|lists)/(\d+)`)

var numericIDRE = regexp.MustCompile(`^\d+$`)

// ParseListRef turns whatever the operator pasted into a list id, or reports that
// it must be resolved by name.
//
// Accepts a HubSpot URL (what they have on screen) or a bare id (what the API
// returns). A name goes back to the caller to search, because resolving a name
// needs HubSpot and this package stays pure.
func ParseListRef(listRef string) (listID string, isName bool) {
	ref := strings.TrimSpace(listRef)
	if ref == "" {
		return "", false
	}
	if match := listURLIDRE.FindStringSubmatch(ref); len(match) == 2 {
		return match[1], false
	}
	if numericIDRE.MatchString(ref) {
		return ref, false
	}
	return "", true
}

// PickNameMatches decides which search hits a typed name resolves to.
//
// A SINGLE exact case-insensitive name match is unambiguous even when other lists
// merely contain the string, so it wins outright over those looser hits. Two or
// more matches — exact or not — resolve to NOTHING and the caller must ask:
// HubSpot does not enforce unique list names, so picking the first would run QA
// on a list the operator never meant and report a verdict under the name they
// typed.
func PickNameMatches(ref string, hits []ListCandidate) (chosen *ListCandidate, ambiguous []ListCandidate) {
	needle := strings.ToLower(strings.TrimSpace(ref))
	exact := make([]ListCandidate, 0, 1)
	for _, hit := range hits {
		if strings.ToLower(strings.TrimSpace(hit.Name)) == needle {
			exact = append(exact, hit)
		}
	}
	matches := hits
	if len(exact) > 0 {
		matches = exact
	}
	switch {
	case len(matches) == 0:
		return nil, nil
	case len(matches) > 1:
		// Ambiguous whether or not the names matched EXACTLY. The previous `&& len(exact)
		// == 0` meant two lists sharing a name silently resolved to `matches[0]`, i.e.
		// whichever order HubSpot returned -- so QA would audit an arbitrary one and report
		// a verdict under the name the operator typed. Duplicate names are reachable in a
		// real portal, especially after an ambiguous create leaves a second list behind.
		// Disambiguation is the honest answer in both cases.
		return nil, matches
	default:
		best := matches[0]
		return &best, nil
	}
}

// ReferencedListIDs are the ids of lists referenced with the given membership
// operator, de-duplicated in first-seen order.
//
// `filterType` is "IN_LIST" for BOTH directions — only `operator` distinguishes
// them. Reading the type instead would make every exclusion look like an
// inclusion, which turns the suppression check inside out.
func ReferencedListIDs(filters []ListFilter, operator string) []string {
	seen := make(map[string]struct{}, len(filters))
	out := make([]string, 0, len(filters))
	for _, f := range filters {
		if f.FilterType != "IN_LIST" || strings.ToUpper(strings.TrimSpace(f.Operator)) != operator {
			continue
		}
		id := f.ListIDString()
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// FilterSignature flattens one filter to a lowercase string for substring
// matching.
//
// Includes the NAME of any list the filter references, which is what makes a
// membership-based filter classifiable at all — "IN_LIST 12345" says nothing,
// while "KubeCon Registrants" says everything. That is also why the hint lists mix
// property names with prose fragments.
func FilterSignature(f ListFilter, nameByID map[string]string) string {
	parts := []string{f.FilterType, f.Property, f.PropertyName}
	if id := f.ListIDString(); id != "" {
		parts = append(parts, nameByID[id])
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// CheckSignalMapping asks whether this list selects on something the contact
// actually did.
func CheckSignalMapping(filters []ListFilter, nameByID map[string]string) Check {
	// Exclusions are dropped before anything is inspected. A NOT_IN_LIST reference
	// says what the list keeps OUT, so it can never establish an inclusion signal
	// — but its signature is non-empty and non-firmographic, so counting it would
	// rescue a geography-only list from the FAIL below purely because it also
	// suppresses opt-outs. That is the exact list this check exists to catch.
	// Coverage of exclusions belongs to checks 2 and 3.
	signatures := make([]string, 0, len(filters))
	for _, f := range filters {
		if strings.ToUpper(strings.TrimSpace(f.Operator)) == "NOT_IN_LIST" {
			continue
		}
		signatures = append(signatures, FilterSignature(f, nameByID))
	}

	for _, signature := range signatures {
		if containsAny(signature, engagementHints) {
			return Check{Verdict: VerdictPass, Findings: []Finding{}}
		}
	}

	if len(signatures) == 0 {
		return Check{Verdict: VerdictNeedsVerify, Findings: []Finding{{
			Severity: SeverityMedium,
			Message:  "List has no inclusion conditions to inspect (static/manual membership) — the signal can't be inferred from filters alone.",
			Fix:      "Confirm manually whether membership was added based on a real engagement action.",
		}}}
	}

	// A signature can be EMPTY when a filter carries none of the fields we read;
	// those count as firmographic-or-unknown rather than as engagement, so an
	// all-empty set does not PASS — it FAILs, same as an explicit firmographic-only
	// set, because neither one carries any evidence of an engagement signal.
	onlyFirmographic := true
	for _, signature := range signatures {
		if strings.TrimSpace(signature) == "" {
			continue
		}
		if !containsAny(signature, firmographicOnlyHints) {
			onlyFirmographic = false
			break
		}
	}
	if onlyFirmographic {
		return Check{Verdict: VerdictFail, Findings: []Finding{{
			Severity: SeverityHigh,
			Message:  "List filters only on geography/firmographic properties, with no engagement or opt-in signal (registration, page view, email activity).",
			Fix:      "Add an event-registration, opt-in, or page-view condition before sending, or keep this list exploratory-only.",
		}}}
	}

	return Check{Verdict: VerdictNeedsVerify, Findings: []Finding{{
		Severity: SeverityMedium,
		Message:  "Could not confidently classify this list's filter conditions as engagement-based or not.",
		Fix:      "Manually review the filter conditions in HubSpot.",
	}}}
}

// SuppressionCheck adds which consent suppressions were actually found, so the UI
// can show the two that matter as applied/not rather than only as findings.
type SuppressionCheck struct {
	Check
	AppliedGDPR   bool
	AppliedOptOut bool
}

// CheckSuppression asks whether the consent suppressions are actually applied,
// given the names of everything this list excludes (including one hop into a
// Combined-Suppression wrapper — see (*AudienceExplorer).exclusionNames in
// internal/dispatch).
//
// Severity tracks legal exposure. Sending to EU contacts with no GDPR suppression
// is the one CRITICAL in this service and the only condition that can produce a
// FAIL here.
//
// Canada is a MEDIUM with an unusual message because this portal has NO dedicated
// CASL suppression list under any name — confirmed against the live portal. The
// check cannot ask for one to be applied, so it asks for a human confirmation
// instead of inventing a remediation the operator cannot perform.
func CheckSuppression(exclusionNames []string, targetsEU, targetsCA bool) SuppressionCheck {
	gdpr, optOut, anySuppression := false, false, false
	for _, name := range exclusionNames {
		if containsAny(name, gdprHints) {
			gdpr = true
		}
		if containsAny(name, optOutHints) {
			optOut = true
		}
		if containsAny(name, genericSuppressionHints) {
			anySuppression = true
		}
	}

	findings := make([]Finding, 0, 3)

	switch {
	case targetsEU && !gdpr:
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Message:  "No GDPR suppression list found among this list's exclusions, but this audience targets the EU.",
			Fix:      "Apply the appropriate GDPR suppression list (e.g. a brand or portfolio GDPR-suppression list) as a NOT_IN_LIST exclusion.",
		})
	case !gdpr && len(exclusionNames) == 0:
		// Only raised when the list excludes NOTHING. A list with exclusions that
		// simply are not GDPR-named is already covered by the opt-out and
		// completeness findings; repeating it here would put three findings on one
		// cause and bury the one that matters.
		findings = append(findings, Finding{
			Severity: SeverityMedium,
			Message:  "No GDPR suppression list found among this list's exclusions.",
			Fix:      "Confirm whether GDPR suppression is required for this audience, and apply it as an exclusion if so.",
		})
	}

	if !optOut {
		findings = append(findings, Finding{
			Severity: SeverityHigh,
			Message:  "No Global Opt-Out(s) list found among this list's exclusions.",
			Fix:      "Apply the relevant Global Opt-Out(s) list (portfolio-wide or brand-scoped) as a NOT_IN_LIST exclusion.",
		})
	}

	if targetsCA && !anySuppression {
		findings = append(findings, Finding{
			Severity: SeverityMedium,
			Message:  "This audience targets Canada, but this portal has no dedicated CASL suppression list — and no suppression exclusion of any kind was found on this list.",
			Fix:      "Manually confirm Canadian contacts' consent/opt-out status is honored (e.g. via the Global Opt-Out list) before sending.",
		})
	}

	return SuppressionCheck{
		Check:         Check{Verdict: VerdictFromFindings(findings), Findings: findings},
		AppliedGDPR:   gdpr,
		AppliedOptOut: optOut,
	}
}

// eventYearFromName is the four-digit year an event name declares, or "" when it declares
// none. The LAST match: "AGNTCon 2026" has one, and a name that mentions two takes the
// later, which is the edition being sent.
func eventYearFromName(eventName string) string {
	matches := yearRE.FindAllString(eventName, -1)
	if len(matches) == 0 {
		return ""
	}
	return strings.TrimSpace(matches[len(matches)-1])
}

// namesTheEdition reports whether a list name carries `year`, in EITHER spelling this
// portfolio uses.
//
// A raw `strings.Contains(name, "2026")` missed this service's OWN naming convention and so
// PASSED on the lists it creates itself. `MasterListName` writes a two-digit QUARTER code --
// `builder_master_test.go` pins "26Q1 - CNCF - KubeCon Europe - Master" -- with no four-digit
// year anywhere in it. Measured before fixing: "26Q1 - CNCF - KubeCon Europe - Registrants"
// returned PASS while the same name with "2026" spliced in returned FAIL, so the check was keyed
// on an accident of one list's name rather than on the edition.
//
// The four-digit form is matched as a standalone year, not a substring: a list id or a contact
// count that happens to read "2026" cannot satisfy it on its own.
func namesTheEdition(name, year string) bool {
	if len(year) != 4 {
		return false
	}
	for _, found := range yearRE.FindAllString(name, -1) {
		if found == year {
			return true
		}
	}
	// The `YYQN` spelling, compared on the captured two digits rather than by substring, so
	// "26Q1" matches 2026 and "25Q4" does not. `quarterCodeRE` is builder_master.go's, the same
	// pattern `MasterListName` writes with.
	for _, m := range quarterCodeRE.FindAllStringSubmatch(name, -1) {
		if m[1] == year[2:] {
			return true
		}
	}
	return false
}

// registrantNaming is how a list name mentions registration, for CheckCurrentRegistrants.
type registrantNaming int

const (
	// registrantNamingNone: the name does not mention registration at all.
	registrantNamingNone registrantNaming = iota
	// registrantNamingPlain: at least one registration term appears un-negated.
	registrantNamingPlain
	// registrantNamingNegated: every registration term in the name is negated.
	registrantNamingNegated
)

// registrantTermRE matches one registration term, capturing any negation that sits ON it --
// either a negating word immediately before it ("not registered", "never attended") or a negating
// prefix fused to it ("unregistered", "de-registered", "pre-registration").
//
// The negation has to be ADJACENT, which is the whole reason this is one regex rather than two
// checks. A first attempt looked for a negating word anywhere in the name and turned
// "Registrants (De-duplicated)" and "Registration - No Discount" into undecidable names: both
// carry a negating word that negates something else entirely. Asking whether the negation is
// attached to the registration term is a question about structure, which converges; asking
// whether any negating spelling appears is a denylist, which does not.
var registrantTermRE = regexp.MustCompile(
	`(?i)(\b(?:not|non|never|without|excluding|exclude)\s+|\b(?:un|non|de|pre)-?)?\b?(?:registrant|registration|registered|attendee|attended)`,
)

// namesRegistrants reports how name mentions registration.
//
// `strings.Contains(name, "registered")` -- what this replaced -- could not tell this event's
// registrants from the people who have NOT registered, because "unregistered" contains
// "registered". That made "KubeCon 2026 - Unregistered Prospects" fail QA as CRITICAL: the one
// list a registration-push send most obviously SHOULD include, reported as the list it must
// exclude. Nine such names were enumerated, every one of them matching before this.
//
// A name counts as plain when ANY occurrence is un-negated, not when the first one is. "Not
// Registered - Event Registration List" names both, and the un-negated mention is the one that
// decides it: the list does hold registrants.
func namesRegistrants(name string) registrantNaming {
	matches := registrantTermRE.FindAllStringSubmatch(name, -1)
	if len(matches) == 0 {
		return registrantNamingNone
	}
	for _, match := range matches {
		if match[1] == "" {
			return registrantNamingPlain
		}
	}
	return registrantNamingNegated
}

// CheckCurrentRegistrants asks whether THIS edition's own registration list is being
// included rather than suppressed.
//
// The whole point of a registration-push send is reaching people who have NOT registered.
// An event's own registration list belongs in the suppressions; included, every invitation
// goes to someone who already holds a ticket. Verified on AGNTCon + MCPCon North America
// (2026-09-30): the edition's registration list, 1,346 contacts, was offered as an INCLUDE,
// ticking it raised nothing, and QA passed -- the three existing checks all look at consent
// suppression or filter shape, and none of them reads the audience's own INTENT.
//
// SCOPED TO A REGISTRATION-ORIENTED SEND, and the contract cannot yet say so. `Post-Event`
// is a real stage (`internal/service/emailstage/stage.go`), and for one this edition's own
// attendees are the intended audience -- so this finding would be wrong there. The QA payload
// carries no stage today, so there is nothing to gate on.
//
// That is safe only because the check is DORMANT: `event_name` is optional and no client sends
// it, so check 4 is omitted from every audit in production. The gate belongs in the same change
// that wires `event_name` through from the UI, where the stage is known -- adding a stage
// attribute to the contract now would be guessing at a shape before there is a caller to fit.
// Until then this must not be enabled for a `Post-Event` send -- tracked as #244.
//
// Keyed on the registration SIGNAL plus the event's own name, not on the name alone. A
// portfolio contains many registration lists and including a PAST edition's is the correct
// and common case -- that is the strongest evidence available for a first-edition send. Only
// THIS edition's is wrong, so a check that fired on any registration list would be wrong far
// more often than right, and would be switched off.
//
// `eventName` empty is NEEDS VERIFY, never a pass. The caller may legitimately not know the
// event (QA can be run on a bare list id), and an audit that silently skipped would be
// indistinguishable from one that looked and found nothing.
func CheckCurrentRegistrants(eventName string, includedNames []string) Check {
	// NO event name means the check DID NOT RUN, which is the zero Check -- an empty verdict and
	// no findings.
	//
	// NEEDS VERIFY would be wrong here, and wrongly in the direction that gets a check removed.
	// Every caller today supplies no event name (the UI is not wired yet), so returning a
	// verdict would flip EVERY existing audit's `Overall` to NEEDS VERIFY and append a finding
	// to it -- a check that cannot run making every unrelated audit look worse.
	//
	// The distinction that matters: a caller who gave no event name did not ask this question,
	// so there is nothing to report. A caller who gave one this check cannot DECIDE -- a
	// year-less name, an all-generic name -- did ask, and gets NEEDS VERIFY naming what to
	// confirm by hand. The empty Check is what `currentRegistrantsResult` omits from the wire.
	if strings.TrimSpace(eventName) == "" {
		return Check{}
	}

	// The event's tokens AND its year, both required.
	//
	// `NewLastSentTerms` STRIPS the year on purpose -- it exists to find PAST editions -- so
	// matching on its terms alone fired on exactly the lists that are correct to include. A
	// past edition's registrants are the strongest evidence a first-edition send has, and a
	// sibling region ("AGNTCon Japan 2026") is a different event entirely. Measured before
	// fixing: terms-only flagged both as FAIL.
	//
	// The year is what separates THIS edition from its own history, and the tokens are what
	// separate it from a sibling. Requiring both is the only pair that leaves all three
	// correct cases passing.
	terms := NewLastSentTerms(eventName, "")
	// `isOtherEdition` is what separates a sibling region, not the token count.
	//
	// The token rule this replaced was a SUBSET test: a sibling's list is a strict token
	// superset, so whenever the event name omitted a region -- the common portfolio shape --
	// the sibling was flagged. Measured: event "KubeCon 2026" FAILed
	// "26Q1 KubeCon Europe 2026 Event Registration", and "PyTorch Conference 2026" FAILed the
	// Japan edition. Four successive token rules each produced a false positive on a list an
	// operator legitimately chose, because the question is about REGIONS and tokens cannot ask
	// it: `editionRegions` carries the alias table and the latin/south disambiguation.
	eventRegions := editionRegions(eventName)
	// An event name made ENTIRELY of portfolio-common words has an EMPTY distinctive tier, and
	// with no region named either there is nothing left to judge on. Measured on
	// "Open Source Summit 2026": the edition's own list and "Open Source Summit Japan 2026"
	// BOTH score overlap=3 against the same 3 generic tokens.
	//
	// Reported as NEEDS VERIFY rather than guessed in either direction. Flagging would hit a
	// sibling's list, which is correct to include; passing would miss this edition's own, which
	// is the defect the check exists for. Neither is defensible, so the operator is asked.
	if len(terms.Event) == 0 && len(eventRegions) == 0 {
		return Check{
			Verdict: VerdictNeedsVerify,
			Findings: []Finding{{
				Severity: SeverityMedium,
				Message:  fmt.Sprintf("Could not check whether this event's own registrants are suppressed: every word in %q is common across the portfolio, so this edition cannot be told from a sibling region's.", eventName),
				Fix:      "Confirm by hand that the registration list among the inclusions is an EARLIER edition's, and that this edition's own registrants are excluded.",
			}},
		}
	}
	year := eventYearFromName(eventName)
	if year == "" {
		// A name with no year cannot be told from its own past editions, and guessing the
		// current year would flag a list the operator may have chosen deliberately.
		return Check{
			Verdict: VerdictNeedsVerify,
			Findings: []Finding{{
				Severity: SeverityMedium,
				Message:  fmt.Sprintf("Could not check whether this event's own registrants are suppressed: %q carries no year, so this edition cannot be told from an earlier one.", eventName),
				Fix:      "Confirm by hand that this edition's registration list is excluded rather than included.",
			}},
		}
	}
	findings := make([]Finding, 0, 1)
	// Set when a registration list could not be judged because the event names no region and
	// the list does. Reported only if nothing else FAILED -- a real hit is the more useful
	// answer, and NEEDS VERIFY alongside a CRITICAL would read as the lesser finding.
	undecidable := false
	// Set when a name mentions registration only in a negated form. Tracked separately from
	// `undecidable` because the two have different causes and the operator is told which: the
	// existing message names the region question, which has nothing to do with this.
	negatedName := ""

	for _, name := range includedNames {
		naming := namesRegistrants(name)
		if naming == registrantNamingNone {
			continue
		}
		if !namesTheEdition(name, year) {
			continue
		}
		// A DIFFERENT region is a different event, however many tokens it shares.
		if isOtherEdition(name, eventRegions) {
			continue
		}
		// And the reverse, which `isOtherEdition` cannot answer: the LIST names a region while
		// the EVENT names none. "KubeCon 2026" against "26Q1 KubeCon Europe 2026 Event
		// Registration" -- the list may be this event's regional edition or a sibling's, and
		// the event name carries nothing to decide it. `isOtherEdition` returns false here by
		// design (it has no event region to compare), which reads as "same edition" and is how
		// four successive token rules each produced a false positive on a list an operator
		// legitimately chose.
		//
		// Skipped rather than flagged: a sibling's registration list is correct to include, so
		// flagging it is the error that gets the check switched off. The caller is told below
		// that the audit was incomplete.
		if len(eventRegions) == 0 && len(editionRegions(name)) > 0 {
			undecidable = true
			continue
		}
		// And it must still match the event itself. `MatchLastSent` admits on the distinctive
		// tier, which is the right bar once the region question is settled separately.
		if !MatchLastSent(name, "", terms).Matched {
			continue
		}
		// The negation question is asked LAST, after every edition gate, because it only matters
		// for a name already established as THIS edition's. Asked first -- as it was when this
		// was introduced -- an unrelated event's negated list downgraded the whole audit:
		// "PyTorch 2025 - Unregistered Prospects" turned an AGNTCon 2026 audit into NEEDS VERIFY
		// for a list check 4 is not scoped to at all.
		//
		// A name that mentions registration only in a NEGATED form -- "Unregistered Prospects",
		// "Not Registered", "Never Attended" -- is the audience this send is FOR, not the one it
		// must exclude, so CRITICAL here would fail QA for exactly the right list.
		// `strings.Contains` could not tell them apart: "unregistered" contains "registered".
		//
		// Reported as incomplete rather than skipped, because the opposite reading is also
		// available: a list genuinely named for this edition's registrants could carry a negated
		// word, and silently passing it would hide the very inclusion this check exists to catch.
		// Undecidable is the honest answer, and it is already how this check reports a region it
		// cannot resolve.
		if naming == registrantNamingNegated {
			if negatedName == "" {
				negatedName = name
			}
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Message:  fmt.Sprintf("%q looks like this event's own registration list and it is INCLUDED, so this send would invite people who have already registered.", name),
			Fix:      "Move this list into the exclusions. A registration-push send reaches people who have not registered; this edition's registrants belong in the combined suppression.",
		})
		break
	}

	if len(findings) == 0 && (undecidable || negatedName != "") {
		reported := make([]Finding, 0, 2)
		if undecidable {
			reported = append(reported, Finding{
				Severity: SeverityMedium,
				Message:  fmt.Sprintf("Could not check every included registration list: %q names no region, so a list naming one cannot be told from a sibling edition's.", eventName),
				Fix:      "Confirm by hand that any regional registration list among the inclusions belongs to an EARLIER edition, and that this edition's own registrants are excluded.",
			})
		}
		if negatedName != "" {
			reported = append(reported, Finding{
				Severity: SeverityMedium,
				Message:  fmt.Sprintf("Could not check whether %q is this event's registrants or the people who have NOT registered: it names registration in a negated form.", negatedName),
				Fix:      "Confirm by hand which it is. A list of people who have not registered belongs in the inclusions; this edition's registrants belong in the combined suppression.",
			})
		}
		return Check{Verdict: VerdictNeedsVerify, Findings: reported}
	}
	return Check{Verdict: VerdictFromFindings(findings), Findings: findings}
}

// ExclusionCheck adds how many exclusion segments were found.
type ExclusionCheck struct {
	Check
	ExclusionCount int
}

// CheckExclusionCompleteness asks whether the list excludes anything at all.
//
// Coarse on purpose, and a FAIL rather than a warning: zero exclusions means no
// unsubscribe, no opt-out, and no non-marketable filter is applied, which is a
// send that should not go out regardless of which specific list is missing. It says
// nothing about whether the exclusions present are the RIGHT ones — that is
// CheckSuppression's job.
func CheckExclusionCompleteness(filters []ListFilter) ExclusionCheck {
	count := len(ReferencedListIDs(filters, "NOT_IN_LIST"))
	if count > 0 {
		return ExclusionCheck{Check: Check{Verdict: VerdictPass, Findings: []Finding{}}, ExclusionCount: count}
	}
	return ExclusionCheck{
		Check: Check{Verdict: VerdictFail, Findings: []Finding{{
			Severity: SeverityHigh,
			Message:  "List has zero exclusion segments (no suppression, opt-out, or non-marketing exclusions).",
			Fix:      "Add the required suppression exclusions before this list is used to send.",
		}}},
		ExclusionCount: 0,
	}
}

// VerdictFromFindings derives a check's verdict from its own findings.
//
// Only a CRITICAL fails the check: a missing GDPR list on an EU send blocks, while
// everything else is something the operator must LOOK AT rather than something
// known to be wrong.
func VerdictFromFindings(findings []Finding) Verdict {
	for _, finding := range findings {
		if finding.Severity == SeverityCritical {
			return VerdictFail
		}
	}
	if len(findings) > 0 {
		return VerdictNeedsVerify
	}
	return VerdictPass
}

// CombineVerdicts takes the worst: one FAIL makes the whole list a FAIL.
func CombineVerdicts(verdicts ...Verdict) Verdict {
	worst := VerdictPass
	for _, v := range verdicts {
		switch v {
		case VerdictFail:
			return VerdictFail
		case VerdictNeedsVerify:
			worst = VerdictNeedsVerify
		case VerdictPass:
			// Leaves `worst` as-is.
		case "":
			// A check that did NOT RUN, which check 4 is whenever no event name reached the
			// audit. Stated rather than left to fall through: the empty verdict is now a
			// routine input, and an unexhausted switch made "neither passes nor fails" true
			// by accident of `worst`'s initial value rather than by intent.
		default:
			// An unrecognised verdict resolves to NEEDS VERIFY, never to PASS. This roll-up
			// feeds a send decision, and `severityRank` already applies the same discipline
			// one level down -- an unknown severity sorts last "so a future severity cannot
			// silently outrank a CRITICAL".
			worst = VerdictNeedsVerify
		}
	}
	return worst
}

// OrderFindings sorts findings most-severe first, preserving the order the checks
// produced within a severity.
//
// The order is part of the answer, not presentation: the report's reader acts on
// the top of the list, so a CRITICAL missing-GDPR finding sitting below a MEDIUM
// "confirm this manually" is a compliance failure an operator can plausibly miss.
// Stable within a severity because the checks already emit their findings in the
// order they matter — reordering them would say nothing true.
func OrderFindings(findings []Finding) []Finding {
	out := make([]Finding, len(findings))
	copy(out, findings)
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

// severityRank orders the severities; an unrecognised one sorts last rather than
// first, so a future severity cannot silently outrank a CRITICAL.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	default:
		return 3
	}
}
