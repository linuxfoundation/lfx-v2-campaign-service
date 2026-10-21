// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// standardRows resolves every StandardSuppressionTerms entry to its newest matching list. A
// term that does not resolve is returned with an empty ListID rather than dropped: an operator
// who cannot see that the GDPR row is unavailable will assume it was applied.
func (x *AudienceExplorer) standardRows(ctx context.Context, client *hubspot.Client) ([]audience.SuppressionRow, error) {
	out := make([]audience.SuppressionRow, 0, len(audience.StandardSuppressionTerms))
	for _, term := range audience.StandardSuppressionTerms {
		row := audience.SuppressionRow{Key: term.Key, Label: term.Label, Category: audience.SuppressionCategoryStandard}
		hit, err := x.bestMatch(ctx, client, []string{term.SearchTerm}, func(name string) bool {
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
// duplicate name -- it never builds two.
func (x *AudienceExplorer) resolveSuppressions(ctx context.Context, client *hubspot.Client, projectID string, suppress []string, registrantsEvent string) (*audience.ComposedList, []string, error) {
	var standard *audience.ComposedList
	rest := audience.UniqueIDs(suppress)
	if len(rest) > 0 {
		rows, err := x.standardRows(ctx, client)
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
		registrants, err := x.ensureRegistrantsList(ctx, client, projectID, registrantsEvent)
		if err != nil {
			return nil, nil, err
		}
		if !slices.Contains(rest, registrants.ListID) {
			rest = append(rest, registrants.ListID)
		}
	}
	return standard, rest, nil
}

// ensureStandardList finds the project's standing standard suppression list by name and keeps
// its membership equal to ids, creating it on first use. Its filters are replaced only when the
// resolved standard lists changed (a new quarter's opt-out list, a newly added term), so an
// unchanged project costs a search and a read, and never a write.
func (x *AudienceExplorer) ensureStandardList(ctx context.Context, client *hubspot.Client, projectID string, ids []string) (*audience.ComposedList, error) {
	name := audience.StandardSuppressionName(projectID)
	filter, err := audience.MasterListFilter(ids)
	if err != nil {
		return nil, err
	}
	existing, err := exactList(ctx, client, name)
	if err != nil {
		return nil, fmt.Errorf("check standard suppression list name: %w", err)
	}
	if existing == nil {
		created, cerr := x.createList(ctx, projectID, name, filter)
		if cerr != nil {
			return nil, fmt.Errorf("create standard suppression list %q: %w", name, cerr)
		}
		return created, nil
	}
	current, err := client.GetList(ctx, existing.ListID)
	if err != nil {
		return nil, fmt.Errorf("read standard suppression list %s: %w", existing.ListID, err)
	}
	if !audience.SameIDSet(audience.CoveredListIDs(current.FilterBranch), ids) {
		if uerr := client.UpdateListFilters(ctx, current.ListID, filter); uerr != nil {
			return nil, fmt.Errorf("refresh standard suppression list %s: %w", current.ListID, uerr)
		}
	}
	return &audience.ComposedList{ListRow: listRow(current)}, nil
}

// ensureRegistrantsList finds or creates the list of the edition's current registrants.
func (x *AudienceExplorer) ensureRegistrantsList(ctx context.Context, client *hubspot.Client, projectID, edition string) (*audience.ComposedList, error) {
	filter, err := audience.CurrentRegistrantsFilter(edition)
	if err != nil {
		return nil, err
	}
	name := audience.CurrentRegistrantsName(edition)
	existing, err := exactList(ctx, client, name)
	if err != nil {
		return nil, fmt.Errorf("check current registrants list name: %w", err)
	}
	if existing != nil {
		return &audience.ComposedList{ListRow: listRow(existing)}, nil
	}
	created, err := x.createList(ctx, projectID, name, filter)
	if err != nil {
		return nil, fmt.Errorf("create current registrants list %q: %w", name, err)
	}
	return created, nil
}
