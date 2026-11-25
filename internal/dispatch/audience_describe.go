// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// Lookup budgets for one describe request. Each referenced list and each contact property is one
// HubSpot round trip, and a master list can reference dozens; past the budget a rule still
// renders, naming the list by id or the property by its humanised internal name. The budgets
// keep a describe of 25 lists from turning into a burst that throttles the operator's other work.
const (
	describeListNameBudget = 20
	describePropertyBudget = 20
)

// DescribeLists reads each list's filter rules and renders them in plain English (see
// audience.DescribeRules). Read-only.
//
// A list that cannot be read is DESCRIBED as such -- not_found for HubSpot's 404, unreadable for
// anything else -- rather than failing the batch: an operator looking at five selected lists
// still learns what four of them mean. The request fails only when NO list could be read and at
// least one read failed with something other than a 404, because then the answer is about the
// connection, not the lists, and the caller's status mapping says so.
func (x *AudienceExplorer) DescribeLists(ctx context.Context, projectID string, listIDs []string) (out []audience.ListDescription, err error) {
	ids := audience.UniqueIDs(listIDs)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: at least one list id is required", audience.ErrInvalidRequest)
	}
	if len(ids) > audience.DescribeMaxLists {
		return nil, &audience.FieldError{Field: "list_ids", Reason: fmt.Sprintf("at most %d lists can be described at once", audience.DescribeMaxLists)}
	}

	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	type read struct {
		list     *hubspot.List
		notFound bool
	}
	reads := make([]read, len(ids))
	cache := make(map[string]*hubspot.List, len(ids))
	// Lists actually READ. A 404 is an answer about one list, not proof the connection works:
	// counted as one, a batch of a missing list plus a permission failure came back 200 with
	// nothing readable in it, hiding an unusable credential.
	readCount := 0
	var firstErr error
	for i, id := range ids {
		list, gerr := client.GetList(ctx, id)
		switch {
		case gerr == nil && list != nil:
			reads[i].list = list
			cache[id] = list
			readCount++
		case hubspot.IsNotFound(gerr):
			reads[i].notFound = true
		default:
			if gerr == nil {
				gerr = fmt.Errorf("hubspot: list %s returned no list", id)
			}
			if firstErr == nil {
				firstErr = gerr
			}
			slog.WarnContext(ctx, "could not read a list to describe it", "project_id", projectID, "list_id", id, "error", gerr)
		}
	}
	if readCount == 0 && firstErr != nil {
		return nil, firstErr
	}

	readable := make([]*hubspot.List, 0, len(reads))
	for _, r := range reads {
		if r.list != nil {
			readable = append(readable, r.list)
		}
	}
	labels := x.describeLabels(ctx, client, cache, readable)

	out = make([]audience.ListDescription, 0, len(ids))
	for i, id := range ids {
		r := reads[i]
		if r.list == nil {
			out = append(out, audience.UnreadableListDescription(id, r.notFound))
			continue
		}
		size := -1
		if s := sizeOf(r.list); s != nil {
			size = int(*s)
		}
		out = append(out, audience.DescribeList(r.list.ListID, r.list.Name, size, r.list.ProcessingType, r.list.AppURL, r.list.FilterBranch, labels))
	}
	return out, nil
}

// describeLabels resolves what the lists' rules reference -- other lists' names, contact
// properties' labels and option labels, event types' labels -- within the lookup budgets. Every
// failure here is swallowed into a missing label: the rule still renders with an id, which is
// worse prose but still true.
func (x *AudienceExplorer) describeLabels(ctx context.Context, client *hubspot.Client, cache map[string]*hubspot.List, lists []*hubspot.List) audience.DescribeLabels {
	labels := audience.DescribeLabels{
		Lists:         map[string]string{},
		Properties:    map[string]audience.PropertyLabel{},
		Events:        audience.KnownEventLabels(),
		Subscriptions: map[string]string{},
	}
	var refs audience.DescribeRefs
	seenList, seenProp, seenEvent := map[string]bool{}, map[string]bool{}, map[string]bool{}
	referencesSubscriptions := false
	for _, l := range lists {
		r := audience.CollectDescribeRefs(l.FilterBranch)
		if len(r.SubscriptionIDs) > 0 {
			referencesSubscriptions = true
		}
		for _, id := range r.ListIDs {
			if !seenList[id] {
				seenList[id] = true
				refs.ListIDs = append(refs.ListIDs, id)
			}
		}
		for _, p := range r.Properties {
			if !seenProp[p] {
				seenProp[p] = true
				refs.Properties = append(refs.Properties, p)
			}
		}
		for _, e := range r.EventTypeIDs {
			if !seenEvent[e] {
				seenEvent[e] = true
				refs.EventTypeIDs = append(refs.EventTypeIDs, e)
			}
		}
	}

	budget := describeListNameBudget
	for _, id := range refs.ListIDs {
		if l, ok := cache[id]; ok && l != nil {
			labels.Lists[id] = l.Name
			continue
		}
		if budget == 0 || ctx.Err() != nil {
			continue
		}
		budget--
		if name, _ := x.listName(ctx, client, cache, id); strings.TrimSpace(name) != "" {
			labels.Lists[id] = name
		}
	}

	budget = describePropertyBudget
	for _, name := range refs.Properties {
		if budget == 0 || ctx.Err() != nil {
			break
		}
		budget--
		prop, perr := client.GetContactProperty(ctx, name)
		if perr != nil || prop == nil {
			continue
		}
		pl := audience.PropertyLabel{Label: prop.Label}
		if len(prop.Options) > 0 {
			pl.Options = make(map[string]string, len(prop.Options))
			for _, o := range prop.Options {
				pl.Options[o.Value] = o.Label
			}
		}
		labels.Properties[strings.ToLower(name)] = pl
	}

	// Subscription names: ONE read, and only when a rule filters on subscription status. GDPR and
	// opt-out suppressions are built almost entirely from those, and "subscription 11147681" says
	// nothing to an operator. Best-effort like every lookup here: the endpoint needs its own scope,
	// and without it the rules keep their ids.
	if referencesSubscriptions && ctx.Err() == nil {
		if names, serr := client.SubscriptionDefinitionNames(ctx); serr == nil {
			labels.Subscriptions = names
		}
	}

	// One definitions read covers every event type the two known ids do not, and only when one
	// is actually referenced.
	unknownEvent := false
	for _, id := range refs.EventTypeIDs {
		if _, ok := labels.Events[id]; !ok {
			unknownEvent = true
			break
		}
	}
	if unknownEvent && ctx.Err() == nil {
		if defs, derr := client.ListEventDefinitions(ctx); derr == nil {
			for _, d := range defs {
				if id := strings.TrimSpace(d.ObjectTypeID); id != "" && strings.TrimSpace(d.Label) != "" {
					if _, known := labels.Events[id]; !known {
						labels.Events[id] = d.Label
					}
				}
			}
		}
	}
	return labels
}
