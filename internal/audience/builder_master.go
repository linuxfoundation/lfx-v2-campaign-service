// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Master-list composition (LFXV2-2770)
//
// Naming, quarter ranking, the master filter branch, and the union-count
// arithmetic. All pure; the HubSpot writes themselves live in internal/dispatch,
// because composition is NOT idempotent and the ordering guarantees around a
// partial failure belong next to the calls that can fail.
// ---------------------------------------------------------------------------

// quarterCodeRE matches the `YYQN` code these lists embed in their names, e.g.
// the "26Q1" in "26Q1 - CNCF - KubeCon Europe - Master".
var quarterCodeRE = regexp.MustCompile(`(?i)(\d{2})Q([1-4])`)

// QuarterCode is the `YYQN` code for a date, in UTC.
//
// UTC deliberately: the code is a label on a marketing asset shared across
// timezones, so deriving it from the server's local zone would name the same event
// differently depending on where the process happens to run.
func QuarterCode(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%02dQ%d", t.Year()%100, int(t.Month()-1)/3+1)
}

// EventQuarterCode is the quarter code for an event's first parseable date, or the
// current quarter when it stated none.
//
// Falling back to "now" rather than to a blank segment is intentional: an operator
// composing a list today for an undated event is overwhelmingly working on the
// current quarter, and a name missing its code sorts below every dated list in
// QuarterRank — so the list would become invisible to next quarter's search.
func EventQuarterCode(dates []string, now time.Time) string {
	for _, raw := range dates {
		raw = strings.TrimSpace(raw)
		if !isoDateRE.MatchString(raw) {
			continue
		}
		parsed, err := time.Parse("2006-01-02", raw[:10])
		if err != nil {
			continue
		}
		return QuarterCode(parsed)
	}
	return QuarterCode(now)
}

// MasterListName builds the name a composed list is created under, e.g.
// `26Q1 - CNCF - KubeCon Europe - Master`.
//
// Blank segments are DROPPED, not blanked: a name containing " -  - " reads as a
// bug in the portal's list index, and an operator scanning for their own asset
// would not recognise it.
func MasterListName(id EventIdentity, suffix string, now time.Time) string {
	body := make([]string, 0, 2)
	for _, segment := range []string{id.BrandShort, id.Name} {
		if segment = strings.TrimSpace(segment); segment != "" {
			body = append(body, segment)
		}
	}
	middle := "Audience"
	if len(body) > 0 {
		middle = strings.Join(body, " - ")
	}
	if suffix = strings.TrimSpace(suffix); suffix == "" {
		suffix = "Master"
	}
	return strings.Join([]string{EventQuarterCode(id.Dates, now), middle, suffix}, " - ")
}

// CombinedSuppressionName is the name of the one wrapper list every composed
// master excludes.
//
// An operator-supplied base name is suffixed rather than replaced, so the pair is
// adjacent in an alphabetical list index and obviously belongs together.
func CombinedSuppressionName(baseName string, id EventIdentity, now time.Time) string {
	if baseName = strings.TrimSpace(baseName); baseName != "" {
		return baseName + " - Combined Suppression"
	}
	return MasterListName(id, "Combined Suppression", now)
}

// QuarterRank is a sortable rank from a name's `YYQN` code; `(-1, -1)` when it has
// none.
//
// These lists are rebuilt every quarter under near-identical names, and HubSpot's
// list search returns NO recency ordering at all, so the code in the name is the
// only signal for which of several matches is current. An unnamed-quarter list
// ranks below every dated one rather than above: an undated name is more likely a
// one-off than the newest edition.
func QuarterRank(name string) (int, int) {
	match := quarterCodeRE.FindStringSubmatch(name)
	if len(match) != 3 {
		return -1, -1
	}
	year, yerr := strconv.Atoi(match[1])
	quarter, qerr := strconv.Atoi(match[2])
	if yerr != nil || qerr != nil {
		return -1, -1
	}
	return year, quarter
}

// ListCandidate is a search hit being ranked. It carries only what ranking reads,
// so the ranking can be tested without a HubSpot client.
type ListCandidate struct {
	ListID string
	Name   string
	// Size is -1 when HubSpot did not report one. Zero means a genuinely empty
	// list, and conflating the two would rank an unreported list as the smallest
	// match rather than merely unknown.
	Size int
}

// NewerFirst reports whether a sorts before b: newest quarter first, ties broken
// on the larger list.
//
// Size is the tiebreak because two same-quarter lists with the same category are
// usually a real one and an abandoned draft, and the real one is the one people
// were added to.
func NewerFirst(a, b ListCandidate) bool {
	aYear, aQuarter := QuarterRank(a.Name)
	bYear, bQuarter := QuarterRank(b.Name)
	if aYear != bYear {
		return aYear > bYear
	}
	if aQuarter != bQuarter {
		return aQuarter > bQuarter
	}
	return a.Size > b.Size
}

// ---------------------------------------------------------------------------
// Filter branches
// ---------------------------------------------------------------------------

// MasterListWithSuppressionFilter builds the master's filter branch: the union of
// the inclusion lists, with the combined-suppression list excluded from EVERY
// member of the union.
//
// Two shape rules make this look redundant and are both load-bearing:
//
//  1. The exclusion filter is repeated INSIDE each AND branch. A single exclusion
//     added as a sibling of the AND branches would be OR'd with them — "in list A,
//     OR in list B, OR not in the suppression list" — which matches almost
//     everyone in the portal and silently does nothing about suppression.
//
//  2. The exclusion's `filterType` stays "IN_LIST"; only `operator` becomes
//     "NOT_IN_LIST". There is no NOT_IN_LIST filter TYPE: HubSpot ACCEPTS one and
//     then ignores the filter, so the list is created, looks correct in the API
//     response, and mails the people it was supposed to exclude.
func MasterListWithSuppressionFilter(includeIDs []string, suppressionListID string) (json.RawMessage, error) {
	if len(includeIDs) == 0 {
		return nil, fmt.Errorf("audience: a master list requires at least one inclusion list")
	}
	if strings.TrimSpace(suppressionListID) == "" {
		return nil, fmt.Errorf("audience: a suppressed master list requires the suppression list id")
	}
	branches := make([]filterBranch, 0, len(includeIDs))
	for _, id := range includeIDs {
		if strings.TrimSpace(id) == "" {
			// A blank id would silently drop one group from the union.
			return nil, fmt.Errorf("audience: a master list cannot be built from a blank list id")
		}
		branches = append(branches, filterBranch{
			FilterBranchType: "AND",
			FilterBranches:   []filterBranch{},
			Filters: []filter{
				inListFilter(id, "IN_LIST"),
				inListFilter(suppressionListID, "NOT_IN_LIST"),
			},
		})
	}
	return json.Marshal(filterBranch{
		FilterBranchType: "OR",
		FilterBranches:   branches,
		Filters:          []filter{},
	})
}

// inListFilter builds one membership filter. `operator` chooses the direction; see
// MasterListWithSuppressionFilter for why filterType does not.
func inListFilter(listID, operator string) filter {
	return filter{
		FilterType: "IN_LIST",
		ListID:     listID,
		Operator:   operator,
		Operation:  operation{Operator: operator, OperationType: "MULTISTRING"},
	}
}

// ---------------------------------------------------------------------------
// Selection arithmetic
// ---------------------------------------------------------------------------

// UniqueIDs de-duplicates ids in first-seen order, dropping blanks.
func UniqueIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id == "" {
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

// ExclusionIDs are the suppression selections minus anything also selected for
// inclusion.
//
// Inclusions win. A list selected both ways is an operator mistake, and resolving
// it toward exclusion would build a master list that excludes a group it also
// includes — an audience the operator believes contains people it cannot contain.
func ExclusionIDs(excludeIDs, includeIDs []string) []string {
	included := make(map[string]struct{}, len(includeIDs))
	for _, id := range UniqueIDs(includeIDs) {
		included[id] = struct{}{}
	}
	out := make([]string, 0, len(excludeIDs))
	for _, id := range UniqueIDs(excludeIDs) {
		if _, clash := included[id]; clash {
			continue
		}
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// Union count
// ---------------------------------------------------------------------------

// PreviewCount is the answer to "how many people would this reach", and how much
// to trust it.
//
// `Exact` is the whole point of the type. Above UnionExactCap no exact answer is
// available, and `Count` then carries the SUM — an over-count, which is the safe
// direction. `Reason` is non-empty exactly when Exact is false, so the UI never
// has to decide whether to explain itself.
type PreviewCount struct {
	Exact    bool
	Count    int
	Estimate int
	Reason   string
}

// EmptyPreviewCount is the answer for an empty selection: exactly zero, no
// caveat.
func EmptyPreviewCount() PreviewCount {
	return PreviewCount{Exact: true, Count: 0, Estimate: 0, Reason: ""}
}

// OverCapPreviewCount refuses an exact count and says why, in the operator's
// terms.
func OverCapPreviewCount(estimate int) PreviewCount {
	return PreviewCount{
		Exact:    false,
		Count:    estimate,
		Estimate: estimate,
		Reason: fmt.Sprintf("combined size ~%s exceeds the live-count limit (%s); showing the sum estimate",
			withThousands(estimate), withThousands(UnionExactCap)),
	}
}

// DegradedPreviewCount is the fallback when the live membership sweep could not
// complete — including when a list's membership was TRUNCATED, which would make
// the union an UNDERCOUNT. Understating how many people an email reaches is the
// one direction that must never be reported as exact.
func DegradedPreviewCount(estimate int) PreviewCount {
	return PreviewCount{
		Exact:    false,
		Count:    estimate,
		Estimate: estimate,
		Reason:   "live membership lookup failed; showing the sum estimate",
	}
}

// ExactPreviewCount reports a completed union sweep. `estimate` (the sum) is kept
// alongside it so the UI can show how much overlap the union removed.
func ExactPreviewCount(unionSize, estimate int) PreviewCount {
	return PreviewCount{Exact: true, Count: unionSize, Estimate: estimate, Reason: ""}
}

// ExceedsExactCap reports whether a summed estimate is too large to count
// exactly. STRICTLY greater: an estimate of exactly UnionExactCap is countable,
// because MembershipPageSize * MembershipMaxPages is exactly that many records.
func ExceedsExactCap(estimate int) bool {
	return estimate > UnionExactCap
}

// withThousands renders an integer with comma separators, matching the en-US
// formatting the operator sees everywhere else in the campaign UI.
func withThousands(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return sign + b.String()
}
