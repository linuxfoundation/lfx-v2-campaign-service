// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFetcher returns fixed page bytes, or an error.
type stubFetcher struct {
	body []byte
	err  error
	url  string
}

func (s *stubFetcher) Fetch(_ context.Context, eventURL string) ([]byte, error) {
	s.url = eventURL
	return s.body, s.err
}

// stubParser returns fixed details, ignoring the body.
type stubParser struct{ details eventurl.EventDetails }

func (s stubParser) Parse([]byte) eventurl.EventDetails { return s.details }

// stubLLM returns fixed completion text, or an error.
type stubLLM struct {
	reply  string
	err    error
	called int
}

func (s *stubLLM) Complete(context.Context, string, string) (string, error) {
	s.called++
	return s.reply, s.err
}

// explorerWithNoPortal builds an explorer over a project that has no HubSpot
// connection, so credential resolution reaches its real refusal rather than a
// panic — a nil repo would nil-deref inside credsSource and prove nothing about
// the degrade path. Every method that needs HubSpot therefore fails at the seam,
// and everything before that seam is reachable in a unit test.
func explorerWithNoPortal(fetcher eventPageReader, parser eventPageParser, llm completer) *AudienceExplorer {
	repo := fakeConnReader{err: domain.ErrNotFound}
	return NewAudienceExplorer(NewAudienceBuilder(repo, identityEncryptor{}, nil), fetcher, parser, llm)
}

// TestCapabilities_NeverErrorsAndNeverLeaksTheStore pins both halves of the
// endpoint's contract. It answers rather than fails, because the whole point is to
// let a caller RENDER the explanation — an error here leaves the tab unable to
// explain itself. And the detail is operator-facing prose: the underlying error
// names the connection store and its failure modes, which must not be surfaced.
func TestCapabilities_NeverErrorsAndNeverLeaksTheStore(t *testing.T) {
	got := explorerWithNoPortal(nil, nil, nil).Capabilities(context.Background(), "proj-1")

	assert.False(t, got.HubSpotConfigured)
	assert.NotEmpty(t, got.Detail, "a degraded answer with no explanation gives the UI nothing to show")
	assert.NotContains(t, got.Detail, "connections")
	assert.NotContains(t, got.Detail, "decrypt",
		"credential-store internals must stay in the log, not in an operator's banner")
}

// TestCapabilities_RequiresAProject pins that a blank project is not quietly
// treated as "configured". The builder refuses to resolve credentials without a
// project id precisely so a list is never built in the wrong portal, and this
// endpoint must report that rather than promising a portal that was never chosen.
func TestCapabilities_RequiresAProject(t *testing.T) {
	got := explorerWithNoPortal(nil, nil, nil).Capabilities(context.Background(), "   ")
	assert.False(t, got.HubSpotConfigured)
}

// TestEventIdentity_ReportsAnUnconfiguredPageReader pins the typed sentinel. With
// no fetcher, discovery cannot read the page at all — and the alternative to
// saying so is searching with an empty name, where every predicate rejects
// everything and the operator sees an empty portal instead of a cause.
func TestEventIdentity_ReportsAnUnconfiguredPageReader(t *testing.T) {
	x := explorerWithNoPortal(nil, nil, nil)
	_, err := x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
	require.ErrorIs(t, err, audience.ErrEventPageUnavailable)

	// A fetcher with no parser is the same condition: neither half alone can
	// produce an identity.
	x = explorerWithNoPortal(&stubFetcher{body: []byte("<html></html>")}, nil, nil)
	_, err = x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
	require.ErrorIs(t, err, audience.ErrEventPageUnavailable)
}

// TestEventIdentity_FetchErrorIsPassedThrough pins that an SSRF-guard rejection or
// a dead page reaches the caller intact. Collapsing it into the "not configured"
// sentinel would tell an operator to configure something that already is.
func TestEventIdentity_FetchErrorIsPassedThrough(t *testing.T) {
	boom := errors.New("event url: host is not allowed")
	x := explorerWithNoPortal(&stubFetcher{err: boom}, stubParser{}, nil)

	_, err := x.eventIdentity(context.Background(), "http://169.254.169.254/")
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, audience.ErrEventPageUnavailable)
}

// TestEventIdentity_NoNameIsAnError pins the refusal to search on nothing. An
// empty name yields no queries and no keywords, so discovery would report a portal
// with no qualifying lists — indistinguishable from a portal that genuinely has
// none, and the operator would go build lists that already exist.
func TestEventIdentity_NoNameIsAnError(t *testing.T) {
	x := explorerWithNoPortal(&stubFetcher{body: []byte("x")}, stubParser{details: eventurl.EventDetails{
		Location: "Amsterdam",
	}}, nil)

	_, err := x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
	require.ErrorIs(t, err, audience.ErrEventNameUnresolved)
}

// TestEventIdentity_TakesTheParsedPageOverTheModel pins the precedence that keeps
// discovery deterministic. The page's own declared name and dates are evidence;
// the model's are a reading of it, and letting the model overwrite a parsed name
// would make the same page classify differently between runs.
func TestEventIdentity_TakesTheParsedPageOverTheModel(t *testing.T) {
	llm := &stubLLM{reply: `{"eventName":"Something Else","brandShort":"CNCF","eventDates":["2030-01-01"]}`}
	x := explorerWithNoPortal(
		&stubFetcher{body: []byte("x")},
		stubParser{details: eventurl.EventDetails{
			Name:      "  KubeCon Europe 2026  ",
			StartDate: "2026-03-17",
			EndDate:   "2026-03-20",
		}},
		llm,
	)

	got, err := x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
	require.NoError(t, err)
	assert.Equal(t, "KubeCon Europe 2026", got.Name, "the parsed name wins and is trimmed, not replaced")
	assert.Equal(t, []string{"2026-03-17", "2026-03-20"}, got.Dates)
	assert.Equal(t, "CNCF", got.BrandShort,
		"the brand token is the one thing the model is for: no markup parse yields it reliably")
	assert.Equal(t, 1, llm.called)
}

// TestEventIdentity_ModelFillsOnlyWhatTheParseMissed covers the fallback half. A
// page with no machine-readable name is common, and refusing it outright would
// block discovery on pages a human can read perfectly well.
func TestEventIdentity_ModelFillsOnlyWhatTheParseMissed(t *testing.T) {
	x := explorerWithNoPortal(
		&stubFetcher{body: []byte("x")},
		stubParser{details: eventurl.EventDetails{Description: "The cloud native conference"}},
		&stubLLM{reply: "{\"eventName\":\"KubeCon Europe 2026\",\"brandShort\":\"CNCF\"," +
			"\"eventDates\":[\"2026-03-17\",\"not a date\"]}"},
	)

	got, err := x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
	require.NoError(t, err)
	assert.Equal(t, "KubeCon Europe 2026", got.Name)
	assert.Equal(t, []string{"2026-03-17"}, got.Dates,
		"model dates go through the same ISO sanitizer; a half-parsed date names a real list for the wrong quarter")
}

// TestEventIdentity_DegradesRatherThanFailsOnTheModel pins that the LLM is
// optional in every direction — absent, erroring, or answering with something that
// is not JSON. Discovery's classification is deterministic, so losing the brand
// token costs a few brand-scoped suppression probes and nothing else; failing the
// whole discovery over it would be a far worse trade.
func TestEventIdentity_DegradesRatherThanFailsOnTheModel(t *testing.T) {
	details := eventurl.EventDetails{Name: "KubeCon Europe 2026", StartDate: "2026-03-17"}

	cases := map[string]completer{
		"no model at all":   nil,
		"model errored":     &stubLLM{err: errors.New("llm: 503")},
		"model wrote prose": &stubLLM{reply: "Sure! The event is KubeCon."},
		"model wrote half an object": &stubLLM{
			reply: `{"brandShort":"CNCF"`,
		},
	}
	for name, llm := range cases {
		t.Run(name, func(t *testing.T) {
			x := explorerWithNoPortal(&stubFetcher{body: []byte("x")}, stubParser{details: details}, llm)

			got, err := x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
			require.NoError(t, err, "the model is never load-bearing")
			assert.Equal(t, "KubeCon Europe 2026", got.Name)
			assert.Empty(t, got.BrandShort, "an unusable answer leaves the brand unset rather than guessed")
		})
	}
}

// TestEventIdentity_ReadsAFencedReply pins the one "not JSON" shape that is NOT a
// degrade. This model fences often, and the cost of dropping a fenced reply is
// silent: the brand token goes missing on an otherwise perfectly good answer, so
// the brand-scoped suppression probes are skipped and the operator is offered
// fewer suppression lists with nothing on screen saying why.
func TestEventIdentity_ReadsAFencedReply(t *testing.T) {
	for _, reply := range []string{
		"```json\n{\"brandShort\":\"CNCF\"}\n```",
		"```\n{\"brandShort\":\"CNCF\"}\n```",
		"  {\"brandShort\":\"CNCF\"}  ",
	} {
		x := explorerWithNoPortal(
			&stubFetcher{body: []byte("x")},
			stubParser{details: eventurl.EventDetails{Name: "KubeCon Europe 2026"}},
			&stubLLM{reply: reply},
		)

		got, err := x.eventIdentity(context.Background(), "https://events.example.org/kubecon")
		require.NoError(t, err)
		assert.Equal(t, "CNCF", got.BrandShort, "reply %q", reply)
	}
}

// TestPreviewCount_EmptySelectionNeverTouchesThePortal pins that zero is answered
// locally. It is the state the panel opens in, so resolving credentials for it
// would make an unconfigured project fail before the operator has asked for
// anything.
func TestPreviewCount_EmptySelectionNeverTouchesThePortal(t *testing.T) {
	x := explorerWithNoPortal(nil, nil, nil)

	for _, ids := range [][]string{nil, {}, {"", "   "}} {
		got, err := x.PreviewCount(context.Background(), "proj-1", ids)
		require.NoError(t, err, "an empty selection must not need a portal: %#v", ids)
		assert.True(t, got.Exact)
		assert.Zero(t, got.Count)
		assert.Empty(t, got.Reason, "exactly zero needs no caveat")
	}
}

// TestComposeMaster_RefusesBeforeItCreatesAnything pins that validation precedes
// the portal. ComposeMaster is NOT idempotent — it creates real contact lists —
// so a request that cannot produce a master must be refused before the first
// create, not after a suppression list is already sitting in the portal.
func TestComposeMaster_RefusesBeforeItCreatesAnything(t *testing.T) {
	x := explorerWithNoPortal(nil, nil, nil)

	_, err := x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{})
	require.ErrorIs(t, err, audience.ErrNoInclusionLists)
	assert.ErrorIs(t, err, audience.ErrInvalidRequest,
		"the handler maps this to a 400 through the general sentinel, so the wrapping has to hold")

	_, err = x.ComposeMaster(context.Background(), "proj-1",
		audience.ComposeInput{ListIDs: []string{"  ", ""}})
	require.ErrorIs(t, err, audience.ErrNoInclusionLists,
		"blank ids are not a selection; treating them as one would build a list matching nobody")
}

// TestComposePartialError_CarriesTheOrphanForward pins the shape the handler
// needs. The suppression list is already in the portal when the master's create
// fails, so the error has to name it — a bare failure invites a retry that creates
// a second suppression list, which is the duplicate this type exists to prevent.
func TestComposePartialError_CarriesTheOrphanForward(t *testing.T) {
	cause := errors.New("hubspot: 429 rate limited")
	err := &audience.ComposePartialError{
		Suppression: audience.ComposedList{ListRow: audience.ListRow{ListID: "555", Name: "… - Combined Suppression"}},
		Err:         cause,
	}

	assert.ErrorIs(t, err, audience.ErrComposePartial, "the handler selects on this sentinel")
	assert.ErrorIs(t, err, cause, "and the cause must stay reachable for the log")

	var partial *audience.ComposePartialError
	require.ErrorAs(t, err, &partial)
	assert.Equal(t, "555", partial.Suppression.ListID,
		"the orphan's id is what lets the UI link an operator straight to it in HubSpot")
}

// TestComposeMaster_SuppressionCreateUnconfirmed_ReturnsPartialWithNameOnly drives
// the hubspot.IsUnconfirmed branch ComposeMaster takes on the suppression create,
// through a real HTTP round trip rather than the ComposePartialError type alone
// (TestComposePartialError_CarriesTheOrphanForward covers that in isolation). A
// 2xx response with no listId is HubSpot's own signature for "may have created it,
// verify before retrying" -- CreateList surfaces that as unconfirmed, and this
// pins that ComposeMaster reports the orphan by NAME only: an unconfirmed create
// has no id to give the operator, and inventing one (or a "555" left over from a
// prior test) would send them to a list that may not exist.
func TestComposeMaster_SuppressionCreateUnconfirmed_ReturnsPartialWithNameOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		default:
			// 2xx with no listId: HubSpot may have created the list, but CreateList
			// cannot confirm it, so this is the unconfirmed case under test.
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"proj-1": activeHubSpotConn(goodHubSpotCreds),
	}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, nil, nil, nil)

	_, err := x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
		ListIDs:        []string{"111"},
		ExcludeListIDs: []string{"222"},
		Name:           "KubeCon NA 2026 — master",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, audience.ErrComposePartial)

	var partial *audience.ComposePartialError
	require.ErrorAs(t, err, &partial)
	assert.Empty(t, partial.Suppression.ListID,
		"an unconfirmed create has no id to report; the deterministic name is the only reconcile key")
	assert.NotEmpty(t, partial.Suppression.Name,
		"the name is what lets an operator search HubSpot for a list that may already exist")
}

// TestComposeMaster_MasterCreateUnconfirmed_ReturnsPartialWithMasterName mirrors
// TestComposeMaster_SuppressionCreateUnconfirmed_ReturnsPartialWithNameOnly for the
// OTHER half of compose: the master create itself is the one that comes back
// unconfirmed. Before this fix, ComposeMaster only checked hubspot.IsUnconfirmed on
// the SUPPRESSION create -- an unconfirmed MASTER create fell through to a bare
// wrapped error, silently dropping the one signal ("HubSpot may have created it
// under this name") an operator needs before deciding whether to retry.
func TestComposeMaster_MasterCreateUnconfirmed_ReturnsPartialWithMasterName(t *testing.T) {
	var suppressionCreated bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case r.URL.Path == "/crm/v3/lists" && r.Method == http.MethodPost && !suppressionCreated:
			// First create: the combined suppression list, which succeeds normally.
			suppressionCreated = true
			_, _ = io.WriteString(w, `{"list":{"listId":"999","name":"suppression","objectTypeId":"0-1","size":0}}`)
		default:
			// Second create: the master, which is unconfirmed -- a 2xx with no listId.
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"proj-1": activeHubSpotConn(goodHubSpotCreds),
	}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, nil, nil, nil)

	_, err := x.ComposeMaster(context.Background(), "proj-1", audience.ComposeInput{
		ListIDs:        []string{"111"},
		ExcludeListIDs: []string{"222"},
		Name:           "KubeCon NA 2026 — master",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, audience.ErrComposePartial)

	var partial *audience.ComposePartialError
	require.ErrorAs(t, err, &partial)
	assert.Equal(t, "KubeCon NA 2026 — master", partial.MasterName,
		"the master's deterministic name is the only reconcile key an unconfirmed create can give")
	assert.Equal(t, "999", partial.Suppression.ListID,
		"the suppression list DID confirm-create, so its real id must still be reported alongside the unconfirmed master")
}

// TestRunQA_ByName_FetchesRealFiltersRatherThanCachedSearchHit pins that QA-by-name
// always resolves the chosen candidate's filters via a real GetList call, never from
// the SearchLists hit sitting in hand. SearchLists never populates filterBranch (only
// GetList's includeFilters=true does), so seeding the filter-resolution cache straight
// from search hits would make every QA-by-name run silently audit against zero
// filters -- passing every "no suppression applied" style check for the wrong reason.
func TestRunQA_ByName_FetchesRealFiltersRatherThanCachedSearchHit(t *testing.T) {
	var getListCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		case r.URL.Path == "/crm/v3/lists/search" && r.Method == http.MethodPost:
			// A search hit, exactly as HubSpot returns one: no filterBranch at all.
			_, _ = io.WriteString(w, `{"lists":[{"listId":"111","name":"KubeCon NA 2026 - master",`+
				`"objectTypeId":"0-1"}],"hasMore":false,"offset":0}`)
		case r.URL.Path == "/crm/v3/lists/111" && r.Method == http.MethodGet:
			getListCalls++
			// The real GetList response: a suppression exclusion the search hit above
			// could never have carried.
			_, _ = io.WriteString(w, `{"list":{"listId":"111","name":"KubeCon NA 2026 - master",`+
				`"objectTypeId":"0-1","size":42,"filterBranch":{"filterBranchType":"OR",`+
				`"filterBranches":[{"filterBranchType":"AND","filters":[{"filterType":"LIST_BRANCH",`+
				`"operator":"NOT_IN_LIST","listId":222}]}]}}}`)
		case r.URL.Path == "/crm/v3/lists/222" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"list":{"listId":"222","name":"GDPR Opt-Out Suppression",`+
				`"objectTypeId":"0-1","size":5}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"proj-1": activeHubSpotConn(goodHubSpotCreds),
	}}
	builder := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))
	x := NewAudienceExplorer(builder, nil, nil, nil)

	outcome, err := x.RunQA(context.Background(), "proj-1", "KubeCon NA 2026 - master", false, false)
	require.NoError(t, err)
	require.False(t, outcome.NeedsDisambiguation)

	assert.Equal(t, 1, getListCalls,
		"the chosen candidate's filters must come from a real GetList call, not the filterless search hit")
	assert.NotEmpty(t, outcome.Checks.Suppression.Findings,
		"the excluded GDPR list only appears if the real filterBranch (not an empty cached one) was read")
}
