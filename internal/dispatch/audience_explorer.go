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
// It resolves a client and throws it away. That is the only LOCAL test available:
// "a connection row exists" is not the same as "a client can be built from it", and
// the difference (a connection deactivated, or an undecryptable credential) is
// exactly what leaves an operator staring at a tab whose every button fails.
//
// SCOPE, stated because the field name over-promises: `HubSpotConfigured: true` means
// "a client can be BUILT for this project", not "that client can talk to HubSpot". A
// syntactically valid but revoked or expired token passes this check — nothing here
// makes an authenticated call. That is deliberate: this endpoint gates whether the tab
// renders at all, so paying for a live round-trip on every page load to catch a case
// the first real request surfaces anyway is the wrong trade. The real requests DO fail
// closed on a revoked token now (the search paths propagate permission rejections
// rather than reporting an empty portal), so the operator gets a typed 503 on first use
// instead of a silent empty state.
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
	cache := map[string]*hubspot.List{}

	for _, candidate := range candidates {
		if out.Inspected >= audience.DiscoveryMaxInspections {
			break
		}
		out.Inspected++
		list, filters, gerr := x.listWithFilters(ctx, client, cache, candidate.ListID)
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
		//
		// An exclusion-only rollup (every filter is NOT_IN_LIST) has no INCLUDED
		// children at all: RollupChildIDs skips exclusion references by design, so
		// childIDs comes back empty here even though IsRollup is true. That must not
		// silently drop the list from the outcome -- classify the list itself instead
		// of falling through to the loop's `continue`, which would leave it neither
		// classified nor reported.
		if childIDs := audience.RollupChildIDs(filters); audience.IsRollup(filters) && len(childIDs) > 0 {
			for _, childID := range childIDs {
				if out.Inspected >= audience.DiscoveryMaxInspections {
					break
				}
				out.Inspected++
				child, childFilters, cerr := x.listWithFilters(ctx, client, cache, childID)
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
	// The page is third-party content an operator merely pointed at, so it is delimited and
	// declared untrusted rather than concatenated in raw. The markers are what the system prompt
	// tells the model to treat as data; `sanitizePromptField` keeps the content from closing them
	// early or spanning lines, so injected text cannot escape the block it is quoted in.
	prompt := strings.Join([]string{
		"BEGIN PAGE METADATA",
		"name: " + sanitizePromptField(details.Name),
		"description: " + sanitizePromptField(details.Description),
		"location: " + sanitizePromptField(details.Location),
		"start: " + sanitizePromptField(details.StartDate),
		"end: " + sanitizePromptField(details.EndDate),
		"END PAGE METADATA",
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
	// The model's reply is derived from untrusted page content, so it is bounded on the way out
	// as well as on the way in. BrandShort reaches EventKeywords and MasterListName, and Name is
	// used the same way — a multi-line or unbounded value there would carry injected text into a
	// produced list name. Neither is interpreted as an instruction anywhere, so collapsing and
	// truncating is enough; nothing here needs to reject a merely odd-looking brand.
	identity.BrandShort = sanitizeExtractedField(extracted.BrandShort)
	if identity.Name == "" {
		identity.Name = sanitizeExtractedField(extracted.EventName)
	}
	if len(identity.Dates) == 0 {
		identity.Dates = audience.SanitizeEventDates(extracted.EventDates)
	}
}

// extractedFieldMaxLen bounds a model-returned identity field. Real event names and brand
// tokens are far shorter; this only has to stop an unbounded one reaching a list name.
const extractedFieldMaxLen = 200

// sanitizeExtractedField collapses whitespace and bounds a value the model returned.
func sanitizeExtractedField(v string) string {
	out := strings.Join(strings.Fields(v), " ")
	if len(out) > extractedFieldMaxLen {
		out = out[:extractedFieldMaxLen]
	}
	return out
}

// promptFieldMaxLen bounds a single scraped field in the prompt. A page can carry an
// arbitrarily long description, and the budget matters less than the fact that a very long
// field is where injected text hides.
const promptFieldMaxLen = 500

// sanitizePromptField makes one scraped value safe to quote inside the delimited block.
//
// Newlines are collapsed because the block is line-oriented: a value containing "\nEND PAGE
// METADATA" would otherwise close the block early and everything after it would read as
// instructions rather than data. The marker words are neutralised for the same reason, since
// collapsing lines alone still lets a value end the block if the model is lenient about
// surrounding whitespace. Truncation bounds the rest.
func sanitizePromptField(v string) string {
	out := strings.Join(strings.Fields(v), " ")
	out = strings.ReplaceAll(out, "BEGIN PAGE METADATA", "[removed]")
	out = strings.ReplaceAll(out, "END PAGE METADATA", "[removed]")
	if len(out) > promptFieldMaxLen {
		out = out[:promptFieldMaxLen]
	}
	return out
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
	// MinLength(1) at the edge accepts "   ", which the HubSpot client then trims to an empty
	// query and answers by walking every list page — turning a typeahead keystroke into the
	// endpoint's worst-case fan-out. Rejected here as the invalid request it is.
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: a list search needs a non-whitespace query", audience.ErrInvalidRequest)
	}
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
		hit, berr := x.bestMatch(ctx, client, []string{term.SearchTerm}, func(name string) bool {
			return audience.MatchesStandardSuppression(name, term.SearchTerm)
		})
		if berr != nil {
			return nil, berr
		}
		if hit != nil {
			row.ListID, row.Name, row.Size, row.HubSpotURL = hit.ListID, hit.Name, sizeOf(hit), hit.AppURL
		}
		out = append(out, row)
	}

	if probe := audience.BrandOptOutProbe(brandShort); probe != "" {
		hit, berr := x.bestMatch(ctx, client, []string{probe}, func(name string) bool {
			return audience.MatchesBrandOptOut(name, brandShort)
		})
		if berr != nil {
			return nil, berr
		}
		if hit != nil {
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
		hit, berr := x.bestMatch(ctx, client, probes, func(name string) bool {
			return audience.MatchesEventSuppression(name, keywords)
		})
		if berr != nil {
			return nil, berr
		}
		if hit != nil {
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
	// Remembers a scan-bound hit so an all-incomplete sweep cannot return as an empty history.
	var incomplete error
	for _, term := range audience.LastSentSearchTerms(eventName, brandShort) {
		emails, serr := client.SearchEmails(ctx, term)
		if serr != nil {
			// A revoked token fails EVERY term alike, so absorbing them all returned 200
			// with an empty history — telling the operator there were no prior sends when
			// the truth is the credential no longer works.
			if hubspot.IsPermissionRejection(serr) {
				return nil, serr
			}
			if errors.Is(serr, hubspot.ErrSearchIncomplete) {
				// The scan bound was reached having matched nothing, so "no prior
				// send" would be an absence nobody established. Try the next term —
				// but REMEMBER it: if no later term finds anything either, returning
				// an empty list would report that unestablished absence as fact.
				incomplete = serr
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
			// A term matched, so the history is established regardless of what an
			// earlier term's scan bound did.
			incomplete = nil
			// The first term that matched anything wins. The brand fallback exists for
			// a renamed or first-time event; letting it also contribute rows to a
			// successful event-name match would mix another event's sends into this
			// event's precedent.
			break
		}
	}

	// Every term that ran hit its scan bound without matching, so nothing here establishes
	// that there is no prior send. Saying so is the only honest answer: an empty list would
	// be read as "this event has never been emailed", which is what the operator uses to
	// decide whether a precedent exists at all.
	if len(found) == 0 && incomplete != nil {
		return nil, incomplete
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
// newerSuppression ranks two candidate suppression lists: newest quarter first, size as the
// same-quarter tiebreak. Mirrors audience.NewerFirst, which takes a ListCandidate rather than
// the *hubspot.List this path carries.
//
// The size tiebreak goes through sizeOf, not the raw Size field: a hit with a known size
// always outranks one HubSpot reported no size for, and reported "no size" must never be
// read as zero (see sizeOf) — comparing raw ints would let an unranked hit tie or beat a
// hit with a genuinely known (even zero) size.
func newerSuppression(a, b *hubspot.List) bool {
	aYear, aQuarter := audience.QuarterRank(a.Name)
	bYear, bQuarter := audience.QuarterRank(b.Name)
	if aYear != bYear {
		return aYear > bYear
	}
	if aQuarter != bQuarter {
		return aQuarter > bQuarter
	}
	aSize, bSize := sizeOf(a), sizeOf(b)
	switch {
	case aSize == nil && bSize == nil:
		// Neither hit carries a resolvable size (e.g. a directly-constructed
		// candidate that never went through resolveSize) -- fall back to the raw,
		// already-normalized Size field rather than treating both as unranked.
		return a.Size > b.Size
	case aSize == nil:
		return false
	case bSize == nil:
		return true
	default:
		return *aSize > *bSize
	}
}

// Returns an error ONLY for a credential rejection. Every other per-probe failure is absorbed,
// because one failed probe must not lose the others — but a revoked token fails every probe
// alike, and absorbing those made SuppressionLists return 200 with every hygiene row marked
// "unavailable": an authentication outage rendered as "this portal has no GDPR lists", which is
// the one answer that must never be guessed.
func (x *AudienceExplorer) bestMatch(ctx context.Context, client *hubspot.Client, probes []string, accept func(name string) bool) (*hubspot.List, error) {
	var best *hubspot.List
	for _, probe := range probes {
		hits, err := client.SearchLists(ctx, probe)
		if err != nil {
			// A revoked token fails EVERY probe, so absorbing them all reported an empty
			// hygiene set for what is really a credential problem — and bypassed the
			// deferred systemScopedHubSpot classifier that turns it into a typed 503.
			if hubspot.IsPermissionRejection(err) {
				return nil, err
			}
			slog.WarnContext(ctx, "suppression list search failed", "error", err)
			continue
		}
		for i := range hits {
			hit := &hits[i]
			if !accept(hit.Name) {
				continue
			}
			// Newest QUARTER first, size only as the same-quarter tiebreak — mirroring
			// audience.NewerFirst. Ranking by size alone let a larger stale list beat the
			// current quarter's, which contradicts StandardSuppressionTerms' invariant that
			// recurring hygiene lists select the highest YYQN — and can omit contacts who
			// were only ever added to the current list.
			//
			// Ranked across ALL probes, not within each one. Returning on the first probe
			// that matched anything meant the synonyms were an ordered fallback rather than
			// alternate spellings of one list: a stale "... Suppression" won outright even
			// when the later "... Exclusion" probe held the current quarter's list, which is
			// the same stale-list selection the newest-quarter rule exists to prevent.
			if best == nil || newerSuppression(hit, best) {
				best = hit
			}
		}
	}
	return best, nil
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
// Four answers, and which one is given matters as much as the number: an EXACT
// union, when the selection is small enough to enumerate; the SUM as a stated
// OVER-count, when it is not (the union can only be smaller) or when the live
// sweep failed or was truncated; and the SUM as a stated UNDER-count, when one
// or more lists did not report a size at all (that sum omits them entirely).
// See audience.PreviewCount's doc for which caveat direction each case carries.
//
// A truncated membership sweep is never folded into an exact answer: a partial
// union under-counts, and telling an operator an email reaches fewer people
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
	// Reject blank EXCLUSIONS on the raw input, before ExclusionIDs drops them. Dropping one
	// silently composed a master with NO suppression even though the caller asked for one — on a
	// create path that is not idempotent, quietly building something different is worse than a
	// 400. Inclusions keep their existing path: an all-blank selection is still
	// ErrNoInclusionLists below, which is the wrapped sentinel the handler maps to a 400.
	for _, id := range in.ExcludeListIDs {
		if strings.TrimSpace(id) == "" {
			return nil, audience.ErrBlankExclusionID
		}
	}

	include := audience.UniqueIDs(in.ListIDs)
	if len(include) == 0 {
		return nil, audience.ErrNoInclusionLists
	}
	if err := audience.ValidateInclusionIDs(include); err != nil {
		return nil, err
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
			if hubspot.IsUnconfirmed(cerr) {
				return nil, &audience.ComposePartialError{
					Suppression:            audience.ComposedList{ListRow: audience.ListRow{Name: suppressionName}},
					SuppressionUnconfirmed: true,
					Err:                    fmt.Errorf("audience compose: create combined suppression list: %w", cerr),
				}
			}
			return nil, fmt.Errorf("audience compose: create combined suppression list: %w", cerr)
		}
		suppression = created
		filter, err = audience.MasterListWithSuppressionFilter(include, created.ListID)
	} else {
		filter, err = audience.MasterListFilter(include)
	}
	if err != nil {
		if suppression != nil {
			return nil, &audience.ComposePartialError{Suppression: *suppression, Err: err}
		}
		return nil, err
	}

	master, cerr := x.createList(ctx, projectID, masterName, filter)
	if cerr != nil {
		wrapped := fmt.Errorf("audience compose: create master list: %w", cerr)
		if hubspot.IsUnconfirmed(cerr) {
			partial := &audience.ComposePartialError{MasterName: masterName, Err: wrapped}
			if suppression != nil {
				partial.Suppression = *suppression
			}
			return nil, partial
		}
		if suppression != nil {
			return nil, &audience.ComposePartialError{Suppression: *suppression, Err: wrapped}
		}
		return nil, wrapped
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

	cache := map[string]*hubspot.List{}

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
			// rows is name/size-only, from SearchLists -- NOT the filter cache. A
			// search hit never carries FilterBranch (only GetList's includeFilters=true
			// does), so seeding the filter cache from it would make listWithFilters
			// return an empty filter set for the chosen candidate instead of fetching
			// its real filters, silently blanking every downstream QA check.
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

	list, filters, gerr := x.listWithFilters(ctx, client, cache, listID)
	if gerr != nil {
		if hubspot.IsNotFound(gerr) {
			return nil, fmt.Errorf("%w: %q", audience.ErrListNotFound, listID)
		}
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
			nameByID[id] = x.listName(ctx, client, cache, id)
		}
	}

	exclusionNames := x.exclusionNames(ctx, client, cache, filters, nameByID)

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
func (x *AudienceExplorer) exclusionNames(ctx context.Context, client *hubspot.Client, cache map[string]*hubspot.List, filters []audience.ListFilter, nameByID map[string]string) []string {
	out := make([]string, 0, 8)
	for _, id := range audience.ReferencedListIDs(filters, "NOT_IN_LIST") {
		if name := nameByID[id]; name != "" {
			out = append(out, name)
		}
		_, childFilters, err := x.listWithFilters(ctx, client, cache, id)
		if err != nil {
			// An unreadable exclusion is reported by its own name only. Failing the
			// whole audit would withhold the two checks that did complete.
			continue
		}
		for _, childID := range audience.ReferencedListIDs(childFilters, "IN_LIST") {
			if name, done := nameByID[childID]; done {
				if name != "" {
					out = append(out, name)
				}
				continue
			}
			name := x.listName(ctx, client, cache, childID)
			nameByID[childID] = name
			if name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// listName resolves a list id to a name via the cache, falling back to the legacy
// API when the id is not a current v3 list at all.
//
// A referenced list can predate the v3 Lists API while still being referenced by a
// current list's filters — and for QA a name that cannot be read is a suppression
// that cannot be credited. "" means genuinely unresolvable.
func (x *AudienceExplorer) listName(ctx context.Context, client *hubspot.Client, cache map[string]*hubspot.List, listID string) string {
	if list, ok := cache[listID]; ok && list != nil {
		return list.Name
	}
	if list, err := client.GetList(ctx, listID); err == nil && list != nil {
		cache[listID] = list
		return list.Name
	}
	return client.LegacyListName(ctx, listID)
}

// listWithFilters fetches a list with its filters and flattens them, consulting
// cache first — RunQA and exclusionNames both resolve names and filters for
// overlapping list ids, and a cache miss here is a HubSpot round-trip a prior
// lookup in the same request may have already paid for.
//
// A list with NO filterBranch is not an error: a manual/static list selects by
// membership rather than by condition, and the checks have specific things to say
// about that.
func (x *AudienceExplorer) listWithFilters(ctx context.Context, client *hubspot.Client, cache map[string]*hubspot.List, listID string) (*hubspot.List, []audience.ListFilter, error) {
	list, ok := cache[listID]
	if !ok {
		fetched, err := client.GetList(ctx, listID)
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
		list = fetched
		if list != nil {
			cache[listID] = list
		}
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
