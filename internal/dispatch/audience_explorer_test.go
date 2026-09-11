// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/llm"
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

// A nil *llm.Client -- exactly what container.newLLMClient() returns when
// AI_PROXY_URL/AI_API_KEY are unset -- must read as "absent" at the guards inside
// Discover. Assigned straight into the completer parameter it becomes a non-nil
// interface holding a nil pointer, the `x.llm == nil` guard reads false, and
// Discover panics with a nil-receiver dereference inside llm.Client.Complete.
// Observed locally on 2026-09-11: every Discover call 500'd with
// "Audience discovery failed" on a service with no AI proxy configured.
func TestNewAudienceExplorerNormalisesTypedNilDependencies(t *testing.T) {
	var client *llm.Client
	var fetcher *eventurl.Fetcher
	var parser *eventurl.Parser

	x := explorerWithNoPortal(fetcher, parser, client)

	if x.llm != nil {
		t.Errorf("llm: want nil interface for a nil *llm.Client, got %T", x.llm)
	}
	if x.fetcher != nil {
		t.Errorf("fetcher: want nil interface for a nil *eventurl.Fetcher, got %T", x.fetcher)
	}
	if x.parser != nil {
		t.Errorf("parser: want nil interface for a nil *eventurl.Parser, got %T", x.parser)
	}
}

// Deliberately asserted at the constructor rather than through Discover: every
// fixture in this file is a project with no HubSpot connection, so Discover
// refuses at the credential seam and returns before reaching enrichIdentity. A
// Discover-level test of this panic passes with the fix reverted -- it proves
// nothing. The constructor is the narrowest place the defect is observable.

// The design's MaxLength on list_ids is a DSL literal and cannot reference
// audience.PreviewMaxLists, so the two can drift silently -- raising the Go budget
// without the design would leave the edge rejecting valid requests, and lowering it
// without the design would let the edge admit a sweep the method then refuses. This
// pins them together; if you change one, this test tells you to change the other.
func TestPreviewMaxListsMatchesTheGeneratedEdgeValidation(t *testing.T) {
	const designMaxLength = 50 // design/audience_builder.go -> list_ids MaxLength

	if audience.PreviewMaxLists != designMaxLength {
		t.Fatalf("audience.PreviewMaxLists = %d but design/audience_builder.go declares MaxLength(%d); update both and re-run `make apigen`",
			audience.PreviewMaxLists, designMaxLength)
	}
}

// Over-budget selections must be refused before any HubSpot call, and as an invalid
// request rather than a transient one -- the remedy is to select fewer lists, so a
// retry of the same payload can never succeed.
func TestPreviewCountRefusesMoreListsThanTheBudget(t *testing.T) {
	x := explorerWithNoPortal(nil, nil, nil)

	ids := make([]string, audience.PreviewMaxLists+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("list-%d", i)
	}

	_, err := x.PreviewCount(context.Background(), "tlf", ids)
	if !errors.Is(err, audience.ErrTooManyPreviewLists) {
		t.Fatalf("want ErrTooManyPreviewLists for %d lists, got %v", len(ids), err)
	}
	// Wrapping ErrInvalidRequest is what maps this to a 400 rather than a retryable 5xx.
	if !errors.Is(err, audience.ErrInvalidRequest) {
		t.Errorf("ErrTooManyPreviewLists must wrap ErrInvalidRequest so the handler maps it to 400; got %v", err)
	}
}
