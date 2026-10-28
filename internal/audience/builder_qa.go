// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	"unicode"
)

// ---------------------------------------------------------------------------
// Pre-send QA on a HubSpot contact list (LFXV2-2770)
//
// Four checks, each answering a question that has cost a real send: does this
// list select on anything a contact actually DID, are the consent suppressions
// genuinely applied as exclusions, does it exclude anything at all, and (check 4,
// current_registrants) does it leave out the event's own current registrants.
// Check 4 reads registration evidence from the filter trees themselves:
// RegistrationEvidence for the editions a list's registration branches include,
// CoveredEditions for those a suppression excludes whole, and
// ExcludedOnEveryPathIDs for the exclusions a list applies on every path.
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

// RegistrationEvidence reads a list's filter tree for the editions whose
// registrants it explicitly includes: every completed-registration branch with an
// exact event_name equality is POSITIVE evidence for its editions, wherever it sits
// ("Seoul location OR registered for KubeCon 2026" includes 2026's registrants).
// Negated, negative-comparison or substring branches add nothing: they name no edition
// the list provably includes.
//
// exclusive says those editions are the WHOLE membership: every path runs through an
// exact completed-registration branch, and no registration branch is inexact. Only
// then may a caller set the list's display name aside as evidence; otherwise an
// unrestricted or membership branch can hold any edition's registrants, and the name
// stays in play beside the editions.
func RegistrationEvidence(raw json.RawMessage) (editions []string, exclusive bool) {
	var root qaBranch
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil, false
	}
	allExact := true
	sawRegistration := false
	root.walk(func(b qaBranch) {
		if !strings.EqualFold(b.FilterBranchType, "UNIFIED_EVENTS") || b.EventTypeID != registrationEventTypeID {
			return
		}
		sawRegistration = true
		found, exact := b.exactEditions()
		if !exact {
			allExact = false
			return
		}
		editions = append(editions, found...)
	})
	editions = UniqueIDs(editions)
	exclusive = sawRegistration && allExact && len(editions) > 0 && root.everyPathRegistered()
	return editions, exclusive
}

// RegisteredEditions are the editions a list's tree selects when they are its WHOLE
// membership (RegistrationEvidence's exclusive case), or nil otherwise.
func RegisteredEditions(raw json.RawMessage) []string {
	editions, exclusive := RegistrationEvidence(raw)
	if !exclusive {
		return nil
	}
	return editions
}

// CoveredEditions are the editions whose registrants a list's tree includes whole: a
// completed-registration branch with an exact event_name equality and NO other
// condition, standing alone as one OR alternative. A suppression with that shape
// excludes every registrant of those editions; one narrowed by anything else covers
// none of them.
func CoveredEditions(raw json.RawMessage) []string {
	var root qaBranch
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil
	}
	return UniqueIDs(root.coveredEditions())
}

// ExcludedOnEveryPathIDs are the lists EVERY path through a list's filter tree
// excludes (a NOT_IN_LIST on each path), as a composed master excludes its combined
// suppression. Such an exclusion applies to the whole list, like a send-time
// suppression; a NOT_IN_LIST on only one branch does not.
func ExcludedOnEveryPathIDs(raw json.RawMessage) []string {
	var root qaBranch
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil
	}
	set := root.requiredOp("NOT_IN_LIST")
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RequiredInListIDs are the lists EVERY contact of a list's filter tree must be a
// member of: an IN_LIST filter on every path through it (AND accumulates, OR keeps
// only what all its alternatives require). A flattened reference proves nothing
// about this. For "A OR B", suppressing A still leaves B minus A, so A is a real
// exclusion; only a list the whole audience is inside would exclude the audience
// itself. An unreadable tree requires nothing.
func RequiredInListIDs(raw json.RawMessage) []string {
	var root qaBranch
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil
	}
	set := root.required()
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CoveredListIDs are the lists whose WHOLE membership a list's filter tree includes:
// a list that is, alone, one alternative of an OR ("R OR OptOut" covers R and OptOut).
// An AND of several conditions covers nothing ("R AND OptOut" is only part of R). It
// is how a suppression wrapper proves its children are excluded at send time.
func CoveredListIDs(raw json.RawMessage) []string {
	var root qaBranch
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil
	}
	set := root.covered()
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// TreeCoveredBy reports whether excluding the lists in covered leaves nobody in the
// filter tree: every path through it requires membership of one of them (an AND
// branch is gone when any one of its conditions is a covered list; an OR is gone
// only when every alternative is). An unreadable or empty tree is not covered.
func TreeCoveredBy(raw json.RawMessage, covered []string) bool {
	var root qaBranch
	if len(raw) == 0 || len(covered) == 0 || json.Unmarshal(raw, &root) != nil {
		return false
	}
	set := make(map[string]struct{}, len(covered))
	for _, id := range covered {
		set[id] = struct{}{}
	}
	return root.coveredBy(set)
}

type qaBranch struct {
	FilterBranchType string     `json:"filterBranchType"`
	EventTypeID      string     `json:"eventTypeId"`
	Operator         string     `json:"operator"`
	FilterBranches   []qaBranch `json:"filterBranches"`
	Filters          []struct {
		FilterType string      `json:"filterType"`
		Operator   string      `json:"operator"`
		ListID     json.Number `json:"listId"`
		Property   string      `json:"property"`
		Operation  struct {
			Operator string   `json:"operator"`
			Values   []string `json:"values"`
		} `json:"operation"`
	} `json:"filters"`
}

func (b qaBranch) walk(visit func(qaBranch)) {
	visit(b)
	for _, c := range b.FilterBranches {
		c.walk(visit)
	}
}

// exactEditions reads a registration event branch: its editions, and whether it is a
// completed registration constrained by an exact event_name equality and nothing
// that could widen or negate it.
func (b qaBranch) exactEditions() ([]string, bool) {
	if !strings.EqualFold(strings.TrimSpace(b.Operator), "HAS_COMPLETED") {
		return nil, false
	}
	// The branch's conditions ALL hold, so several event_name equalities narrow each
	// other: the editions are what every one of them allows, never their union.
	var editions []string
	seen := false
	for _, f := range b.Filters {
		if !strings.EqualFold(strings.TrimSpace(f.Property), "event_name") {
			continue
		}
		op := strings.ToUpper(strings.TrimSpace(f.Operation.Operator))
		if op != "IS_EQUAL_TO" && op != "IS_ANY_OF" {
			return nil, false
		}
		if !seen {
			editions = UniqueIDs(append([]string(nil), f.Operation.Values...))
			seen = true
			continue
		}
		allowed := make(map[string]bool, len(f.Operation.Values))
		for _, v := range f.Operation.Values {
			allowed[strings.ToLower(strings.TrimSpace(v))] = true
		}
		kept := editions[:0]
		for _, e := range editions {
			if allowed[strings.ToLower(strings.TrimSpace(e))] {
				kept = append(kept, e)
			}
		}
		editions = kept
	}
	return editions, len(editions) > 0
}

// everyPathRegistered reports whether every path through the tree requires an exact
// completed-registration branch: an AND path does when any of its conditions is one;
// an OR only when every alternative does (a direct filter there is an alternative
// that does not).
func (b qaBranch) everyPathRegistered() bool {
	switch strings.ToUpper(b.FilterBranchType) {
	case "UNIFIED_EVENTS":
		if b.EventTypeID != registrationEventTypeID {
			return false
		}
		_, exact := b.exactEditions()
		return exact
	case "AND":
		for _, c := range b.FilterBranches {
			if c.everyPathRegistered() {
				return true
			}
		}
		return false
	case "OR":
		if len(b.Filters) > 0 || len(b.FilterBranches) == 0 {
			return false
		}
		for _, c := range b.FilterBranches {
			if !c.everyPathRegistered() {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (b qaBranch) coveredEditions() []string {
	switch strings.ToUpper(b.FilterBranchType) {
	case "OR":
		var out []string
		for _, c := range b.FilterBranches {
			out = append(out, c.coveredEditions()...)
		}
		return out
	case "AND":
		if len(b.Filters) == 0 && len(b.FilterBranches) == 1 {
			return b.FilterBranches[0].coveredEditions()
		}
		return nil
	case "UNIFIED_EVENTS":
		if b.EventTypeID != registrationEventTypeID {
			return nil
		}
		// Every filter must be the exact event_name equality: any other condition
		// narrows the branch below an edition's whole registration.
		for _, f := range b.Filters {
			if !strings.EqualFold(strings.TrimSpace(f.Property), "event_name") {
				return nil
			}
		}
		if len(b.FilterBranches) > 0 {
			return nil
		}
		editions, exact := b.exactEditions()
		if !exact {
			return nil
		}
		return editions
	default:
		return nil
	}
}

func (b qaBranch) covered() map[string]struct{} {
	inList := func(i int) (string, bool) {
		f := b.Filters[i]
		id := strings.TrimSpace(f.ListID.String())
		return id, f.FilterType == "IN_LIST" && f.Operator == "IN_LIST" && id != ""
	}
	out := map[string]struct{}{}
	switch strings.ToUpper(b.FilterBranchType) {
	case "OR":
		for i := range b.Filters {
			if id, ok := inList(i); ok {
				out[id] = struct{}{}
			}
		}
		for _, c := range b.FilterBranches {
			for id := range c.covered() {
				out[id] = struct{}{}
			}
		}
	case "AND":
		// Only a single condition: anything more narrows it below the whole list.
		switch {
		case len(b.Filters) == 1 && len(b.FilterBranches) == 0:
			if id, ok := inList(0); ok {
				out[id] = struct{}{}
			}
		case len(b.Filters) == 0 && len(b.FilterBranches) == 1:
			return b.FilterBranches[0].covered()
		}
	}
	return out
}

func (b qaBranch) coveredBy(set map[string]struct{}) bool {
	inSet := func(i int) bool {
		f := b.Filters[i]
		_, ok := set[strings.TrimSpace(f.ListID.String())]
		return ok && f.FilterType == "IN_LIST" && f.Operator == "IN_LIST"
	}
	switch strings.ToUpper(b.FilterBranchType) {
	case "AND":
		for i := range b.Filters {
			if inSet(i) {
				return true
			}
		}
		for _, c := range b.FilterBranches {
			if c.coveredBy(set) {
				return true
			}
		}
		return false
	case "OR":
		alternatives := 0
		for i := range b.Filters {
			alternatives++
			if !inSet(i) {
				return false
			}
		}
		for _, c := range b.FilterBranches {
			alternatives++
			if !c.coveredBy(set) {
				return false
			}
		}
		return alternatives > 0
	default:
		return false
	}
}

func (b qaBranch) required() map[string]struct{} { return b.requiredOp("IN_LIST") }

// requiredOp is required() for a membership operator: IN_LIST for lists every path is
// inside, NOT_IN_LIST for lists every path excludes.
func (b qaBranch) requiredOp(operator string) map[string]struct{} {
	inList := func(i int) (string, bool) {
		f := b.Filters[i]
		id := strings.TrimSpace(f.ListID.String())
		return id, f.FilterType == "IN_LIST" && f.Operator == operator && id != ""
	}
	switch strings.ToUpper(b.FilterBranchType) {
	case "AND":
		out := map[string]struct{}{}
		for i := range b.Filters {
			if id, ok := inList(i); ok {
				out[id] = struct{}{}
			}
		}
		for _, c := range b.FilterBranches {
			for id := range c.requiredOp(operator) {
				out[id] = struct{}{}
			}
		}
		return out
	case "OR":
		var alternatives []map[string]struct{}
		for i := range b.Filters {
			alt := map[string]struct{}{}
			if id, ok := inList(i); ok {
				alt[id] = struct{}{}
			}
			alternatives = append(alternatives, alt)
		}
		for _, c := range b.FilterBranches {
			alternatives = append(alternatives, c.requiredOp(operator))
		}
		if len(alternatives) == 0 {
			return map[string]struct{}{}
		}
		out := alternatives[0]
		for _, alt := range alternatives[1:] {
			for id := range out {
				if _, ok := alt[id]; !ok {
					delete(out, id)
				}
			}
		}
		return out
	default:
		return map[string]struct{}{}
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
	// registrantNamingNegated: every registration term in the name is negated or qualified.
	registrantNamingNegated
)

// namesRegistrants reports how name mentions registration.
//
// `strings.Contains(name, "registered")` -- the first version of this -- could not tell this
// event's registrants from the people who have NOT registered, because "unregistered" contains
// "registered". That made "Unregistered Prospects" fail QA as CRITICAL: the one list a
// registration-push send most obviously SHOULD include, reported as the list it must exclude.
//
// The second version matched the term together with an adjacent negation using one regex, and
// three separate holes came out of a review of it -- each a case where the pattern judged the RAW
// string and the raw string did not carry the structure the pattern assumed:
//
//   - "Not Yet Registered" and "Haven't Registered" read as PLAIN, because the negation was
//     required to sit immediately before the term. Any intervening word reopened the false
//     CRITICAL this predicate exists to close.
//   - "Co-Registrants" read as PLAIN through an optional `\b?` that could match empty, so a bare
//     stem matched mid-word. "xregistrants" and "bioregistration" matched too.
//   - "Ünregistered" read as PLAIN because Go's `\b` is ASCII-only: there is no boundary between
//     "Ü" and "n", so the `un` prefix never matched. "REGİSTRANTS" went the other way and read as
//     NO mention at all, missing a real registrant list.
//
// So this judges TOKENS of a normalised form rather than substrings of the raw name, and asks the
// three questions separately rather than encoding them in one pattern:
//
//  1. Accents are folded and case lowered first, so "Ünregistered" and "REGİSTRANTS" reduce to
//     forms the token rules below can read. A list name is operator-typed and arrives from a
//     HubSpot portal, so it carries whatever the operator pasted.
//  2. A name is split into PHRASES on separators that end a thought -- a spaced dash, a comma,
//     brackets, a pipe, a slash. A negation reaches only to the end of its own phrase, which is
//     what keeps "Not Interested - Registration List" and "Registration - No Discount" plain:
//     both carry a negating word negating something else, in a different phrase.
//  3. Within a phrase a hyphen and an apostrophe JOIN rather than separate, so "Pre-registration"
//     and "Haven't" survive as single tokens. A fused prefix (`un`, `de`, `pre`, `re`, `mis`,
//     `anti`, `co`) is itself the qualification and needs no separate negating word.
//
// A name counts as PLAIN when any phrase carries an un-negated term, not when the first one does:
// "Not Registered - Event Registration List" names both, and the un-negated mention is the one
// that decides it -- the list does hold registrants.
func namesRegistrants(name string) registrantNaming {
	anyTerm, anyPlain := false, false
	for _, phrase := range registrantPhraseSplitRE.Split(foldForNaming(name), -1) {
		negated := false
		for _, word := range registrantWords(phrase) {
			switch {
			case registrantNegationWords[word]:
				negated = true
			case registrantFusedTermRE.MatchString(word):
				// A fused prefix carries its own qualification, so the term is never plain here.
				anyTerm = true
			case registrantTermRE.MatchString(word):
				anyTerm = true
				if !negated {
					anyPlain = true
				}
			}
		}
	}
	switch {
	case !anyTerm:
		return registrantNamingNone
	case anyPlain:
		return registrantNamingPlain
	default:
		return registrantNamingNegated
	}
}

// registrantNameFold strips accents so a decorated spelling reduces to the ASCII form the token
// rules read. `unicode.Mn` is the nonspacing-mark category NFD decomposition produces.
var registrantNameFold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// foldForNaming lower-cases and de-accents. On a transform error it still lower-cases, so the
// ASCII majority of a malformed name is read rather than the whole name being skipped.
func foldForNaming(name string) string {
	folded, _, err := transform.String(registrantNameFold, name)
	if err != nil {
		return strings.ToLower(name)
	}
	return strings.ToLower(folded)
}

// registrantPhraseSplitRE splits on separators that END a thought, so a negation cannot reach
// across one. A SPACED dash only: "Pre-registration" must stay one token.
var registrantPhraseSplitRE = regexp.MustCompile(`\s+[-\x{2013}\x{2014}]\s+|[,()\[\]|/]+`)

// registrantWords splits a phrase into words, keeping internal `-` and `'` so a fused prefix
// ("pre-registration") and a contraction ("haven't") survive as one token each.
func registrantWords(phrase string) []string {
	return strings.FieldsFunc(phrase, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '\''
	})
}

// registrantTermRE matches a bare registration term, anchored so it cannot match mid-word.
var registrantTermRE = regexp.MustCompile(`^(?:registrant|registration|registered|attendee|attended)s?$`)

// registrantFusedTermRE matches a registration term carrying a prefix that qualifies it.
// `co` and `re` are here with the negations because neither names THIS edition's own registrant
// set either -- a co-registrant or a re-registration is not the population the check judges.
var registrantFusedTermRE = regexp.MustCompile(`^(?:un|non|de|pre|re|mis|anti|co)-?(?:registrant|registration|registered|attendee|attended)s?$`)

// registrantNegationWords are the words that negate a registration term later in their phrase.
// Both the apostrophe and bare spellings, because an operator types either.
var registrantNegationWords = map[string]bool{
	"not": true, "non": true, "never": true, "without": true, "excluding": true, "exclude": true,
	"no":      true,
	"haven't": true, "hasn't": true, "didn't": true, "don't": true, "won't": true,
	"havent": true, "hasnt": true, "didnt": true, "dont": true, "wont": true,
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
	// ExclusionCount is every exclusion that will apply: the list's own plus those
	// applied at send time. SendTimeCount is the send-time share of it.
	ExclusionCount int
	SendTimeCount  int
}

// CheckExclusionCompleteness asks whether the send excludes anything at all.
//
// Coarse on purpose, and a FAIL rather than a warning: zero exclusions means no
// unsubscribe, no opt-out, and no non-marketable filter is applied, which is a
// send that should not go out regardless of which specific list is missing. It says
// nothing about whether the exclusions present are the RIGHT ones — that is
// CheckSuppression's job.
//
// sendTime counts suppression lists that are applied when the email is sent rather
// than baked into the list — the builder's design for custom lists and for lists
// attached without a composed master. They exclude the same contacts, so they count.
func CheckExclusionCompleteness(filters []ListFilter, sendTime int) ExclusionCheck {
	count := len(ReferencedListIDs(filters, "NOT_IN_LIST")) + sendTime
	if count > 0 {
		return ExclusionCheck{Check: Check{Verdict: VerdictPass, Findings: []Finding{}}, ExclusionCount: count, SendTimeCount: sendTime}
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
