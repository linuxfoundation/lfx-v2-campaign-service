// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
//
// include are the lists the send goes to. The caller's own overlap check sees only the ids it
// was given, but this substitutes the standing list for the standard ids and adds the
// registrants list, and either can be a list the send also goes to (an operator including
// the edition's registrants list, say). A list both sent to and suppressed is a master that
// excludes a whole inclusion branch, or a recorded audience dispatch refuses, so every
// resolved exclusion is checked against include: the lists found by name are checked BEFORE
// anything is created or rewritten, and the final set once more before returning, so the
// caller composes or records nothing on a contradiction. Either is an
// audience.ExclusionOverlapError (400) naming the list.
func (x *AudienceExplorer) resolveSuppressions(ctx context.Context, client *hubspot.Client, projectID string, include, suppress []string, registrantsEvent string) (*audience.ComposedList, []string, error) {
	// The registrants filter depends only on the input, so it is built BEFORE the first write: a
	// year-less edition must be a 400 with nothing created or rewritten in the portal.
	edition := strings.TrimSpace(registrantsEvent)
	var registrantsFilter json.RawMessage
	if edition != "" {
		filter, err := audience.CurrentRegistrantsFilter(edition)
		if err != nil {
			return nil, nil, err
		}
		registrantsFilter = filter
	}
	const (
		standingRole    = "the project's standing standard suppression list, which replaces the selected standard suppressions"
		registrantsRole = "the current-registrants list of current_registrants_event"
	)
	overlap := func(l *hubspot.List, id, role string) error {
		if !slices.Contains(include, id) {
			return nil
		}
		name := ""
		if l != nil {
			name = l.Name
		}
		return &audience.ExclusionOverlapError{ListID: id, Name: name, Role: role}
	}

	// Reads only, up to the first write: the standard terms, then each standing list by name.
	rest := audience.UniqueIDs(suppress)
	var standardIDs []string
	useStandard := false
	if len(rest) > 0 {
		rows, err := x.standardRows(ctx, client, true)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve standard suppressions: %w", err)
		}
		standardIDs = resolvedIDs(rows)
		useStandard, rest = audience.SplitStandardSuppressions(rest, standardIDs)
	}
	var standingHit, registrantsHit *hubspot.List
	if useStandard {
		name := audience.StandardSuppressionName(projectID)
		hit, err := exactList(ctx, client, name)
		if err != nil {
			return nil, nil, fmt.Errorf("find or create standard suppression list %q: %w", name, err)
		}
		if hit != nil {
			if oerr := overlap(hit, hit.ListID, standingRole); oerr != nil {
				return nil, nil, oerr
			}
		}
		standingHit = hit
	}
	if edition != "" {
		name := audience.CurrentRegistrantsName(edition)
		hit, err := exactList(ctx, client, name)
		if err != nil {
			return nil, nil, fmt.Errorf("find or create current registrants list %q: %w", name, err)
		}
		if hit != nil {
			if oerr := overlap(hit, hit.ListID, registrantsRole); oerr != nil {
				return nil, nil, oerr
			}
		}
		registrantsHit = hit
	}

	// Writes: create or grow what the reads found, or did not.
	var standard *audience.ComposedList
	if useStandard {
		var err error
		if standard, err = x.ensureStandardList(ctx, client, projectID, standardIDs, standingHit); err != nil {
			return nil, nil, err
		}
		if oerr := overlap(nil, standard.ListID, standingRole); oerr != nil {
			return nil, nil, oerr
		}
	}
	if edition != "" {
		registrants, err := x.ensureRegistrantsList(ctx, client, projectID, edition, registrantsFilter, registrantsHit)
		if err != nil {
			return nil, nil, err
		}
		if oerr := overlap(nil, registrants.ListID, registrantsRole); oerr != nil {
			return nil, nil, oerr
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

// findOrCreateStanding returns hit, the list resolveSuppressions already found under name,
// or creates name from filter when there was none. Exactly one of found (an existing list the
// caller must still verify) and created is set.
//
// HubSpot list names are unique per portal, so a create can be refused because the name
// already exists: a concurrent attach or compose created it first, or an earlier create
// landed and search has not indexed it yet. A create can also be UNCONFIRMED. Either way the
// list may well be there, so the name is looked up again, with a short bounded backoff, and
// what is found is used; only when it is still not found does the create's error stand,
// with its classification intact. A permission rejection is not retried: it creates nothing
// and no amount of looking will find a list.
func (x *AudienceExplorer) findOrCreateStanding(ctx context.Context, client *hubspot.Client, projectID, name string, filter json.RawMessage, hit *hubspot.List) (found *hubspot.List, created *audience.ComposedList, err error) {
	if hit != nil {
		return hit, nil, nil
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
	if refused {
		// Still not found among contact lists, yet the create was refused: the name may be held
		// by a list of ANOTHER object type, which SearchLists never returns (names are unique per
		// portal across types). Name it as the conflict it is instead of a bare refusal.
		if other := otherTypeList(ctx, client, name); other != nil {
			return nil, nil, &audience.StandingListConflictError{Name: other.Name, ListID: other.ListID,
				Reason: "holds this standing list's name but is not a contact list"}
		}
	}
	return nil, nil, cerr
}

// otherTypeList is the non-contact list named name (case-insensitively), or nil -- also when
// the lookup itself fails, since it only refines an error the caller already has.
func otherTypeList(ctx context.Context, client *hubspot.Client, name string) *hubspot.List {
	name = strings.TrimSpace(name)
	lists, err := client.SearchListsAnyType(ctx, name)
	if err != nil {
		slog.WarnContext(ctx, "standing list name lookup across object types failed", "error", err)
		return nil
	}
	for i := range lists {
		ot := lists[i].ObjectTypeID
		if ot != "" && ot != "0-1" && strings.EqualFold(strings.TrimSpace(lists[i].Name), name) {
			l := lists[i]
			return &l
		}
	}
	return nil
}

// standingGrowAttempts bounds how many times ensureStandardList writes the standing list's
// filter before giving up on a list that keeps being replaced under it.
const standingGrowAttempts = 3

// reusableStanding refuses a list found under a standing name that the service cannot use as
// that list: not a contact list, or not DYNAMIC (a static list cannot carry a filter). what
// names the standing list for the message.
func reusableStanding(l *hubspot.List, what string) error {
	if l.ObjectTypeID != "" && l.ObjectTypeID != "0-1" {
		return &audience.StandingListConflictError{Name: l.Name, ListID: l.ListID,
			Reason: "has " + what + " name but is not a contact list"}
	}
	if !strings.EqualFold(strings.TrimSpace(l.ProcessingType), "DYNAMIC") {
		return &audience.StandingListConflictError{Name: l.Name, ListID: l.ListID,
			Reason: "has " + what + " name but is not a DYNAMIC list"}
	}
	return nil
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
// HubSpot has no conditional filter update, so a read-merge-write can race another writer (a
// concurrent attach or compose, or an operator) that replaces the filter from an older read.
// Every write is therefore confirmed by reading the list back: when anything this call meant
// to keep is missing, it is merged in again, up to standingGrowAttempts writes, and the call
// then fails rather than report a list that does not cover what it claims. This is not a lock:
// a stale overwrite that lands after this call's confirming read is not seen here; the next
// attach or compose with the full standard set re-adds any resolved standard list it dropped.
//
// A list found by name is reused only when it is a DYNAMIC contact list, and is grown only
// when its filter is a pure union of lists that the rewrite reproduces. Anything else is a
// StandingListConflictError naming the list.
func (x *AudienceExplorer) ensureStandardList(ctx context.Context, client *hubspot.Client, projectID string, ids []string, hit *hubspot.List) (*audience.ComposedList, error) {
	name := audience.StandardSuppressionName(projectID)
	filter, err := audience.MasterListFilter(ids)
	if err != nil {
		return nil, err
	}
	existing, created, err := x.findOrCreateStanding(ctx, client, projectID, name, filter, hit)
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
	const what = "the project's standard suppression list"
	want := audience.UniqueIDs(ids)
	for attempt := 1; ; attempt++ {
		if rerr := reusableStanding(current, what); rerr != nil {
			return nil, rerr
		}
		covered := audience.CoveredListIDs(current.FilterBranch)
		want = audience.UniqueIDs(append(append([]string(nil), covered...), want...))
		if len(want) == len(covered) {
			return &audience.ComposedList{ListRow: listRow(current)}, nil
		}
		if attempt > standingGrowAttempts {
			return nil, fmt.Errorf("standard suppression list %s was replaced by another writer on each of %d attempts to grow it; retry", current.ListID, standingGrowAttempts)
		}
		if !audience.StandingFilterIsUnion(current.FilterBranch) {
			return nil, &audience.StandingListConflictError{Name: current.Name, ListID: current.ListID,
				Reason: "must gain a standard suppression list, but its filter holds conditions other than whole lists that a rewrite would drop"}
		}
		grown, ferr := audience.MasterListFilter(want)
		if ferr != nil {
			return nil, ferr
		}
		if uerr := client.UpdateListFilters(ctx, current.ListID, grown); uerr != nil {
			return nil, fmt.Errorf("grow standard suppression list %s: %w", current.ListID, uerr)
		}
		if current, err = client.GetList(ctx, current.ListID); err != nil {
			return nil, fmt.Errorf("confirm standard suppression list %s: %w", existing.ListID, err)
		}
	}
}

// ensureRegistrantsList finds or creates the list of the edition's current registrants, from
// the filter resolveSuppressions already built and validated.
//
// A list found by name is reused only when it is a DYNAMIC contact list whose filter selects
// all of this edition's registrations and nobody else (audience.RegistrantsListMatches). The
// name match is case-insensitive and anyone can create a list under any name, so reusing on the
// name alone could suppress a different edition's registrants, only some of them, or nobody;
// such a list is a StandingListConflictError naming it instead.
func (x *AudienceExplorer) ensureRegistrantsList(ctx context.Context, client *hubspot.Client, projectID, edition string, filter json.RawMessage, hit *hubspot.List) (*audience.ComposedList, error) {
	name := audience.CurrentRegistrantsName(edition)
	existing, created, err := x.findOrCreateStanding(ctx, client, projectID, name, filter, hit)
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
	if rerr := reusableStanding(current, "this edition's current-registrants list"); rerr != nil {
		return nil, rerr
	}
	if !audience.RegistrantsListMatches(current.FilterBranch, edition) {
		return nil, &audience.StandingListConflictError{Name: current.Name, ListID: current.ListID,
			Reason: fmt.Sprintf("has this edition's current-registrants list name but does not select exactly the registrants of %q", strings.TrimSpace(edition))}
	}
	return &audience.ComposedList{ListRow: listRow(current)}, nil
}
