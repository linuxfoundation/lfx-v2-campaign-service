// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// standardRows resolves every StandardSuppressionTerms entry to its newest matching list. A
// term that does not resolve is returned with an empty ListID rather than dropped: an operator
// who cannot see that the GDPR row is unavailable will assume it was applied.
//
// strict makes a failed search an error instead of an unresolved row. The read path (the
// picker) stays lenient; the write path must be strict, because resolveSuppressions writes the
// resolved set into the shared standing list, and a 429 on one term would otherwise remove that
// hygiene list from every audience that excludes it.
func (x *AudienceExplorer) standardRows(ctx context.Context, client *hubspot.Client, strict bool) ([]audience.SuppressionRow, error) {
	match := x.bestMatch
	if strict {
		match = x.bestMatchStrict
	}
	out := make([]audience.SuppressionRow, 0, len(audience.StandardSuppressionTerms))
	for _, term := range audience.StandardSuppressionTerms {
		row := audience.SuppressionRow{Key: term.Key, Label: term.Label, Category: audience.SuppressionCategoryStandard}
		hit, err := match(ctx, client, []string{term.SearchTerm}, func(name string) bool {
			return audience.MatchesStandardSuppression(name, term.SearchTerm)
		})
		if err != nil {
			return nil, err
		}
		if hit != nil {
			row.ListID, row.Name, row.Size, row.HubSpotURL = hit.ListID, hit.Name, sizeOf(hit), hit.AppURL
		}
		out = append(out, row)
	}
	return out, nil
}

// resolvedIDs are the list ids of the rows that resolved.
func resolvedIDs(rows []audience.SuppressionRow) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ListID != "" {
			ids = append(ids, r.ListID)
		}
	}
	return ids
}

// resolveSuppressions turns an operator's suppression selection into what the send records:
// the project's standing standard list (reused, never one per audience) plus every other
// selected list as its own entry. When registrantsEvent is set, the current edition's
// registrants list is found or created and added as its own entry too.
//
// Both lists are found by exact name before anything is created, so a repeat or a retry
// reuses them. That also keeps an UNCONFIRMED create safe to retry: if HubSpot did create the
// list, the retry finds it by name, and if search has not indexed it yet, HubSpot refuses the
// duplicate name -- it never builds two -- and findOrCreateStanding looks the name up again.
func (x *AudienceExplorer) resolveSuppressions(ctx context.Context, client *hubspot.Client, projectID string, suppress []string, registrantsEvent string) (*audience.ComposedList, []string, error) {
	// The registrants filter depends only on the input, so it is built BEFORE the first write: a
	// year-less edition must be a 400 with nothing created or rewritten in the portal.
	var registrantsFilter json.RawMessage
	if edition := strings.TrimSpace(registrantsEvent); edition != "" {
		filter, err := audience.CurrentRegistrantsFilter(edition)
		if err != nil {
			return nil, nil, err
		}
		registrantsFilter = filter
	}
	var standard *audience.ComposedList
	rest := audience.UniqueIDs(suppress)
	if len(rest) > 0 {
		rows, err := x.standardRows(ctx, client, true)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve standard suppressions: %w", err)
		}
		ids := resolvedIDs(rows)
		useStandard, others := audience.SplitStandardSuppressions(rest, ids)
		rest = others
		if useStandard {
			if standard, err = x.ensureStandardList(ctx, client, projectID, ids); err != nil {
				return nil, nil, err
			}
		}
	}
	if strings.TrimSpace(registrantsEvent) != "" {
		registrants, err := x.ensureRegistrantsList(ctx, client, projectID, registrantsEvent, registrantsFilter)
		if err != nil {
			return nil, nil, err
		}
		if !slices.Contains(rest, registrants.ListID) {
			rest = append(rest, registrants.ListID)
		}
	}
	return standard, rest, nil
}

// standingRelookupWaits are the pauses before each re-lookup of a standing list whose
// create was refused or unconfirmed. Bounded: three looks over under two seconds.
var standingRelookupWaits = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}

// findOrCreateStanding finds the list named name, creating it from filter when absent.
// Exactly one of found (an existing list the caller must still verify) and created is set.
//
// HubSpot list names are unique per portal, so a create can be refused because the name
// already exists: a concurrent attach or compose created it first, or an earlier create
// landed and search has not indexed it yet. A create can also be UNCONFIRMED. Either way the
// list may well be there, so the name is looked up again, with a short bounded backoff, and
// what is found is used; only when it is still not found does the create's error stand,
// with its classification intact. A permission rejection is not retried: it creates nothing
// and no amount of looking will find a list.
func (x *AudienceExplorer) findOrCreateStanding(ctx context.Context, client *hubspot.Client, projectID, name string, filter json.RawMessage) (found *hubspot.List, created *audience.ComposedList, err error) {
	if found, err = exactList(ctx, client, name); err != nil || found != nil {
		return found, nil, err
	}
	created, cerr := x.createList(ctx, projectID, name, filter)
	if cerr == nil {
		return nil, created, nil
	}
	refused := hubspot.IsDefiniteRejection(cerr) && !hubspot.IsPermissionRejection(cerr)
	if !refused && !hubspot.IsUnconfirmed(cerr) {
		return nil, nil, cerr
	}
	for _, wait := range standingRelookupWaits {
		if serr := x.sleep(ctx, wait); serr != nil {
			return nil, nil, cerr
		}
		hit, lerr := exactList(ctx, client, name)
		if lerr != nil {
			// The create's outcome is what the caller must act on; a failed look does not
			// change it.
			continue
		}
		if hit != nil {
			return hit, nil, nil
		}
	}
	return nil, nil, cerr
}

// ensureStandardList finds the project's standing standard suppression list by name, creating
// it on first use, and keeps it covering every id in ids. It only ever GROWS: the new filter
// is the union of the lists it already covers and ids, written only when that union adds an
// id.
//
// Never shrinking is the point. Every audience in the project excludes this one list, and
// dispatch applies its id at send time, so dropping a member stops suppressing those people
// on every send at once. A member stops appearing in ids for reasons that are not a decision
// to stop suppressing it: a quarter rotation (the new quarter's opt-out list resolves while
// the old one still holds the people not yet copied over), a term that no longer resolves,
// search-index lag, or a list an operator added in HubSpot by hand. An unchanged project costs
// a search and a read, never a write.
//
// A list found by name is reused only when it is DYNAMIC (a static list cannot carry the
// union filter) and, when it must be rewritten, only when its filter is a pure union of lists
// that the rewrite reproduces. Anything else is a StandingListConflictError naming the list.
func (x *AudienceExplorer) ensureStandardList(ctx context.Context, client *hubspot.Client, projectID string, ids []string) (*audience.ComposedList, error) {
	name := audience.StandardSuppressionName(projectID)
	filter, err := audience.MasterListFilter(ids)
	if err != nil {
		return nil, err
	}
	existing, created, err := x.findOrCreateStanding(ctx, client, projectID, name, filter)
	if err != nil {
		return nil, fmt.Errorf("find or create standard suppression list %q: %w", name, err)
	}
	if created != nil {
		return created, nil
	}
	current, err := client.GetList(ctx, existing.ListID)
	if err != nil {
		return nil, fmt.Errorf("read standard suppression list %s: %w", existing.ListID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(current.ProcessingType), "DYNAMIC") {
		return nil, &audience.StandingListConflictError{Name: current.Name, ListID: current.ListID,
			Reason: "has the project's standard suppression list name but is not a DYNAMIC list"}
	}
	covered := audience.CoveredListIDs(current.FilterBranch)
	union := audience.UniqueIDs(append(append([]string(nil), covered...), ids...))
	if len(union) > len(covered) {
		if !audience.StandingFilterIsUnion(current.FilterBranch) {
			return nil, &audience.StandingListConflictError{Name: current.Name, ListID: current.ListID,
				Reason: "must gain a standard suppression list, but its filter holds conditions other than whole lists that a rewrite would drop"}
		}
		grown, ferr := audience.MasterListFilter(union)
		if ferr != nil {
			return nil, ferr
		}
		if uerr := client.UpdateListFilters(ctx, current.ListID, grown); uerr != nil {
			return nil, fmt.Errorf("grow standard suppression list %s: %w", current.ListID, uerr)
		}
	}
	return &audience.ComposedList{ListRow: listRow(current)}, nil
}

// ensureRegistrantsList finds or creates the list of the edition's current registrants, from
// the filter resolveSuppressions already built and validated.
//
// A list found by name is reused only when it is DYNAMIC and its filter selects exactly this
// edition's registrations (audience.RegistrantsListMatches). The name match is
// case-insensitive and anyone can create a list under any name, so reusing on the name alone
// could suppress a different edition's registrants, or nobody; such a list is a
// StandingListConflictError naming it instead.
func (x *AudienceExplorer) ensureRegistrantsList(ctx context.Context, client *hubspot.Client, projectID, edition string, filter json.RawMessage) (*audience.ComposedList, error) {
	name := audience.CurrentRegistrantsName(edition)
	existing, created, err := x.findOrCreateStanding(ctx, client, projectID, name, filter)
	if err != nil {
		return nil, fmt.Errorf("find or create current registrants list %q: %w", name, err)
	}
	if created != nil {
		return created, nil
	}
	current, err := client.GetList(ctx, existing.ListID)
	if err != nil {
		return nil, fmt.Errorf("read current registrants list %s: %w", existing.ListID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(current.ProcessingType), "DYNAMIC") {
		return nil, &audience.StandingListConflictError{Name: current.Name, ListID: current.ListID,
			Reason: "has this edition's current-registrants list name but is not a DYNAMIC list"}
	}
	if !audience.RegistrantsListMatches(current.FilterBranch, edition) {
		return nil, &audience.StandingListConflictError{Name: current.Name, ListID: current.ListID,
			Reason: fmt.Sprintf("has this edition's current-registrants list name but does not select exactly the registrants of %q", strings.TrimSpace(edition))}
	}
	return &audience.ComposedList{ListRow: listRow(current)}, nil
}
