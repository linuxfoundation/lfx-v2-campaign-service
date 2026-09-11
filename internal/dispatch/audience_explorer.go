// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// ---------------------------------------------------------------------------
// Audience exploration (LFXV2-2770)
//
// The orchestration half of the audience builder: it reads the project's HubSpot
// portal, and for one endpoint writes two lists into it. The DECISIONS all live in
// internal/audience, which is pure — this file only sequences calls, applies the
// budgets, and decides what a partial failure is allowed to look like.
//
// It is a separate type from AudienceBuilder even though it borrows that type's
// credential resolution, because the two have different contracts. AudienceBuilder
// materialises the audience a BRIEF commits to and its failures fail the brief.
// Exploration is what an operator does BEFORE committing, so almost everything here
// degrades to a partial answer with the gap named, rather than failing.
//
// The exception is ComposeAudienceMaster, which creates real contact lists in a
// production portal and is NOT idempotent.
// ---------------------------------------------------------------------------

// eventPageReader fetches an event page's bytes. Narrow, and satisfied in production
// only by eventurl.Fetcher, which carries the SSRF guard: the URL here is typed by an
// operator, so nothing may reach this seam with an unguarded HTTP client.
type eventPageReader interface {
	Fetch(ctx context.Context, eventURL string) ([]byte, error)
}

// eventPageParser extracts event metadata deterministically from a fetched page.
type eventPageParser interface {
	Parse(body []byte) eventurl.EventDetails
}

// completer is the LLM seam, used for ONE thing: recovering the short brand token
// ("CNCF") that no amount of markup parsing yields reliably.
//
// It is optional, and nothing an operator acts on depends on it. Discovery's
// classification is entirely deterministic — the model never decides which list gets
// mailed, because a model that guesses that is a model that can silently mail the
// wrong ten thousand people.
type completer interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// AudienceExplorer implements the audience-builder exploration methods against a
// project's real HubSpot portal.
type AudienceExplorer struct {
	// builder supplies per-project credential resolution and its per-build client
	// cache. Embedded by pointer rather than re-implemented so there is exactly one
	// place in this service that turns a project id into a HubSpot client.
	builder *AudienceBuilder
	fetcher eventPageReader
	parser  eventPageParser
	llm     completer
	// now is injected so the quarter segment of a derived list name is testable. It
	// is never nil after construction.
	now func() time.Time
}

// NewAudienceExplorer constructs the explorer. fetcher, parser and llm may each be
// nil: a deployment without them still serves every endpoint that does not read an
// event page, and Discover reports what it could not do rather than failing.
func NewAudienceExplorer(builder *AudienceBuilder, fetcher eventPageReader, parser eventPageParser, llm completer) *AudienceExplorer {
	// A nil *llm.Client (what the container returns when AI_PROXY_URL/AI_API_KEY are
	// unset) arrives here as a non-nil interface holding a nil pointer, so the
	// `x.llm == nil` guards downstream read false and call through to a nil receiver.
	// Normalising at the single construction site is what makes "llm may be nil" in
	// the doc comment above actually true for every guard, rather than each guard
	// having to re-derive it. Same for the other two optional dependencies, which
	// reach their guards by the identical path.
	if isNilIface(llm) {
		llm = nil
	}
	if isNilIface(fetcher) {
		fetcher = nil
	}
	if isNilIface(parser) {
		parser = nil
	}
	return &AudienceExplorer{builder: builder, fetcher: fetcher, parser: parser, llm: llm, now: time.Now}
}

// isNilIface reports whether v is either an untyped nil interface or an interface
// holding a nil pointer. reflect is the only way to see the second case.
//
// Only llm reaches this with a typed nil today (the container's eventFetcher and
// NewParser never return nil), but all three arrive by the same route and are
// guarded by the same `== nil` test, so normalising one and not the others would
// leave the next nil-returning constructor to rediscover this panic.
func isNilIface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

// systemScopedHubSpot tags a PERMISSION rejection that surfaced on a request made with an
// already-resolved client, mirroring CreateList and BuiltInPortalID in audience_builder.go.
//
// Resolution cannot see this class of defect: the client was built from a credential that
// decrypted cleanly, and the token was only refused when it was first used. Every explore
// endpoint issues its HubSpot requests after that point, so a 401/403 can only appear here.
//
// The origin half is what makes it worth tagging at all. A foundation with no HubSpot
// connection of its own now resolves the LF SYSTEM row — the ordinary case, not the exception —
// so one revoked shared token rejects every such foundation at once. Untagged, each of them is
// told to audit a HubSpot configuration that is correct, while nothing names the single row an
// operator could rotate.
//
// Non-permission errors pass through UNCHANGED. They carry classifications this must not
// flatten — hubspot.IsUnconfirmed's "a list may exist, go look" above all, which is how a
// duplicate contact list gets made — and a read timeout is not a credential to rotate.
func systemScopedHubSpot(err error, fromSystem bool) error {
	if err == nil || !hubspot.IsPermissionRejection(err) {
		return err
	}
	tagged := fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, err)
	if fromSystem {
		return fmt.Errorf("%w: %w", domain.ErrSystemConnectionNotUsable, tagged)
	}
	return tagged
}

// Capabilities reports whether this project has a usable HubSpot connection.
//
// It resolves a client and throws it away. That is the only honest test available:
// "a connection row exists" is not the same as "a client can be built from it", and
// the difference (a connection deactivated, or an undecryptable credential) is
// exactly what leaves an operator staring at a tab whose every button fails.
//
// Never returns an error. An unresolvable connection is the ANSWER here, not a
// failure — this endpoint exists to drive a degraded UI, so failing it would leave
// the caller unable to render even the explanation.
func (x *AudienceExplorer) Capabilities(ctx context.Context, projectID string) audience.ExploreCapabilities {
	if _, _, err := x.builder.client(ctx, projectID); err != nil {
		// The underlying error is logged, not returned: it can name the connection
		// store and its failure modes, and this string is rendered to an operator.
		slog.WarnContext(ctx, "audience builder unavailable for project",
			"project_id", projectID, "error", err)
		return audience.ExploreCapabilities{
			HubSpotConfigured: false,
			Detail:            "No usable HubSpot connection resolves for this project. Connect HubSpot, or check that the existing connection is still active.",
		}
	}
	return audience.ExploreCapabilities{HubSpotConfigured: true}
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// Discover reads the event page, then finds and classifies the portal's existing
// lists that qualify for the event's audience. It creates nothing.
func (x *AudienceExplorer) Discover(ctx context.Context, projectID, eventURL string) (outcome *audience.DiscoveryOutcome, err error) {
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	identity, err := x.eventIdentity(ctx, eventURL)
	if err != nil {
		return nil, err
	}

	keywords := audience.EventKeywords(identity.Name)
	candidates, serr := x.searchCandidates(ctx, client, identity, keywords)
	if serr != nil {
		return nil, serr
	}

	// Newest quarter first, so the inspection budget is spent on the lists most
	// likely to be the current edition's rather than on whatever the search returned
	// first (HubSpot's list search has no recency ordering at all).
	sort.SliceStable(candidates, func(i, j int) bool { return audience.NewerFirst(candidates[i], candidates[j]) })

	out := &audience.DiscoveryOutcome{Event: identity, Lists: []audience.DiscoveredList{}}
	found := map[audience.Signal]struct{}{}
	seenClassified := map[string]struct{}{}

	for _, candidate := range candidates {
		if out.Inspected >= audience.DiscoveryMaxInspections {
			break
		}
		out.Inspected++
		list, filters, gerr := x.listWithFilters(ctx, client, candidate.ListID)
		if gerr != nil {
			// One unreadable list must not lose the other thirty-nine. The list simply
			// does not appear, which is the same outcome as it not matching.
			slog.WarnContext(ctx, "audience discovery could not read a candidate list",
				"project_id", projectID, "list_id", candidate.ListID, "error", gerr)
			continue
		}

		// A rollup list selects purely by membership of OTHER lists, so classifying it
		// would yield `uncertain` while discarding the children that carry the real
		// evidence. Its children are inspected instead — and they count against the
		// budget, because each one is a HubSpot round-trip.
		if audience.IsRollup(filters) {
			for _, childID := range audience.RollupChildIDs(filters) {
				if out.Inspected >= audience.DiscoveryMaxInspections {
					break
				}
				out.Inspected++
				child, childFilters, cerr := x.listWithFilters(ctx, client, childID)
				if cerr != nil {
					slog.WarnContext(ctx, "audience discovery could not read a rollup child list",
						"project_id", projectID, "list_id", childID, "error", cerr)
					continue
				}
				x.classifyInto(out, found, seenClassified, child, childFilters, identity)
			}
			continue
		}

		x.classifyInto(out, found, seenClassified, list, filters, identity)
	}

	out.MissingSignals = audience.MissingSignals(found)
	return out, nil
}

// classifyInto classifies one list and appends it to the outcome, de-duplicating by
// list id (a list can be reached both directly and as a rollup child).
func (x *AudienceExplorer) classifyInto(
	out *audience.DiscoveryOutcome,
	found map[audience.Signal]struct{},
	seen map[string]struct{},
	list *hubspot.List,
	filters []audience.ListFilter,
	identity audience.EventIdentity,
) {
	if list == nil {
		return
	}
	if _, dup := seen[list.ListID]; dup {
		return
	}
	seen[list.ListID] = struct{}{}

	classification := audience.ClassifyList(list.Name, filters, identity)
	// `uncertain` is recorded as a visible row but NOT as a signal found. Counting it
	// would suppress the missing-signal section for a portal where nothing was
	// actually identified — the one case where that section matters most.
	if classification.Signal != audience.SignalUncertain {
		found[classification.Signal] = struct{}{}
	}
	out.Lists = append(out.Lists, audience.DiscoveredList{
		ListRow:  listRow(list),
		Signal:   classification.Signal,
		Reason:   classification.Reason,
		ListType: list.ProcessingType,
		Scope:    classification.Scope,
	})
}

// searchCandidates runs discovery's searches and returns the plausible hits,
// de-duplicated by list id.
//
// Returns an error ONLY for a credential rejection. Every other per-query failure is
// absorbed, because one bad query must not lose the hits the others returned — but a
// revoked token fails every query alike, and absorbing those would turn a broken
// connection into a confident "this portal has no lists".
func (x *AudienceExplorer) searchCandidates(ctx context.Context, client *hubspot.Client, identity audience.EventIdentity, keywords map[string]struct{}) ([]audience.ListCandidate, error) {
	seen := map[string]struct{}{}
	out := make([]audience.ListCandidate, 0, 32)
	for _, query := range audience.DiscoveryQueries(identity) {
		hits, err := client.SearchLists(ctx, query)
		if err != nil {
			// A revoked token fails EVERY query, so continuing past them all reports an
			// empty portal for what is really a credential problem — and bypasses the
			// deferred systemScopedHubSpot classifier that would make it a typed 503.
			if hubspot.IsPermissionRejection(err) {
				return nil, err
			}
			// A failed search narrows the candidate set; it does not invalidate the
			// hits the other queries returned. Discovery reports what it found.
			slog.WarnContext(ctx, "audience discovery search failed", "query_len", len(query), "error", err)
			continue
		}
		for i := range hits {
			hit := &hits[i]
			if _, dup := seen[hit.ListID]; dup {
				continue
			}
			if !audience.IsPlausibleCandidate(hit.Name, keywords) {
				continue
			}
			seen[hit.ListID] = struct{}{}
			out = append(out, listCandidate(hit))
		}
	}
	return out, nil
}

// eventIdentity resolves the event's name, brand token and dates from its page.
//
// The parse is deterministic and authoritative for the name and dates. The LLM is
// asked only to improve on it — for the brand token, which markup does not carry —
// and its answer is accepted only where it does not contradict the parse.
func (x *AudienceExplorer) eventIdentity(ctx context.Context, eventURL string) (audience.EventIdentity, error) {
	if x.fetcher == nil || x.parser == nil {
		return audience.EventIdentity{}, audience.ErrEventPageUnavailable
	}
	body, err := x.fetcher.Fetch(ctx, eventURL)
	if err != nil {
		return audience.EventIdentity{}, err
	}
	details := x.parser.Parse(body)

	identity := audience.EventIdentity{
		Name:  strings.TrimSpace(details.Name),
		Dates: audience.SanitizeEventDates([]string{details.StartDate, details.EndDate}),
	}
	x.enrichIdentity(ctx, &identity, details)

	if identity.Name == "" {
		// Without a name there are no search queries and no keywords, so every
		// suppression predicate would reject everything and discovery would report an
		// empty portal. Saying so is the only useful answer.
		return audience.EventIdentity{}, audience.ErrEventNameUnresolved
	}
	return identity, nil
}

// enrichIdentity asks the model for the brand token, and for a name or dates only
// where the parse produced none.
func (x *AudienceExplorer) enrichIdentity(ctx context.Context, identity *audience.EventIdentity, details eventurl.EventDetails) {
	if x.llm == nil {
		return
	}
	prompt := strings.Join([]string{
		"Event page metadata:",
		"name: " + details.Name,
		"description: " + details.Description,
		"location: " + details.Location,
		"start: " + details.StartDate,
		"end: " + details.EndDate,
	}, "\n")
	raw, err := x.llm.Complete(ctx, audience.EventExtractionSystemPrompt, prompt)
	if err != nil {
		// Degrading is correct: without a brand token the brand-scoped suppression
		// probes are skipped and the standard ones still run. Nothing is
		// misclassified, only fewer rows are offered.
		slog.WarnContext(ctx, "audience discovery could not extract an event brand", "error", err)
		return
	}
	var extracted struct {
		EventName  string   `json:"eventName"`
		BrandShort string   `json:"brandShort"`
		EventDates []string `json:"eventDates"`
	}
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &extracted); err != nil {
		slog.WarnContext(ctx, "audience discovery got an unparseable event extraction", "error", err)
		return
	}
	identity.BrandShort = strings.TrimSpace(extracted.BrandShort)
	if identity.Name == "" {
		identity.Name = strings.TrimSpace(extracted.EventName)
	}
	if len(identity.Dates) == 0 {
		identity.Dates = audience.SanitizeEventDates(extracted.EventDates)
	}
}

// stripJSONFence unwraps a fenced code block around a model's JSON reply, matching
// parseEmailCopyResponse in internal/service — the same model behind the same LiteLLM
// proxy answers both, and it fences often enough that not handling it here would drop
// the brand token on a reply that is otherwise perfectly good.
func stripJSONFence(raw string) string {
	out := strings.TrimSpace(raw)
	out = strings.TrimPrefix(out, "```json")
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(out, "```")
	return strings.TrimSpace(out)
}

// ---------------------------------------------------------------------------
// Search, suppression, last-sent, existing masters
// ---------------------------------------------------------------------------

// SearchLists is the typeahead over the portal's contact lists, for attaching a list
// by hand. Unfiltered by any predicate on purpose: the operator is the filter.
func (x *AudienceExplorer) SearchLists(ctx context.Context, projectID, query string) (rows []audience.ListRow, err error) {
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	hits, err := client.SearchLists(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]audience.ListRow, 0, len(hits))
	for i := range hits {
		out = append(out, listRow(&hits[i]))
	}
	return out, nil
}

// SuppressionLists resolves the suppression rows an operator chooses from: the six
// fixed portfolio-wide hygiene lists, the brand's own opt-out list, and the event's
// own suppression list carried over from a prior edition.
//
// A row whose list does not resolve is returned with an empty ListID rather than
// dropped. An operator who cannot see that the GDPR row is unavailable will assume it
// was applied.
func (x *AudienceExplorer) SuppressionLists(ctx context.Context, projectID, brandShort, eventName string) (rows []audience.SuppressionRow, err error) {
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	keywords := audience.EventKeywords(eventName)
	out := make([]audience.SuppressionRow, 0, len(audience.StandardSuppressionTerms)+2)

	for _, term := range audience.StandardSuppressionTerms {
		row := audience.SuppressionRow{Key: term.Key, Label: term.Label, Category: audience.SuppressionCategoryStandard}
		if hit := x.bestMatch(ctx, client, []string{term.SearchTerm}, func(name string) bool {
			return audience.MatchesStandardSuppression(name, term.SearchTerm)
		}); hit != nil {
			row.ListID, row.Name, row.Size, row.HubSpotURL = hit.ListID, hit.Name, sizeOf(hit), hit.AppURL
		}
		out = append(out, row)
	}

	if probe := audience.BrandOptOutProbe(brandShort); probe != "" {
		if hit := x.bestMatch(ctx, client, []string{probe}, func(name string) bool {
			return audience.MatchesBrandOptOut(name, brandShort)
		}); hit != nil {
			out = append(out, audience.SuppressionRow{
				Key:        "brand_global_opt_outs",
				Label:      audience.BrandOptOutLabel(brandShort),
				Category:   audience.SuppressionCategoryBrand,
				ListID:     hit.ListID,
				Name:       hit.Name,
				Size:       sizeOf(hit),
				HubSpotURL: hit.AppURL,
			})
		}
	}

	if probes := audience.EventSuppressionProbes(eventName); len(probes) > 0 {
		if hit := x.bestMatch(ctx, client, probes, func(name string) bool {
			return audience.MatchesEventSuppression(name, keywords)
		}); hit != nil {
			out = append(out, audience.SuppressionRow{
				Key:        "event_suppression",
				Label:      audience.EventSuppressionLabel(eventName),
				Category:   audience.SuppressionCategoryEvent,
				ListID:     hit.ListID,
				Name:       hit.Name,
				Size:       sizeOf(hit),
				HubSpotURL: hit.AppURL,
			})
		}
	}

	// The brand and event rows are APPENDED rather than resolved-or-placeheld,
	// because unlike the fixed six they are not expected to exist: most events have
	// no prior edition's suppression list, and an empty row for one would read as a
	// missing required suppression.
	return out, nil
}

// ExistingMasterLists finds master lists already composed for this event family,
// newest quarter first — so an operator can reuse last edition's rather than compose
// a near-duplicate under a slightly different name.
func (x *AudienceExplorer) ExistingMasterLists(ctx context.Context, projectID, eventName, brandShort string) (masters []audience.ListRow, err error) {
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	keywords := audience.EventKeywords(eventName)
	seen := map[string]struct{}{}
	matches := make([]audience.ListCandidate, 0, 8)
	rows := map[string]audience.ListRow{}

	for _, probe := range audience.ExistingMasterProbes(brandShort, eventName) {
		hits, serr := client.SearchLists(ctx, probe)
		if serr != nil {
			// A credential rejection fails every probe alike; swallowing them all would
			// report "no master lists exist" for a broken connection.
			if hubspot.IsPermissionRejection(serr) {
				return nil, serr
			}
			slog.WarnContext(ctx, "existing-master search failed", "project_id", projectID, "error", serr)
			continue
		}
		for i := range hits {
			hit := &hits[i]
			if _, dup := seen[hit.ListID]; dup {
				continue
			}
			if !audience.MatchesExistingMaster(hit.Name, keywords) {
				continue
			}
			seen[hit.ListID] = struct{}{}
			matches = append(matches, listCandidate(hit))
			rows[hit.ListID] = listRow(hit)
		}
	}

	sort.SliceStable(matches, func(i, j int) bool { return audience.NewerFirst(matches[i], matches[j]) })
	out := make([]audience.ListRow, 0, len(matches))
	for _, m := range matches {
		out = append(out, rows[m.ListID])
	}
	return out, nil
}

// LastSent reports the most recent marketing emails for this event family and the
// lists each one targeted.
//
// Only PUBLISHED emails count. A draft's selection is what someone was in the middle
// of deciding, and presenting it as precedent would recommend an audience nobody ever
// approved.
func (x *AudienceExplorer) LastSent(ctx context.Context, projectID, eventName, brandShort string, limit int) (sent []audience.LastSentEmail, err error) {
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	if limit <= 0 {
		limit = 3
	}
	keywords := audience.EventKeywords(eventName)

	type ranked struct {
		email   hubspot.Email
		overlap int
	}
	seen := map[string]struct{}{}
	found := make([]ranked, 0, 8)
	for _, term := range audience.LastSentSearchTerms(eventName, brandShort) {
		emails, serr := client.SearchEmails(ctx, term)
		if serr != nil {
			if errors.Is(serr, hubspot.ErrSearchIncomplete) {
				// The scan bound was reached having matched nothing, so "no prior
				// send" would be an absence nobody established. Try the next term.
				continue
			}
			slog.WarnContext(ctx, "last-sent email search failed", "project_id", projectID, "error", serr)
			continue
		}
		for _, email := range emails {
			if _, dup := seen[email.ID]; dup {
				continue
			}
			if !isPublished(email.State) {
				continue
			}
			seen[email.ID] = struct{}{}
			found = append(found, ranked{email: email, overlap: audience.KeywordOverlap(email.Name, keywords)})
		}
		if len(found) > 0 {
			// The first term that matched anything wins. The brand fallback exists for
			// a renamed or first-time event; letting it also contribute rows to a
			// successful event-name match would mix another event's sends into this
			// event's precedent.
			break
		}
	}

	// Most keyword overlap first, then most recently updated — SearchEmails already
	// returns newest-first, and sort.SliceStable preserves that within a tie.
	sort.SliceStable(found, func(i, j int) bool { return found[i].overlap > found[j].overlap })
	if len(found) > limit {
		found = found[:limit]
	}

	out := make([]audience.LastSentEmail, 0, len(found))
	for _, r := range found {
		row := audience.LastSentEmail{
			EmailID:          r.email.ID,
			EmailName:        r.email.Name,
			HubSpotURL:       r.email.AppURL,
			IncludedLists:    []audience.ListBrief{},
			SuppressionLists: []audience.ListBrief{},
		}
		sendLists, lerr := client.GetEmailSendLists(ctx, r.email.ID)
		if lerr != nil {
			// The email is still worth showing: its name and link are the operator's
			// route into HubSpot to read the selection by hand.
			slog.WarnContext(ctx, "could not read a last-sent email's lists",
				"project_id", projectID, "email_id", r.email.ID, "error", lerr)
			out = append(out, row)
			continue
		}
		row.SentAt = sendLists.PublishDate
		row.IncludedLists = x.listBriefs(ctx, client, sendLists.Include, sendLists.LegacyInclude)
		row.SuppressionLists = x.listBriefs(ctx, client, sendLists.Exclude, sendLists.LegacyExclude)
		out = append(out, row)
	}
	return out, nil
}

// isPublished reports whether an email state means it actually went out. HubSpot
// spells the state in several ways across API versions, so the check is a prefix/
// substring rather than an equality against one spelling.
func isPublished(state string) bool {
	s := strings.ToUpper(strings.TrimSpace(state))
	return strings.Contains(s, "PUBLISHED") || strings.Contains(s, "SENT") || strings.Contains(s, "AUTOMATED")
}

// listBriefs resolves referenced list ids to names, v3 ids first and legacy ids
// second.
func (x *AudienceExplorer) listBriefs(ctx context.Context, client *hubspot.Client, ids, legacyIDs []string) []audience.ListBrief {
	out := make([]audience.ListBrief, 0, len(ids)+len(legacyIDs))
	for _, id := range audience.UniqueIDs(ids) {
		brief := audience.ListBrief{ListID: id, Missing: true}
		if list, err := client.GetList(ctx, id); err == nil && list != nil {
			brief.Name, brief.Size, brief.Missing = list.Name, sizeOf(list), false
		} else if name := client.LegacyListName(ctx, id); name != "" {
			// A v3 id that only resolves under the legacy API still names a real list.
			brief.Name, brief.Missing = name, false
		}
		out = append(out, brief)
	}
	for _, id := range audience.UniqueIDs(legacyIDs) {
		brief := audience.ListBrief{ListID: id, ResolvedFromLegacyID: id, Missing: true}
		if name := client.LegacyListName(ctx, id); name != "" {
			brief.Name, brief.Missing = name, false
		}
		out = append(out, brief)
	}
	return out
}

// bestMatch runs each probe until one yields a list the predicate accepts, and
// returns the largest such list.
//
// Largest, not first: these probes resolve HYGIENE lists, and where a portal holds
// both "GDPR Suppression" and an abandoned "GDPR Suppression (old)", the one with
// more members is the live one. Under-applying a suppression is the failure that
// reaches a contact who asked not to be reached, so the tie is broken towards
// excluding more people.
//
// A probe whose search fails is skipped: the next probe may still resolve the row,
// and a row that resolves to nothing is rendered as unavailable rather than as an
// error.
func (x *AudienceExplorer) bestMatch(ctx context.Context, client *hubspot.Client, probes []string, accept func(name string) bool) *hubspot.List {
	for _, probe := range probes {
		hits, err := client.SearchLists(ctx, probe)
		if err != nil {
			// NOTE: a credential rejection is NOT propagated here, unlike the discovery
			// and existing-master search loops above. bestMatch returns only *hubspot.List
			// and has three inline `if hit := ...` callers, so threading an error through
			// is a wider change than this fix should make silently. The consequence is
			// bounded: suppression probes feed the OPTIONAL suppression panel, and the
			// caller's own credential resolution already failed loudly before reaching
			// here for a wholly revoked token. Tracked rather than half-done.
			slog.WarnContext(ctx, "suppression list search failed", "error", err)
			continue
		}
		var best *hubspot.List
		for i := range hits {
			hit := &hits[i]
			if !accept(hit.Name) {
				continue
			}
			if best == nil || hit.Size > best.Size {
				best = hit
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Preview count
// ---------------------------------------------------------------------------

// previewCountTimeout bounds the whole preview sweep: up to PreviewMaxLists GetList
// calls plus, below the cap, paginated membership reads. It must be shorter than the
// gateway's patience -- the failure this replaces was a 504 with nothing in this
// service's log -- yet long enough that a legitimate selection completes. It is also
// deliberately far below the HubSpot client's own retryMax*maxRetryWait (180s): under
// sustained throttling the retry policy alone would outlast any request budget, and a
// preview must degrade to a stated over-count rather than hang.
const previewCountTimeout = 25 * time.Second

// PreviewCount answers "how many people would this reach" for a selection of lists.
//
// Three answers, and which one is given matters as much as the number:
//   - an EXACT union, when the selection is small enough to enumerate;
//   - the SUM as a stated over-count, when it is not (the union can only be smaller);
//   - the SUM as a stated over-count, when the live sweep failed or was truncated.
//
// The last case is why a truncated membership is never folded into an exact answer:
// a partial union UNDER-counts, and telling an operator an email reaches fewer people
// than it does is the one error direction with no recovery after the send.
func (x *AudienceExplorer) PreviewCount(ctx context.Context, projectID string, listIDs []string) (count audience.PreviewCount, err error) {
	ids := audience.UniqueIDs(listIDs)
	if len(ids) == 0 {
		return audience.EmptyPreviewCount(), nil
	}
	// Defence in depth behind the design's MaxLength: that bound guards the HTTP
	// edge, this one guards every caller of the method (a future internal one
	// included) and is what the budget comment on PreviewMaxLists actually promises.
	if len(ids) > audience.PreviewMaxLists {
		return audience.PreviewCount{}, audience.ErrTooManyPreviewLists
	}
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return audience.PreviewCount{}, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	// The sweep below is up to 2*PreviewMaxLists sequential round-trips. Without a
	// deadline the request runs until the gateway kills it, which surfaces to the
	// operator as a bare 504 and leaves no line in this service's log saying why.
	// Bounding it here turns that into a degraded-but-honest answer.
	ctx, cancel := context.WithTimeout(ctx, previewCountTimeout)
	defer cancel()

	sizes := make([]*int64, 0, len(ids))
	for _, id := range ids {
		list, gerr := client.GetList(ctx, id)
		if gerr != nil {
			if hubspot.IsNotFound(gerr) {
				return audience.PreviewCount{}, fmt.Errorf("audience preview: list %s: %w", id, audience.ErrListNotFound)
			}
			return audience.PreviewCount{}, fmt.Errorf("audience preview: read list %s: %w", id, gerr)
		}
		// `list.Size` is a plain int, so an OMITTED size is indistinguishable from a
		// genuinely empty list -- `sizeOf` exists precisely to keep them apart. Summing
		// an omitted size as 0 makes the total short by that whole list, and if the
		// membership sweep below then fails, that short sum is returned as the "safe"
		// over-count. Understating reach is the one direction with no recovery after a
		// send, so an unknown size stops the estimate rather than silently shrinking it.
		sizes = append(sizes, sizeOf(list))
	}
	estimate, known := sumKnownSizes(sizes)
	if !known {
		return audience.UnknownSizePreviewCount(), nil
	}
	if audience.ExceedsExactCap(estimate) {
		return audience.OverCapPreviewCount(estimate), nil
	}

	union := make(map[string]struct{}, estimate)
	for _, id := range ids {
		members, truncated, merr := client.ListMembershipIDs(ctx, id)
		if merr != nil || truncated {
			if merr != nil {
				slog.WarnContext(ctx, "audience preview membership sweep failed",
					"project_id", projectID, "list_id", id, "error", merr)
			}
			return audience.DegradedPreviewCount(estimate), nil
		}
		for _, member := range members {
			union[member] = struct{}{}
		}
	}
	return audience.ExactPreviewCount(len(union), estimate), nil
}

// sumKnownSizes totals the selected lists' sizes, reporting whether every one was known.
//
// A nil entry is a size HubSpot did not report. `hubspot.List.Size` is a plain int, so an
// omitted size is indistinguishable from a genuinely empty list; `sizeOf` returns *int64
// to keep them apart, and this is where that distinction has to be honoured. Summing a nil
// as zero makes the total short by that entire list, and PreviewCount then hands the short
// sum back as the "safe" over-count when the membership sweep fails — understating reach,
// which is the one direction with no recovery after a send.
//
// Extracted from PreviewCount so the decision is reachable in a unit test: the call site
// needs a live portal and an encrypted connection to exercise, and a guard no test can
// reach is a guard that silently stops working.
func sumKnownSizes(sizes []*int64) (total int, allKnown bool) {
	for _, size := range sizes {
		if size == nil {
			return 0, false
		}
		total += int(*size)
	}
	return total, true
}

// ---------------------------------------------------------------------------
// Compose
// ---------------------------------------------------------------------------

// ComposeMaster creates the combined suppression list and then the master list in the
// project's portal.
//
// Ordering is fixed and load-bearing: the suppression list must exist before the
// master can reference its id inside every branch of the union. That is also what
// makes this NOT idempotent — a failure between the two creates the orphan
// ComposePartialError describes.
func (x *AudienceExplorer) ComposeMaster(ctx context.Context, projectID string, in audience.ComposeInput) (*audience.ComposeOutcome, error) {
	include := audience.UniqueIDs(in.ListIDs)
	if len(include) == 0 {
		return nil, audience.ErrNoInclusionLists
	}
	exclude := audience.ExclusionIDs(in.ExcludeListIDs, include)

	// One resolved client for the whole composition. Both lists must land in the SAME
	// portal, or the master references a suppression id that does not exist beside it.
	ctx = x.builder.BeginBuild(ctx)
	if _, _, err := x.builder.cachedClient(ctx, projectID); err != nil {
		return nil, err
	}

	identity := audience.EventIdentity{Name: in.EventName, BrandShort: in.BrandShort, Dates: in.EventDates}
	masterName := strings.TrimSpace(in.Name)
	if masterName == "" {
		masterName = audience.MasterListName(identity, "Master", x.now())
	}

	var suppression *audience.ComposedList
	var filter json.RawMessage
	var err error

	if len(exclude) > 0 {
		suppressionFilter, ferr := audience.MasterListFilter(exclude)
		if ferr != nil {
			return nil, ferr
		}
		suppressionName := audience.CombinedSuppressionName(in.Name, identity, x.now())
		created, cerr := x.createList(ctx, projectID, suppressionName, suppressionFilter)
		if cerr != nil {
			return nil, fmt.Errorf("audience compose: create combined suppression list: %w", cerr)
		}
		suppression = created
		filter, err = audience.MasterListWithSuppressionFilter(include, created.ListID)
	} else {
		filter, err = audience.MasterListFilter(include)
	}
	if err != nil {
		return nil, err
	}

	master, cerr := x.createList(ctx, projectID, masterName, filter)
	if cerr != nil {
		if suppression != nil {
			return nil, &audience.ComposePartialError{Suppression: *suppression, Err: cerr}
		}
		return nil, fmt.Errorf("audience compose: create master list: %w", cerr)
	}

	return &audience.ComposeOutcome{
		Master:        *master,
		Suppression:   suppression,
		SourceListIDs: include,
	}, nil
}

// createList creates one list and reads it back for its size and link.
func (x *AudienceExplorer) createList(ctx context.Context, projectID, name string, filter json.RawMessage) (*audience.ComposedList, error) {
	client, fromSystem, err := x.builder.cachedClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Only a PERMISSION rejection is tagged, and only with its origin — 401/403 creates
	// nothing, so there is no unconfirmed state to preserve and the operator arm needs to
	// know whose credential was refused.
	//
	// Everything else is passed through unwrapped so an UNCONFIRMED create keeps its
	// "verify before retrying" classification rather than flattening into a failure a
	// caller would retry into a duplicate list.
	created, cerr := client.CreateList(ctx, name, filter)
	if cerr != nil {
		return nil, systemScopedHubSpot(cerr, fromSystem)
	}
	if created == nil {
		return nil, fmt.Errorf("hubspot: create list %q returned no list", name)
	}
	return &audience.ComposedList{ListRow: listRow(created)}, nil
}

// ---------------------------------------------------------------------------
// QA
// ---------------------------------------------------------------------------

// RunQA audits one composed list: whether it selects on real engagement, whether the
// consent suppressions are applied, and whether it excludes anything at all.
//
// It creates and changes nothing, and its verdict is evidence for a human decision
// rather than a gate — see internal/audience/builder_qa.go.
func (x *AudienceExplorer) RunQA(ctx context.Context, projectID, listRef string, targetsEU, targetsCA bool) (outcome *audience.QaOutcome, err error) {
	client, fromSystem, err := x.builder.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { err = systemScopedHubSpot(err, fromSystem) }()

	listID, isName := audience.ParseListRef(listRef)
	if isName {
		hits, serr := client.SearchLists(ctx, listRef)
		if serr != nil {
			return nil, serr
		}
		candidates := make([]audience.ListCandidate, 0, len(hits))
		rows := map[string]*hubspot.List{}
		for i := range hits {
			candidates = append(candidates, listCandidate(&hits[i]))
			rows[hits[i].ListID] = &hits[i]
		}
		chosen, ambiguous := audience.PickNameMatches(listRef, candidates)
		switch {
		case len(ambiguous) > 0:
			out := &audience.QaOutcome{NeedsDisambiguation: true, Candidates: make([]audience.QaCandidate, 0, len(ambiguous))}
			for _, c := range ambiguous {
				out.Candidates = append(out.Candidates, audience.QaCandidate{
					ListID: c.ListID, Name: c.Name, Size: sizeOf(rows[c.ListID]),
				})
			}
			return out, nil
		case chosen == nil:
			return nil, fmt.Errorf("%w: %q", audience.ErrListNotFound, listRef)
		default:
			listID = chosen.ListID
		}
	}
	if strings.TrimSpace(listID) == "" {
		return nil, fmt.Errorf("%w: a list id or name is required", audience.ErrInvalidRequest)
	}

	list, filters, gerr := x.listWithFilters(ctx, client, listID)
	if gerr != nil {
		return nil, gerr
	}

	// Names of every list referenced in EITHER direction. The names are what the
	// checks classify on — "IN_LIST 12345" carries no signal, while "LF Events GDPR
	// Suppression" carries all of it.
	nameByID := map[string]string{}
	for _, operator := range []string{"IN_LIST", "NOT_IN_LIST"} {
		for _, id := range audience.ReferencedListIDs(filters, operator) {
			if _, done := nameByID[id]; done {
				continue
			}
			nameByID[id] = x.listName(ctx, client, id)
		}
	}

	exclusionNames := x.exclusionNames(ctx, client, filters, nameByID)

	signalMapping := audience.CheckSignalMapping(filters, nameByID)
	suppression := audience.CheckSuppression(exclusionNames, targetsEU, targetsCA)
	completeness := audience.CheckExclusionCompleteness(filters)

	findings := make([]audience.Finding, 0, 6)
	findings = append(findings, signalMapping.Findings...)
	findings = append(findings, suppression.Findings...)
	findings = append(findings, completeness.Findings...)
	findings = audience.OrderFindings(findings)

	return &audience.QaOutcome{
		ListID:     list.ListID,
		Name:       list.Name,
		HubSpotURL: list.AppURL,
		Checks: audience.QaChecks{
			SignalMapping:         signalMapping,
			Suppression:           suppression,
			ExclusionCompleteness: completeness,
		},
		Findings: findings,
		Overall:  audience.CombineVerdicts(signalMapping.Verdict, suppression.Verdict, completeness.Verdict),
	}, nil
}

// exclusionNames collects the names of everything the list excludes, following ONE
// hop into each excluded list's own inclusions.
//
// The hop is required, not defensive: ComposeMaster wraps every exclusion in a single
// "… - Combined Suppression" list, so a master composed by this service excludes ONE
// list whose name says nothing about GDPR or opt-outs. Without the hop, every list
// this service creates would fail its own suppression check.
//
// Exactly one hop. Suppression lists reference each other, and an unbounded walk
// across a portal's exclusion graph would spend the request's deadline reading lists
// whose bearing on this audience is already indirect.
func (x *AudienceExplorer) exclusionNames(ctx context.Context, client *hubspot.Client, filters []audience.ListFilter, nameByID map[string]string) []string {
	out := make([]string, 0, 8)
	for _, id := range audience.ReferencedListIDs(filters, "NOT_IN_LIST") {
		if name := nameByID[id]; name != "" {
			out = append(out, name)
		}
		_, childFilters, err := x.listWithFilters(ctx, client, id)
		if err != nil {
			// An unreadable exclusion is reported by its own name only. Failing the
			// whole audit would withhold the two checks that did complete.
			continue
		}
		for _, childID := range audience.ReferencedListIDs(childFilters, "IN_LIST") {
			if name := x.listName(ctx, client, childID); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// listName resolves a list id to a name, falling back to the legacy API.
//
// A referenced list can predate the v3 Lists API while still being referenced by a
// current list's filters — and for QA a name that cannot be read is a suppression
// that cannot be credited. "" means genuinely unresolvable.
func (x *AudienceExplorer) listName(ctx context.Context, client *hubspot.Client, listID string) string {
	if list, err := client.GetList(ctx, listID); err == nil && list != nil {
		return list.Name
	}
	return client.LegacyListName(ctx, listID)
}

// listWithFilters fetches a list with its filters and flattens them.
//
// A list with NO filterBranch is not an error: a manual/static list selects by
// membership rather than by condition, and the checks have specific things to say
// about that.
func (x *AudienceExplorer) listWithFilters(ctx context.Context, client *hubspot.Client, listID string) (*hubspot.List, []audience.ListFilter, error) {
	list, err := client.GetList(ctx, listID)
	if err != nil {
		// A 404 means the portal holds no such list, which the contract declares as a 404.
		// Passing the raw API error up sends it to audienceExploreErr's default arm and a
		// generic 500, telling the operator the service broke when the real answer is that
		// the id they gave does not exist.
		if hubspot.IsNotFound(err) {
			return nil, nil, fmt.Errorf("hubspot: list %s: %w", listID, audience.ErrListNotFound)
		}
		return nil, nil, err
	}
	if list == nil {
		return nil, nil, fmt.Errorf("hubspot: list %s returned no list", listID)
	}
	branch, perr := audience.ParseFilterBranch(list.FilterBranch)
	if perr != nil {
		// An unparseable branch is reported as NO filters rather than as a failure:
		// HubSpot's filter schema is a growing union, and an unmodelled member must
		// fall through to "could not determine", never break the read.
		slog.WarnContext(ctx, "could not parse a list's filter branch", "list_id", listID, "error", perr)
		return list, nil, nil
	}
	return list, audience.CollectFilters(branch), nil
}

// ---------------------------------------------------------------------------
// Mapping helpers
// ---------------------------------------------------------------------------

// listRow maps a HubSpot list to the row shape the pickers render.
func listRow(l *hubspot.List) audience.ListRow {
	if l == nil {
		return audience.ListRow{}
	}
	return audience.ListRow{ListID: l.ListID, Name: l.Name, Size: sizeOf(l), HubSpotURL: l.AppURL}
}

// listCandidate maps a HubSpot list to the ranking shape.
//
// An UNREPORTED size becomes -1, which is the whole reason this conversion exists:
// hubspot.List collapses "no size reported" into 0, and ranking a list of unknown
// size as the smallest match would push the real current-quarter list below an
// abandoned draft.
func listCandidate(l *hubspot.List) audience.ListCandidate {
	candidate := audience.ListCandidate{ListID: l.ListID, Name: l.Name, Size: -1}
	if size := sizeOf(l); size != nil {
		candidate.Size = int(*size)
	}
	return candidate
}

// sizeOf reports a list's membership size, or nil when HubSpot reported none.
//
// nil and 0 are different answers and must stay different all the way to the wire: a
// list whose size was never returned rendered as "0 contacts" tells an operator the
// list is empty when it may hold fifty thousand people.
func sizeOf(l *hubspot.List) *int64 {
	if l == nil {
		return nil
	}
	if l.TopLevelSize != nil {
		size := int64(*l.TopLevelSize)
		return &size
	}
	raw, present := l.AdditionalProperties[hsListSizePropKey]
	if !present {
		return nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	size := int64(parsed)
	return &size
}

// hsListSizePropKey mirrors the hubspot package's unexported additionalProperties
// key for a search hit's size. Duplicated rather than exported because it is a
// property name in HubSpot's schema, not part of this service's contract — and this
// is the only caller that needs to tell "absent" from "zero".
const hsListSizePropKey = "hs_list_size"
