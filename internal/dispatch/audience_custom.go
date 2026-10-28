// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
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
// SSRF-guarded reader and its metadata quoted to the model, so the event's audience
// can be drafted without running discovery first. It is not location-scoped unless
// the request asks for one.
func (x *AudienceExplorer) InterpretCustom(ctx context.Context, projectID, request, eventURL string, answers []audience.CustomAnswer) (outcome *audience.CustomInterpretOutcome, err error) {
	if strings.TrimSpace(request) == "" && strings.TrimSpace(eventURL) != "" {
		// Neutral on purpose: "regional" read to the model as a request for the host
		// city and country, which an event audience no longer defaults to.
		request = "Plan the default audience for this event."
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
			// Filled in after the parser's length checks, so it is cut here: a long page
			// title (or one the year pushed over) would fail preview unchanged.
			draft.Criteria.Label = audience.FitLabel(event.Name)
		}
		// Past editions come from the warehouse, verbatim: they are exact filter values,
		// and registered_for's default. Editions the request or the answers named are
		// used instead, but only as exact names of this event or one of its editions,
		// and an audience that excludes past registrants gets none. The guard runs even
		// when the warehouse returned nothing: a guessed name matches nobody either way.
		{
			var ambiguities []string
			named, invented := knownEditionsWithAmbiguity(draft.Criteria.RegisteredFor, append([]string{event.Name}, event.PastEditions...),
				func(name string, candidates []string) {
					ambiguities = append(ambiguities, fmt.Sprintf("%q could be any of %s", name, quoteAll(candidates)))
				})
			if len(ambiguities) > 0 {
				draft.Notes = append(draft.Notes, fmt.Sprintf(
					"Left out %d registered_for names that match more than one past edition once spacing and case are ignored; name the exact spelling to include one: %s.",
					len(ambiguities), strings.Join(ambiguities, "; ")))
			}
			if len(invented) > 0 {
				reason := "are not this event's or one of its past editions' exact names"
				if len(event.PastEditions) == 0 {
					reason = "could not be verified: no past editions were found for this event"
				}
				draft.Notes = append(draft.Notes, fmt.Sprintf("Left out %d registered_for names that %s: %s.",
					len(invented), reason, strings.Join(invented, "; ")))
			}
			// The past editions are the default of two audiences: a regional one (with a
			// location), and the event's default audience, which selects the event
			// page's visitors and has no location. The service adds them rather than the
			// model, so they are capped newest first. Anything else gets none, because
			// the registration branch is OR'd beside the others: for speakers or
			// ambassadors alone it would AND every one with a past registration and drop
			// first-timers; for a topic-only education audience it would add past
			// registrants who never enrolled.
			defaults := event.PastEditions
			hasLocation := len(draft.Criteria.Cities) > 0 || len(draft.Criteria.Countries) > 0
			regional := hasLocation && (draft.Criteria.ContactLocation || draft.Criteria.EventRegistered || draft.Criteria.EducationEnrolled)
			rolesOnly := draft.Criteria.Speakers != nil || draft.Criteria.Ambassadors
			// The event's default audience pairs the visitors of THIS event's page with the
			// brand's education enrollees. A page-view URL of some other page is a different
			// segment, and a visitors-only draft (no education) asked for visitors alone:
			// ORing past registrants into either would reach people outside what was asked for.
			// The signals alone cannot tell the default from "this page's visitors plus some
			// OTHER course's enrollees", so the model's explicit include_past_editions: true is
			// required as well. The regional audience keeps its existing rule (unset applies).
			eventDefault := draft.IncludePastEditions && !hasLocation && !rolesOnly && draft.Criteria.EducationEnrolled &&
				selectsPage(draft.Criteria.PageViewURLs, event.URL)
			// An event page identified by a non-tracking query ("/event?id=123") cannot be
			// isolated: HubSpot's page-view filter is a CONTAINS match, so "?id=123" also matches
			// "?id=1234" and misses "?utm_source=x&id=123". Its visitors are not a reliable
			// default audience, so none is built; the URL stays in page_view_urls as the most
			// precise visitor filter there is, and the operator picks editions and pages.
			if eventDefault && strings.ContainsRune(withoutTracking(event.URL), '?') {
				eventDefault = false
				// Only when that withholds the defaults: editions the operator named are kept below.
				if len(named) == 0 {
					draft.Notes = append(draft.Notes, "No past editions were added automatically: this event's page is identified by a query (\""+
						withoutTracking(event.URL)+"\") that page-view filters cannot isolate, so it is not treated as the event's default audience. Pick the editions and pages explicitly.")
				}
			}
			switch {
			case len(named) > 0:
				defaults = named
			case len(draft.Criteria.RegisteredFor) > 0, draft.ExcludePastEditions, !regional && !eventDefault:
				// Every named edition was unknown, or past registrants were ruled out:
				// adding the warehouse's editions would reach people the operator excluded.
				defaults = nil
			default:
				// The warehouse matches the event's name as a SUBSTRING, so "Synthetic Summit"
				// also returns "Synthetic Summit Workshop 2025" -- another event. Only editions
				// that are this event's name with another year are added unasked; the rest are
				// named in a note, for the operator to add by name if they belong.
				var others []string
				defaults, others = sameEventEditions(defaults, event.Name)
				if len(others) > 0 {
					draft.Notes = append(draft.Notes, fmt.Sprintf(
						"Left out %d past events the warehouse matched whose names are not this event's name with another year; name any that are this event's editions to include them: %s.",
						len(others), strings.Join(others, "; ")))
				}
			}
			kept, omitted := capPastEditions(defaults)
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
		out.Event = &audience.EventIdentity{Name: event.PageName, BrandShort: event.BrandShort, Dates: event.Dates}
	}

	ctx = x.builder.BeginBuild(ctx)
	client, fromSystem, cerr := x.builder.cachedClient(ctx, projectID)
	if cerr != nil {
		return nil, cerr
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	if brand := strings.TrimSpace(draft.BrandShort); brand != "" {
		gate, gerr := x.brandMaster(ctx, client, brand)
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
	out.Similar = audience.GuardReuse(x.similarLists(ctx, client, request, answers, out.Draft.Criteria), out.Draft.Criteria)
	if event != nil && event.BrandShort == "" && strings.TrimSpace(out.Draft.BrandShort) == "" {
		// No brand from the page or the model: take the one this event's lists use.
		// A brand the model named already resolved its own gate above, and is never
		// overridden by whichever segment the event's lists happen to be filed under.
		if brand := x.listBrand(ctx, client, event.Name, event.Name); brand != "" {
			out.Draft.BrandShort = brand
			out.Event.BrandShort = brand
			if gate, gerr := x.brandMaster(ctx, client, brand); gerr == nil && gate != nil {
				row := listRow(gate)
				out.Gate = &row
				out.Draft.Criteria.GateListID = row.ListID
			}
		}
	}
	if out.Gate == nil {
		// A project or event series ("gRPC") has no master list of its own: its lists
		// are filed under the foundation that owns it, whose master is the gate.
		named := strings.TrimSpace(out.Draft.BrandShort)
		query := named
		if query == "" {
			query = strings.TrimSpace(out.Draft.Criteria.Label)
		}
		// Unscoped: the project's lists are named for its events ("gRPConf"), not for the
		// project ("gRPC"), so a scoped vote would discard every one of them.
		if brand := x.listBrand(ctx, client, query, ""); brand != "" && !strings.EqualFold(brand, named) {
			if gate, gerr := x.brandMaster(ctx, client, brand); gerr == nil && gate != nil {
				row := listRow(gate)
				out.Gate = &row
				out.Draft.Criteria.GateListID = row.ListID
				out.Draft.BrandShort = brand
				out.Draft.Notes = append(out.Draft.Notes, fmt.Sprintf(
					"Suggested %s's master list (%s): %q has none of its own, and its HubSpot lists are filed under %s. Change it if that is wrong.",
					brand, row.Name, query, brand))
			}
		}
	}
	if out.Gate == nil && (draft.Criteria.ContactLocation || draft.Criteria.GateEventHistory) {
		out.Draft.Notes = append(out.Draft.Notes, "No brand master list was found to gate the location branches. Pick one before previewing.")
	}
	return out, nil
}

// brandMaster is the brand's standing master list, or nil.
func (x *AudienceExplorer) brandMaster(ctx context.Context, client *hubspot.Client, brand string) (*hubspot.List, error) {
	return x.bestMatch(ctx, client, audience.BrandMasterProbes(brand), func(name string) bool {
		return audience.MatchesBrandMaster(name, brand)
	})
}

// listBrand is the brand the portal's lists matching query are filed under, or "".
// eventName, when set, keeps only that event's own lists in the vote (see
// audience.DominantBrand). A failed search is no brand: the caller then reports the
// gate as missing.
func (x *AudienceExplorer) listBrand(ctx context.Context, client *hubspot.Client, query, eventName string) string {
	if query == "" {
		return ""
	}
	hits, err := client.SearchLists(ctx, query)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Name)
	}
	return audience.DominantBrand(names, eventName)
}

// PreviewCustom validates criteria against the portal and returns what Create
// would build. Writes nothing.
func (x *AudienceExplorer) PreviewCustom(ctx context.Context, projectID string, criteria audience.CustomCriteria, name string) (preview *audience.CustomPreview, err error) {
	if nerr := audience.CheckCustomListName(name); nerr != nil {
		return nil, nerr
	}
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
		// The topic filter is always a string CONTAINS, so only a string property
		// can carry it: a number or enumeration one would preview and then match
		// nobody, for the automatic pick and the caller's alike.
		names := make(map[string]string, len(props))
		for _, p := range props {
			if strings.EqualFold(strings.TrimSpace(p.Type), "string") {
				names[p.Name] = p.Label
			}
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

	// An EXPLICIT name is the operator's: one too long was refused up front (CheckCustomListName),
	// never cut, so two typed names that differ past the limit cannot collide. Only the DERIVED
	// name is cut to fit, so the name preview returns is one create accepts back unchanged.
	listName := strings.Join(strings.Fields(name), " ")
	if listName == "" {
		listName = audience.FitListName(audience.CustomListName(resolved, x.now()))
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
	if nerr := audience.CheckCustomListName(name); nerr != nil {
		return nil, nerr
	}
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
func (x *AudienceExplorer) similarLists(ctx context.Context, client *hubspot.Client, request string, answers []audience.CustomAnswer, criteria audience.CustomCriteria) []audience.CustomSimilarList {
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
	raw, err := x.llm.Complete(ctx, audience.CustomReuseSystemPrompt, audience.CustomReusePrompt(request, answers, criteria, candidates))
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
		// The page without the TRACKING parameters and fragment the operator reached it by
		// (utm_*, click ids): the model copies this URL into page_view_urls, where HubSpot keeps
		// the whole string and a tracking query would narrow the visitors to that link. Any
		// other query stays: "/event?id=123" names the page, and "/event" would match every
		// event on that route.
		URL: withoutTracking(eventURL),
		// The edition's own name carries its year ("Synthetic Summit 2026"), as its
		// registrations are stored: a yearless page title would be copied into an
		// exact registered_for filter and match none of them.
		Name:       audience.WithEventYear(identity.Name, identity.Dates),
		PageName:   identity.Name,
		BrandShort: identity.BrandShort,
		Location:   sanitizeExtractedField(details.Location),
		Dates:      identity.Dates,
	}, nil
}

// knownEditions keeps the requested names that are, ignoring case and spacing,
// one of the known edition names, returned in the known spelling, and lists the
// rest. A guessed edition name would match nobody.
func knownEditions(requested, known []string) (kept, unknown []string) {
	return knownEditionsWithAmbiguity(requested, known, nil)
}

// knownEditionsWithAmbiguity is knownEditions that also reports, through ambiguous, each
// requested name that matched more than one warehouse spelling only after normalising (with
// the candidates), so the caller can say why it was left out.
//
// A requested name is kept as the warehouse spells it, verbatim: first an EXACT match (the
// warehouse can hold "Synthetic Summit 2025" and "Synthetic  Summit 2025" as two editions),
// then -- only when exactly one warehouse spelling normalises to it, ignoring case and spacing
// -- that spelling. Several candidates are ambiguous and the name is unknown: guessing one
// would send to the wrong edition's registrants.
func knownEditionsWithAmbiguity(requested, known []string, ambiguous func(name string, candidates []string)) (kept, unknown []string) {
	norm := func(v string) string { return strings.ToLower(strings.Join(strings.Fields(v), " ")) }
	exact := make(map[string]bool, len(known))
	byNorm := make(map[string][]string, len(known))
	for _, k := range known {
		n := norm(k)
		if n == "" || exact[k] {
			continue
		}
		exact[k] = true
		byNorm[n] = append(byNorm[n], k)
	}
	seen := map[string]bool{}
	for _, r := range requested {
		if strings.TrimSpace(r) == "" || seen[r] {
			continue
		}
		seen[r] = true
		switch candidates := byNorm[norm(r)]; {
		case exact[r]:
			kept = append(kept, r)
		case len(candidates) == 1:
			kept = append(kept, candidates[0])
		case len(candidates) > 1:
			if ambiguous != nil {
				ambiguous(r, candidates)
			}
			unknown = append(unknown, r)
		default:
			unknown = append(unknown, r)
		}
	}
	return uniqueExact(kept), unknown
}

// uniqueExact drops exact repeats, first kept.
func uniqueExact(values []string) []string {
	if values == nil {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// capPastEditions bounds the warehouse's past editions to registered_for's
// contract cap, so an unedited event-audience draft still passes preview. The
// warehouse returns them alphabetically and may return up to its own row cap,
// far above the contract's, so the most recent are kept: ordered newest year
// first (an edition with no year in its name sorts last), the input order
// breaking ties. It returns the kept names, verbatim, and how many were left out.
//
// The warehouse returns DISTINCT (EVENT_NAME, EVENT_ID) pairs, so one name can arrive several
// times; repeats are dropped first (exact match, first spelling kept), or they would use up cap
// slots and push real editions out.
func capPastEditions(past []string) ([]string, int) {
	seen := make(map[string]bool, len(past))
	var kept []string // nil for no editions, as before: an empty registered_for stays absent
	for _, p := range past {
		if !seen[p] {
			seen[p] = true
			kept = append(kept, p)
		}
	}
	unique := len(kept)
	if unique <= audience.CustomMaxRegisteredFor {
		return kept, 0
	}
	slices.SortStableFunc(kept, func(a, b string) int { return strings.Compare(yearIn(b), yearIn(a)) })
	return kept[:audience.CustomMaxRegisteredFor], unique - audience.CustomMaxRegisteredFor
}

// selectsPage reports whether urls includes the event page pageURL itself. Scheme, a leading
// "www.", a trailing slash and the path's case are ignored: page_view_urls is a CONTAINS match
// with the scheme optional, and the model may copy the event URL with or without them.
//
// The query is the page's identity when it is not tracking: pageURL keeps every non-tracking
// parameter (withoutTracking), and a page-view URL is the event page only when its query is
// exactly that remainder. So "/event?id=123" selects the page "/event?id=123"; "?id=456", a bare
// "/event" (every event on the route), or any added parameter such as "?ticket=vip" or
// "?utm_source=x" (only the visits made with it) does not. A page-view URL with a fragment is
// never the page. An empty pageURL matches nothing.
func selectsPage(urls []string, pageURL string) bool {
	norm := func(u string) string {
		u = strings.ToLower(strings.TrimSpace(u))
		u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
		u = strings.TrimPrefix(u, "www.")
		return strings.TrimRight(u, "/")
	}
	split := func(u string) (page, query string) {
		u = strings.TrimSpace(u)
		if i := strings.IndexByte(u, '?'); i >= 0 {
			return norm(u[:i]), u[i+1:]
		}
		return norm(u), ""
	}
	wantPage, wantQuery := split(withoutTracking(pageURL))
	if wantPage == "" {
		return false
	}
	for _, u := range urls {
		if strings.ContainsRune(u, '#') {
			continue
		}
		if page, query := split(u); page == wantPage && query == wantQuery {
			return true
		}
	}
	return false
}

// editionFamily is an edition name without its year, lowercased and with whitespace collapsed:
// "Synthetic Summit 2026", "Synthetic  Summit 2025" and "Synthetic Summit2024" are one family.
//
// A year is found by the SAME rule the warehouse resolver applies to the names it returns
// (yearIn here, yearInName in internal/platform/snowflake): a 19xx/20xx run of exactly four
// digits, bounded by non-digits -- so a year written against letters ("Summit2026") counts and
// a longer number containing one does not. A \b rule would miss the first and keep its year in
// the family, so the resolver's own edition would be left out of the default.
func editionFamily(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); {
		if i+4 <= len(name) && isSupportedYear(name[i:i+4]) &&
			(i == 0 || name[i-1] < '0' || name[i-1] > '9') && (i+4 == len(name) || name[i+4] < '0' || name[i+4] > '9') {
			b.WriteByte(' ')
			i += 4
			continue
		}
		b.WriteByte(name[i])
		i++
	}
	return strings.ToLower(strings.Join(strings.Fields(b.String()), " "))
}

// sameEventEditions splits the warehouse's past editions into those that are eventName's own
// family (the same name with another year) and the rest, both verbatim and in order. An event
// name with no family left (empty) keeps nothing.
func sameEventEditions(past []string, eventName string) (same, others []string) {
	family := editionFamily(eventName)
	for _, p := range past {
		if family != "" && editionFamily(p) == family {
			same = append(same, p)
		} else {
			others = append(others, p)
		}
	}
	return same, others
}

// trackingParams are query parameters that say how a visitor ARRIVED, never which page it is:
// campaign tags and ad or email click ids. utm_* and hsa_* are matched as prefixes.
var trackingParams = map[string]bool{
	"gclid": true, "fbclid": true, "msclkid": true, "mc_cid": true, "mc_eid": true, "_hsenc": true, "_hsmi": true,
}

// isTrackingParam reports whether a query parameter name is a tracking parameter.
func isTrackingParam(name string) bool {
	name = strings.ToLower(name)
	return trackingParams[name] || strings.HasPrefix(name, "utm_") || strings.HasPrefix(name, "hsa_")
}

// withoutTracking is u without its fragment and without its tracking query parameters, every
// other parameter kept in order and byte for byte (no re-encoding). "?utm_source=x&id=123"
// becomes "?id=123"; a query left empty is dropped with its "?".
func withoutTracking(u string) string {
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	i := strings.IndexByte(u, '?')
	if i < 0 {
		return u
	}
	kept := make([]string, 0, 4)
	for _, param := range strings.Split(u[i+1:], "&") {
		if param == "" {
			continue
		}
		name, _, _ := strings.Cut(param, "=")
		if !isTrackingParam(name) {
			kept = append(kept, param)
		}
	}
	if len(kept) == 0 {
		return u[:i]
	}
	return u[:i] + "?" + strings.Join(kept, "&")
}

// quoteAll quotes each value, joined with " or ", so spellings that differ only in spacing stay
// visibly different in a note.
func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, " or ")
}
