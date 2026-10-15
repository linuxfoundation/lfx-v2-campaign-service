// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// ---------------------------------------------------------------------------
// Custom audiences
//
// Interpret asks the model for a DRAFT and resolves the lists it cannot know.
// Preview and Create share one validation path and differ only in the write:
// Create never trusts a previously previewed filter, it rebuilds from the
// criteria, because a preview the client edited afterwards is not a preview.
// ---------------------------------------------------------------------------

// customReuseCandidates bounds the reuse step: enough to show the plausible
// matches, few enough that ranking them is one short model call.
const customReuseCandidates = 8

// customVersionLimit bounds the "(vN)" probe. Past it the operator is asked to
// name the list, rather than this walking the portal's list index indefinitely.
const customVersionLimit = 10

// InterpretCustom turns a free-text request into draft criteria.
//
// eventURL, when set, makes this an EVENT audience: the page is fetched through the
// SSRF-guarded reader and its metadata quoted to the model, so a regional plan can
// be drafted without running discovery first.
func (x *AudienceExplorer) InterpretCustom(ctx context.Context, projectID, request, eventURL string, answers []audience.CustomAnswer) (outcome *audience.CustomInterpretOutcome, err error) {
	if strings.TrimSpace(request) == "" && strings.TrimSpace(eventURL) != "" {
		request = "Plan the regional event audience for this event."
	}
	if _, perr := audience.CustomInterpretPrompt(request, answers, nil); perr != nil {
		return nil, perr
	}
	if x.llm == nil {
		return nil, audience.ErrCustomInterpretUnavailable
	}
	var event *audience.CustomEventContext
	if u := strings.TrimSpace(eventURL); u != "" {
		ev, everr := x.customEventContext(ctx, u)
		if everr != nil {
			return nil, everr
		}
		event = ev
	}
	prompt, perr := audience.CustomInterpretPrompt(request, answers, event)
	if perr != nil {
		return nil, perr
	}
	raw, lerr := x.llm.Complete(ctx, audience.CustomInterpretSystemPrompt, prompt)
	if lerr != nil {
		return nil, fmt.Errorf("audience custom: interpret request: %w", lerr)
	}
	draft, derr := audience.ParseCustomDraft(raw)
	if derr != nil {
		return nil, derr
	}
	if event != nil {
		// The page is authoritative for who the event is; the model only fills gaps.
		if event.BrandShort != "" {
			draft.BrandShort = event.BrandShort
		}
		if draft.Criteria.Label == "" {
			draft.Criteria.Label = event.Name
		}
		// Past editions come from the warehouse, verbatim: they are exact filter values.
		if len(event.PastEditions) > 0 {
			kept, omitted := capPastEditions(event.PastEditions)
			draft.Criteria.RegisteredFor = kept
			if omitted > 0 {
				draft.Notes = append(draft.Notes, fmt.Sprintf(
					"Kept the %d most recent past editions in registered_for; %d older ones were left out (the field takes at most %d).",
					len(kept), omitted, audience.CustomMaxRegisteredFor))
			}
		}
	}
	draft.Questions = audience.SettleQuestions(draft.Questions, answers)
	out := &audience.CustomInterpretOutcome{Draft: *draft}
	if event != nil {
		out.Event = &audience.EventIdentity{Name: event.Name, BrandShort: event.BrandShort, Dates: event.Dates}
	}

	ctx = x.builder.BeginBuild(ctx)
	client, fromSystem, cerr := x.builder.cachedClient(ctx, projectID)
	if cerr != nil {
		return nil, cerr
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	if brand := strings.TrimSpace(draft.BrandShort); brand != "" {
		gate, gerr := x.bestMatch(ctx, client, audience.BrandMasterProbes(brand), func(name string) bool {
			return audience.MatchesBrandMaster(name, brand)
		})
		if gerr != nil {
			return nil, gerr
		}
		if gate != nil {
			row := listRow(gate)
			out.Gate = &row
			out.Draft.Criteria.GateListID = row.ListID
		}
	}
	if s := out.Draft.Criteria.Speakers; s != nil && strings.TrimSpace(draft.Criteria.Label) != "" {
		keywords := audience.EventKeywords(draft.Criteria.Label)
		found, serr := x.bestMatch(ctx, client, []string{draft.Criteria.Label + " Speakers"}, func(name string) bool {
			return audience.SpeakerListFits(name, s.Scope, s.EventNames) && audience.SharesKeyword(name, keywords)
		})
		if serr != nil {
			return nil, serr
		}
		if found != nil {
			row := listRow(found)
			out.Speakers = &row
			s.ListID = row.ListID
		}
	}
	out.Similar = audience.GuardReuse(x.similarLists(ctx, client, request, out.Draft.Criteria), out.Draft.Criteria)
	if event != nil && event.BrandShort == "" && strings.TrimSpace(out.Draft.BrandShort) == "" {
		// No brand from the page or the model: take the one this event's lists use.
		// A brand the model named already resolved its own gate above, and is never
		// overridden by whichever segment the event's lists happen to be filed under.
		if hits, serr := client.SearchLists(ctx, event.Name); serr == nil {
			names := make([]string, 0, len(hits))
			for _, h := range hits {
				names = append(names, h.Name)
			}
			if brand := audience.DominantBrand(names, event.Name); brand != "" {
				out.Draft.BrandShort = brand
				out.Event.BrandShort = brand
				if gate, gerr := x.bestMatch(ctx, client, audience.BrandMasterProbes(brand), func(name string) bool {
					return audience.MatchesBrandMaster(name, brand)
				}); gerr == nil && gate != nil {
					row := listRow(gate)
					out.Gate = &row
					out.Draft.Criteria.GateListID = row.ListID
				}
			}
		}
	}
	if out.Gate == nil && (draft.Criteria.ContactLocation || draft.Criteria.GateEventHistory) {
		out.Draft.Notes = append(out.Draft.Notes, "No brand master list was found to gate the location branches. Pick one before previewing.")
	}
	return out, nil
}

// PreviewCustom validates criteria against the portal and returns what Create
// would build. Writes nothing.
func (x *AudienceExplorer) PreviewCustom(ctx context.Context, projectID string, criteria audience.CustomCriteria, name string) (preview *audience.CustomPreview, err error) {
	ctx = x.builder.BeginBuild(ctx)
	client, fromSystem, cerr := x.builder.cachedClient(ctx, projectID)
	if cerr != nil {
		return nil, cerr
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()
	return x.previewCustom(ctx, client, criteria, name)
}

func (x *AudienceExplorer) previewCustom(ctx context.Context, client *hubspot.Client, criteria audience.CustomCriteria, name string) (*audience.CustomPreview, error) {
	// Idempotent; the service already did this, but every filter value built below
	// must be the form HubSpot stores whichever entry point the criteria came from.
	criteria.Countries = audience.CanonicalCountries(criteria.Countries)
	// The topic property is read from the portal BEFORE validation, which refuses
	// topics without one. A caller's choice is honoured only when it is one of the
	// portal's own candidates; anything else is replaced by the best candidate.
	requested := strings.TrimSpace(criteria.EducationTopicProperty)
	criteria.EducationTopicProperty = ""
	var topicCandidates []string
	if len(criteria.EducationTopics) > 0 {
		props, perr := client.EventTypeProperties(ctx, audience.EducationEventTypeID)
		if perr != nil {
			return nil, fmt.Errorf("audience custom: read education event properties: %w", perr)
		}
		names := make(map[string]string, len(props))
		for _, p := range props {
			names[p.Name] = p.Label
		}
		topicCandidates = audience.CourseTopicCandidates(names)
		if len(topicCandidates) > 0 {
			criteria.EducationTopicProperty = topicCandidates[0]
			if slices.Contains(topicCandidates, requested) {
				criteria.EducationTopicProperty = requested
			}
		}
	}
	// Speaker signals are typed by the portal's registration event definition, read
	// here for the same reason the topic property is: a guessed type is refused.
	if s := criteria.Speakers; s != nil && strings.TrimSpace(s.ListID) == "" {
		props, perr := client.EventTypeProperties(ctx, audience.RegistrationEventTypeID)
		if perr != nil {
			return nil, fmt.Errorf("audience custom: read registration event properties: %w", perr)
		}
		wanted := map[string]bool{}
		for _, name := range audience.SpeakerSignalProperties() {
			wanted[name] = true
		}
		types := map[string]string{}
		for _, p := range props {
			if wanted[p.Name] {
				types[p.Name] = p.Type
			}
		}
		speakers := *s
		speakers.PropertyTypes = types
		criteria.Speakers = &speakers
	}
	if err := audience.ValidateCustomCriteria(criteria); err != nil {
		return nil, err
	}
	var warnings []string

	// Every contact property is read from the portal: its existence, and its type,
	// which decides the operation type. Both are things a guess gets silently wrong.
	types := map[string]bool{}
	kinds := map[string]string{}
	var countryOptions map[string]bool
	var missing []string
	for _, prop := range audience.CustomContactProperties(criteria) {
		def, perr := client.GetContactProperty(ctx, prop)
		if perr != nil {
			return nil, fmt.Errorf("audience custom: read property %q: %w", prop, perr)
		}
		if def == nil {
			missing = append(missing, prop)
			continue
		}
		types[prop] = def.IsEnumeration()
		kinds[prop] = strings.ToLower(strings.TrimSpace(def.Type))
		if prop == "country_dropdown" && def.IsEnumeration() {
			countryOptions = make(map[string]bool, len(def.Options))
			for _, o := range def.Options {
				countryOptions[o.Value] = true
			}
		}
	}
	if len(missing) > 0 {
		return nil, &audience.UnknownPropertiesError{Names: missing}
	}
	if err := audience.CheckFilterKinds(criteria.PropertyFilters, kinds); err != nil {
		return nil, err
	}
	if len(criteria.Countries) > 0 && criteria.ContactLocation && !types["country_dropdown"] {
		warnings = append(warnings, "country_dropdown is not an enumeration in this portal; the country branch may match nobody.")
	}
	if criteria.ContactLocation && countryOptions != nil {
		// An enumeration only ever holds its own options: a country outside them
		// adds nothing to the country_dropdown branch, silently.
		for _, country := range criteria.Countries {
			if !countryOptions[country] {
				warnings = append(warnings, fmt.Sprintf("%q is not one of country_dropdown's options in this portal; the country_dropdown branch will not match it.", country))
			}
		}
	}
	resolved := criteria
	resolved.PropertyFilters = make([]audience.CustomPropertyFilter, len(criteria.PropertyFilters))
	for i, f := range criteria.PropertyFilters {
		f.Enumeration = types[strings.TrimSpace(f.Property)]
		resolved.PropertyFilters[i] = f
	}

	var gate *audience.ListRow
	if id := strings.TrimSpace(resolved.GateListID); id != "" {
		l, gerr := contactList(ctx, client, id)
		if gerr != nil {
			return nil, gerr
		}
		row := listRow(l)
		gate = &row
	}
	if s := resolved.Speakers; s != nil && strings.TrimSpace(s.ListID) != "" {
		if _, serr := contactList(ctx, client, s.ListID); serr != nil {
			return nil, serr
		}
	}

	filter, ferr := audience.CustomListFilter(resolved)
	if ferr != nil {
		return nil, ferr
	}
	branches, perr := audience.ParseFilterBranch(filter)
	if perr != nil {
		return nil, fmt.Errorf("audience custom: re-read built filter: %w", perr)
	}

	listName := strings.Join(strings.Fields(name), " ")
	if listName == "" {
		listName = audience.CustomListName(resolved, x.now())
	}
	listName, nerr := x.freeCustomName(ctx, client, listName)
	if nerr != nil {
		return nil, nerr
	}

	out := &audience.CustomPreview{
		Name:                     listName,
		Criteria:                 resolved,
		EducationTopicCandidates: topicCandidates,
		BranchCount:              len(branches.FilterBranches),
		Gate:                     gate,
		Warnings:                 warnings,
	}
	if gate != nil && gate.Size != nil && audience.AllBranchesGated(resolved) {
		bound := int(*gate.Size)
		out.UpperBound = &bound
	}
	return out, nil
}

// CreateCustom builds and creates the list. NOT idempotent.
func (x *AudienceExplorer) CreateCustom(ctx context.Context, projectID string, criteria audience.CustomCriteria, name string) (created *audience.ComposedList, err error) {
	ctx = x.builder.BeginBuild(ctx)
	client, fromSystem, cerr := x.builder.cachedClient(ctx, projectID)
	if cerr != nil {
		return nil, cerr
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	preview, perr := x.previewCustom(ctx, client, criteria, name)
	if perr != nil {
		return nil, perr
	}
	filter, ferr := audience.CustomListFilter(preview.Criteria)
	if ferr != nil {
		return nil, ferr
	}
	list, lerr := x.createList(ctx, projectID, preview.Name, filter)
	if lerr != nil {
		if hubspot.IsUnconfirmed(lerr) {
			return nil, &audience.ComposePartialError{
				MasterName: preview.Name,
				Err:        fmt.Errorf("audience custom: create list: %w", lerr),
			}
		}
		return nil, fmt.Errorf("audience custom: create list: %w", lerr)
	}
	slog.InfoContext(ctx, "custom audience list created", "list_id", list.ListID, "branches", preview.BranchCount)
	return list, nil
}

// freeCustomName returns name, or the first "(vN)" version of it no list holds.
// A taken name is never reused: a matching name does not prove a matching audience.
func (x *AudienceExplorer) freeCustomName(ctx context.Context, client *hubspot.Client, name string) (string, error) {
	candidate := name
	for n := 2; n <= customVersionLimit+1; n++ {
		taken, err := exactList(ctx, client, candidate)
		if err != nil {
			return "", fmt.Errorf("audience custom: check list name: %w", err)
		}
		if taken == nil {
			return candidate, nil
		}
		candidate = audience.VersionedName(name, n)
	}
	return "", fmt.Errorf("%w: %q and its first %d versions are all taken; choose a different name",
		audience.ErrInvalidRequest, name, customVersionLimit)
}

// contactList reads a list the caller referenced and refuses anything that is not
// a contact list.
func contactList(ctx context.Context, client *hubspot.Client, id string) (*hubspot.List, error) {
	l, err := client.GetList(ctx, strings.TrimSpace(id))
	if err != nil {
		if hubspot.IsNotFound(err) {
			return nil, fmt.Errorf("audience custom: list %s: %w", id, audience.ErrListNotFound)
		}
		return nil, fmt.Errorf("audience custom: read list %s: %w", id, err)
	}
	if l.ObjectTypeID != "" && l.ObjectTypeID != "0-1" {
		return nil, fmt.Errorf("audience custom: list %s is not a contact list: %w", id, audience.ErrListNotFound)
	}
	return l, nil
}

// similarLists finds lists that may already be this audience and asks the model
// to rank them. Best effort throughout: a failed search or ranking costs the
// operator a suggestion, never the draft.
func (x *AudienceExplorer) similarLists(ctx context.Context, client *hubspot.Client, request string, criteria audience.CustomCriteria) []audience.CustomSimilarList {
	seen := map[string]struct{}{}
	var candidates []audience.ListRow
	for _, probe := range audience.CustomReuseProbes(criteria) {
		hits, err := client.SearchLists(ctx, probe)
		if err != nil {
			slog.WarnContext(ctx, "custom audience reuse search failed", "error", err)
			continue
		}
		for i := range hits {
			hit := &hits[i]
			if _, dup := seen[hit.ListID]; dup || !audience.ReusableCandidate(hit.Name) {
				continue
			}
			seen[hit.ListID] = struct{}{}
			candidates = append(candidates, listRow(hit))
			if len(candidates) == customReuseCandidates {
				break
			}
		}
		if len(candidates) == customReuseCandidates {
			break
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	unranked := make([]audience.CustomSimilarList, 0, len(candidates))
	for _, c := range candidates {
		unranked = append(unranked, audience.CustomSimilarList{ListRow: c})
	}
	if x.llm == nil {
		return unranked
	}
	raw, err := x.llm.Complete(ctx, audience.CustomReuseSystemPrompt, audience.CustomReusePrompt(request, candidates))
	if err != nil {
		slog.WarnContext(ctx, "custom audience reuse ranking failed", "error", err)
		return unranked
	}
	ranked, err := audience.ParseCustomReuse(raw, candidates)
	if err != nil {
		slog.WarnContext(ctx, "custom audience reuse ranking unparseable", "error", err)
		return unranked
	}
	return ranked
}

// customEventContext reads an event page for an event audience: the parsed name,
// dates and location, plus the brand token the model extracts — the same reading
// discovery does, without the list search.
func (x *AudienceExplorer) customEventContext(ctx context.Context, eventURL string) (*audience.CustomEventContext, error) {
	if x.fetcher == nil || x.parser == nil {
		return nil, audience.ErrEventPageUnavailable
	}
	body, err := x.fetcher.Fetch(ctx, eventURL)
	if err != nil {
		return nil, err
	}
	details := x.parser.Parse(body)
	identity := audience.EventIdentity{
		Name:  strings.TrimSpace(details.Name),
		Dates: audience.SanitizeEventDates([]string{details.StartDate, details.EndDate}),
	}
	x.enrichIdentity(ctx, &identity, details)
	if identity.Name == "" {
		return nil, audience.ErrEventNameUnresolved
	}
	// Past editions, from the warehouse when it is configured. Best effort: without
	// them the plan still covers the region, only not this event's own history.
	year := ""
	if len(identity.Dates) > 0 && len(identity.Dates[0]) >= 4 {
		year = identity.Dates[0][:4]
	}
	past, perr := x.builder.ResolvePastEditions(ctx, identity.Name, "", year)
	if perr != nil {
		slog.WarnContext(ctx, "custom event audience could not resolve past editions", "error", perr)
	}
	return &audience.CustomEventContext{
		PastEditions: past,
		URL:          eventURL,
		Name:         identity.Name,
		BrandShort:   identity.BrandShort,
		Location:     sanitizeExtractedField(details.Location),
		Dates:        identity.Dates,
	}, nil
}

// capPastEditions bounds the warehouse's past editions to registered_for's
// contract cap, so an unedited event-audience draft still passes preview. The
// warehouse returns them alphabetically and may return up to its own row cap,
// far above the contract's, so the most recent are kept: ordered newest year
// first (an edition with no year in its name sorts last), the input order
// breaking ties. It returns the kept names, verbatim, and how many were left out.
func capPastEditions(past []string) ([]string, int) {
	kept := append([]string(nil), past...)
	if len(kept) <= audience.CustomMaxRegisteredFor {
		return kept, 0
	}
	slices.SortStableFunc(kept, func(a, b string) int { return strings.Compare(yearIn(b), yearIn(a)) })
	return kept[:audience.CustomMaxRegisteredFor], len(past) - audience.CustomMaxRegisteredFor
}
