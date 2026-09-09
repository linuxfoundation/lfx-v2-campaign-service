// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/snowflake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingResolver captures the arguments the builder sends to the warehouse.
type recordingResolver struct {
	term, location, year string
	events               []snowflake.Event
}

func (r *recordingResolver) ResolvePastEventNames(_ context.Context, eventTerm, locationTerm, currentYear string) ([]snowflake.Event, error) {
	r.term, r.location, r.year = eventTerm, locationTerm, currentYear
	return r.events, nil
}

// TestResolvePastEditions_StripsTheYearFromTheSearchTerm pins the fix for a query that could
// never match. Event names normally CONTAIN their year ("KubeCon Korea 2026"), and the warehouse
// query does an `ILIKE '%term%'` match — so passing the full name asks for rows containing
// "KubeCon Korea 2026", which excludes every past edition (they carry a different year in their
// name). Searching on the year-stripped family name instead finds sibling editions across years;
// numeric year exclusion is then applied in Go over the returned rows, not baked into the SQL.
// Sibling discovery silently returned zero for every returning event before this fix, degrading
// each one to a country-only audience.
func TestResolvePastEditions_StripsTheYearFromTheSearchTerm(t *testing.T) {
	r := &recordingResolver{}
	b := NewAudienceBuilder(nil, nil, r)

	_, err := b.ResolvePastEditions(context.Background(), "KubeCon Korea 2026", "Korea", "2026")
	require.NoError(t, err)

	assert.NotContains(t, r.term, "2026",
		"the search term must not contain the year it also excludes, or the query matches nothing")
	assert.Equal(t, "KubeCon Korea", r.term)
	assert.Equal(t, "2026", r.year)
}

// TestResolvePastEditions_DerivesTheYearFromTheEventName covers a brief whose details omit the
// year: it is recoverable from the name the brief already carries.
func TestResolvePastEditions_DerivesTheYearFromTheEventName(t *testing.T) {
	r := &recordingResolver{}
	b := NewAudienceBuilder(nil, nil, r)

	_, err := b.ResolvePastEditions(context.Background(), "KubeCon Korea 2026", "Korea", "")
	require.NoError(t, err)
	assert.Equal(t, "2026", r.year, "the year must come from the event, not the wall clock")
	assert.Equal(t, "KubeCon Korea", r.term)
}

// TestResolvePastEditions_NoYearMeansNoEditions pins the degrade. The warehouse query EXCLUDES
// names containing the supplied year, so a wall-clock fallback drops the wrong edition — on a
// 2027 brief read in 2026 it omits the 2026 edition, and on an older brief it lets that brief's
// OWN edition through as "past". Without a real year, return nothing and let the caller record
// a country-only audience.
func TestResolvePastEditions_NoYearMeansNoEditions(t *testing.T) {
	r := &recordingResolver{events: []snowflake.Event{{EventName: "should not be reached"}}}
	b := NewAudienceBuilder(nil, nil, r)

	names, err := b.ResolvePastEditions(context.Background(), "Some Event", "Korea", "")
	require.NoError(t, err, "a missing year degrades, it does not fail")
	assert.Empty(t, names)
	assert.Empty(t, r.year, "the warehouse must not be queried with a guessed year")
}

// TestResolvePastEditions_PreservesWarehouseNamesVerbatim pins that the authoritative name is
// NOT normalized. It is used as an EXACT HubSpot filter value, so trimming it would silently
// change the name and could match nothing.
func TestResolvePastEditions_PreservesWarehouseNamesVerbatim(t *testing.T) {
	r := &recordingResolver{events: []snowflake.Event{
		{EventName: "  KubeCon Korea 2025  "},
		{EventName: "   "}, // whitespace-only: dropped, not trimmed into existence
		{EventName: "KubeCon Korea 2024"},
	}}
	b := NewAudienceBuilder(nil, nil, r)

	names, err := b.ResolvePastEditions(context.Background(), "KubeCon Korea 2026", "", "2026")
	require.NoError(t, err)
	assert.Equal(t, []string{"  KubeCon Korea 2025  ", "KubeCon Korea 2024"}, names,
		"warehouse names must survive verbatim, including surrounding whitespace")
}

func TestYearIn(t *testing.T) {
	cases := map[string]string{
		"KubeCon Korea 2026": "2026",
		"2024 Open Summit":   "2024",
		"No year here":       "",
		"Event 123456":       "", // a longer digit run is not a year
		"Event 999":          "",
		// Four digits outside 19xx/20xx are not years. A first-digit-only check would
		// extract "1000"/"2999" here and hand them to the warehouse client, which rejects
		// them — so the two predicates have to agree on the same range.
		"Event 9999":      "",
		"Event 1899":      "",
		"Event 2100":      "",
		"Event 1900 Expo": "1900",
		"Event 2099 Expo": "2099",
	}
	for in, want := range cases {
		assert.Equal(t, want, yearIn(in), "input %q", in)
	}
}

// TestResolvePastEditions_OutOfRangeYearDoesNotReachTheResolver pins the dispatch copy of the
// range guard, which is independent of the warehouse client's and of the service's — drift in
// any one of the three would leave the other two's tests green.
//
// An out-of-range currentYear must not be forwarded. Above the range the warehouse comparison
// inverts ("past editions only" returns future ones); below it every edition is excluded. Here
// the guard also gates the FALLBACK: an unusable currentYear falls through to yearIn(eventTerm),
// and when the name carries no usable year either, the builder degrades to no editions rather
// than querying with a year it cannot trust.
func TestResolvePastEditions_OutOfRangeYearDoesNotReachTheResolver(t *testing.T) {
	for _, bad := range []string{"9999", "3000", "0202", "1899", "2100"} {
		r := &recordingResolver{events: []snowflake.Event{{EventName: "should not be reached"}}}
		b := NewAudienceBuilder(nil, nil, r)

		names, err := b.ResolvePastEditions(context.Background(), "Some Event", "Korea", bad)
		require.NoError(t, err, "an unusable year degrades, it does not fail")
		assert.Empty(t, names, "currentYear %q must not produce editions", bad)
		assert.Empty(t, r.year, "currentYear %q must never reach the warehouse", bad)
	}
	// The boundaries INSIDE the range are still forwarded, so this is a range check and not a
	// blanket reject.
	for _, ok := range []string{"1900", "2026", "2099"} {
		r := &recordingResolver{}
		b := NewAudienceBuilder(nil, nil, r)

		_, err := b.ResolvePastEditions(context.Background(), "Some Event", "Korea", ok)
		require.NoError(t, err)
		assert.Equal(t, ok, r.year, "currentYear %q must be forwarded", ok)
	}
}

// TestBeginBuild_ScopesTheClientCacheToOneBuild pins the cache LIFETIME. The builder is a
// container singleton, so caching on it would pin a credential for the life of the process — a
// HubSpot connection that is rotated, revoked or deactivated would keep being used by every
// later build, and a deleted connection would still "work".
//
// Scoping to the context means: shared within one build (all its lists land in one portal), and
// re-resolved on the next.
func TestBeginBuild_ScopesTheClientCacheToOneBuild(t *testing.T) {
	b := NewAudienceBuilder(nil, nil, nil)

	c1 := b.BeginBuild(context.Background())
	c2 := b.BeginBuild(context.Background())

	s1, ok1 := c1.Value(scopeKey{}).(*buildScope)
	s2, ok2 := c2.Value(scopeKey{}).(*buildScope)
	require.True(t, ok1)
	require.True(t, ok2)
	assert.NotSame(t, s1, s2, "each build must get its OWN scope, or a rotated credential is pinned")

	// A context with no scope must not share one either.
	_, ok := context.Background().Value(scopeKey{}).(*buildScope)
	assert.False(t, ok, "an unscoped context resolves fresh rather than reusing a cached client")
}

// TestBuiltInPortalID_SharesTheBuildScopedClientWithCreateList pins the adapter behaviour the
// service-level tests structurally cannot see: they inject fakeBuilder, so nothing there exercises
// the real method, and TestBeginBuild_ScopesTheClientCacheToOneBuild only inspects context values
// without ever resolving a client.
//
// The invariant is that the portal reported and the lists created come from the SAME client. Both
// go through cachedClient, so within one BeginBuild scope the connection is read once. A regression
// to a fresh client in BuiltInPortalID would compile, pass every existing test, and produce the one
// outcome the column exists to prevent: a connection rotated mid-build stamps the NEW portal onto
// lists that live in the OLD one, and the dispatch guard then compares against a portal the ids were
// never in -- worse than not stamping at all, because it looks verified.
//
// Proven through the repository's call log rather than by reaching into the cache, so the test
// still holds if the caching mechanism is rewritten. Both halves matter: one read within a build,
// and a fresh read for the NEXT build, since a cache that never expired would pin a revoked
// credential across builds.
func TestBuiltInPortalID_SharesTheBuildScopedClientWithCreateList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case hubSpotTokenInfoPath:
			_, _ = io.WriteString(w, `{"hubId":8112310}`)
		default:
			_, _ = io.WriteString(w, `{"listId":"30967"}`)
		}
	}))
	defer srv.Close()

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf": activeHubSpotConn(goodHubSpotCreds),
	}}
	b := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))

	// ONE build: resolve the portal, then create a list, in the order BuildAudience uses.
	buildCtx := b.BeginBuild(context.Background())
	portal, err := b.BuiltInPortalID(buildCtx, "cncf")
	require.NoError(t, err)
	require.Equal(t, "8112310", portal)

	_, cerr := b.CreateList(buildCtx, "cncf", "KubeCon NA 2026 — master", json.RawMessage(`{}`))
	require.NoError(t, cerr)

	require.Len(t, repo.gets, 1,
		"the portal lookup and the list creation must share one build-scoped client; a second "+
			"resolution here means a credential rotated mid-build can stamp a portal the list ids "+
			"are not in, which the dispatch guard would then read as verified")

	// A SEPARATE build must resolve again, or a revoked credential is pinned for the lifetime of
	// the container-singleton builder.
	nextCtx := b.BeginBuild(context.Background())
	_, nerr := b.BuiltInPortalID(nextCtx, "cncf")
	require.NoError(t, nerr)
	require.Len(t, repo.gets, 2,
		"each build must re-resolve, or a rotated or revoked credential stays live across builds")
}

// TestBuiltInPortalID_LatePermissionFailureCarriesTheOrigin pins that a token the RESOLUTION
// accepted and the platform then rejected is still attributed to the row it came from.
//
// cachedClient's systemScoped covers construction only: it tags what resolution itself can see. A
// revoked or under-scoped token builds a client without complaint and is refused on the first real
// call -- and that call was wrapped bare, so it reached audienceBuildErr with neither
// ErrConnectionNotUsable nor an origin and fell through to the generic "retry once portal identity
// is readable" 500.
//
// The consequence is specific: when the SHARED LF token is the revoked one, every foundation
// without its own connection is told to retry a fault only an operator can fix, and nothing pages
// that operator. Both rows are asserted, because tagging everything system-owned would blame the LF
// row for a project's own bad token -- the same misattribution pointing the other way.
func TestBuiltInPortalID_LatePermissionFailureCarriesTheOrigin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"insufficient scope"}`)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name       string
		owner      string
		wantSystem bool
	}{
		{name: "the shared LF token is an operator fault", owner: model.SystemProjectID, wantSystem: true},
		{name: "the project's own token stays the project's", owner: "cncf", wantSystem: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &scopedConnReader{rows: map[string]*model.Connection{
				tc.owner: activeHubSpotConn(goodHubSpotCreds),
			}}
			b := NewAudienceBuilder(repo, identityEncryptor{}, nil, hubspot.WithBaseURL(srv.URL))

			_, err := b.BuiltInPortalID(b.BeginBuild(context.Background()), "cncf")
			require.Error(t, err, "a 403 on the portal lookup must not be reported as an empty portal")

			require.True(t, errors.Is(err, domain.ErrConnectionNotUsable),
				"an untagged permission failure reaches audienceBuildErr's generic arm and is "+
					"reported as a transient outage worth retrying")
			require.Equal(t, tc.wantSystem, errors.Is(err, domain.ErrSystemConnectionNotUsable),
				"whose token was refused decides who can repair it, and the message follows the tag")
		})
	}
}
