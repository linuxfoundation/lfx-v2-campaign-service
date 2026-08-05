// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/snowflake"
)

// PastEditionResolver is the Snowflake capability the builder needs. An interface (rather than
// *snowflake.Client) so the builder can be constructed without a warehouse — a deployment with
// no Snowflake config still builds country-only audiences instead of failing outright.
type PastEditionResolver interface {
	ResolvePastEventNames(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]snowflake.Event, error)
}

// AudienceBuilder implements service.AudienceBuilder against the real platforms. It lives in
// this package because it needs the same per-project credential resolution the dispatchers use:
// HubSpot tokens are stored per project as encrypted connections, not injected as global config.
type AudienceBuilder struct {
	creds     *credsSource
	snowflake PastEditionResolver
	// snowErr records that Snowflake WAS configured but the client could not be constructed
	// (a malformed or rotated private key, say). It is deliberately distinct from a nil
	// resolver with no error, which means "no warehouse configured". Both leave the resolver
	// unusable, but only this one is an outage: without it a broken key looks exactly like a
	// deliberate country-only deployment, and a returning event silently loses its entire
	// past-registrant audience while every signal reports success.
	snowErr error
	opts    []hubspot.Option
}

// buildScope caches the resolved client for the duration of ONE build. It is created by
// BeginBuild and discarded when that build ends, so a credential rotated between builds is
// picked up by the next one — a builder-lifetime cache would pin a stale (or revoked)
// credential for the life of the process.
type buildScope struct {
	mu      sync.Mutex
	clients map[string]*hubspot.Client
}

// NewAudienceBuilder builds the audience builder. snow may be nil: the warehouse is used only
// to widen an audience with past editions, so a nil resolver degrades to a country-only build
// rather than blocking the email channel entirely.
func NewAudienceBuilder(repo connReader, enc domain.Encryptor, snow PastEditionResolver, opts ...hubspot.Option) *AudienceBuilder {
	return &AudienceBuilder{creds: newCredsSource(repo, enc), snowflake: snow, opts: opts}
}

// NewDegradedAudienceBuilder builds a builder whose warehouse was CONFIGURED but could not be
// constructed. Every ResolvePastEditions call then reports snowErr, so the caller takes its
// degrade branch and the stored InclusionSummary says the history could not be read.
//
// This is a separate constructor rather than a nil-resolver shortcut precisely because the two
// must not collapse into one another: NewAudienceBuilder(.., nil) still means "no warehouse
// configured", which is a legitimate deployment and gets the benign note.
func NewDegradedAudienceBuilder(repo connReader, enc domain.Encryptor, snowErr error, opts ...hubspot.Option) *AudienceBuilder {
	return &AudienceBuilder{creds: newCredsSource(repo, enc), snowErr: snowErr, opts: opts}
}

// ResolvePastEditions returns the VERBATIM names of an event's past editions.
//
// The names are used as exact HubSpot filter values, so they must come from the warehouse and
// never be guessed — a wrong name yields an empty list indistinguishable from a correct one.
// It returns an empty slice (not an error) when the event has no prior edition.
func (b *AudienceBuilder) ResolvePastEditions(ctx context.Context, eventTerm, locationTerm, currentYear string) ([]string, error) {
	if b.snowErr != nil {
		// CONFIGURED but unusable. Surfacing this as an error (rather than the nil-resolver
		// silence below) is the whole point: the caller logs the degrade and records the
		// "could NOT be resolved" note, so a rotated key is distinguishable from a first-time
		// event instead of both reporting a clean success.
		return nil, fmt.Errorf("snowflake is configured but unusable: %w", b.snowErr)
	}
	if b.snowflake == nil {
		// Not an error: the caller degrades to a country-only audience and records the gap.
		return nil, nil
	}
	// The year must come from the EVENT, not the wall clock. ResolvePastEventNames excludes
	// names containing the supplied year, so a wall-clock fallback omits the wrong edition: on
	// a 2027 brief read in 2026 it drops the 2026 edition, and on an older brief it lets that
	// brief's OWN edition through as a "past" one. Without a real year, return no editions —
	// the caller degrades to a country-only audience and records the gap.
	year := strings.TrimSpace(currentYear)
	if !isFourDigitYear(year) {
		year = yearIn(eventTerm)
	}
	if year == "" {
		return nil, nil
	}

	// Strip the year from the search term. The event name normally CONTAINS its year
	// ("KubeCon Korea 2026"), and ResolvePastEventNames does an `ILIKE '%term%'` match — so
	// passing the full name asks for rows containing "KubeCon Korea 2026", which excludes
	// every past edition (they carry a different year in their name). Sibling discovery
	// silently returned zero for every returning event.
	family := strings.TrimSpace(strings.ReplaceAll(eventTerm, year, ""))
	if family == "" {
		family = eventTerm
	}

	events, err := b.snowflake.ResolvePastEventNames(ctx, family, locationTerm, year)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(events))
	for _, e := range events {
		// Trim ONLY to test emptiness — the stored value is used VERBATIM as an exact HubSpot
		// filter, so trimming it would change the authoritative name and could match nothing.
		if strings.TrimSpace(e.EventName) != "" {
			names = append(names, e.EventName)
		}
	}
	return names, nil
}

// CreateList creates one DYNAMIC contact list in the project's HubSpot portal.
//
// The resolved client is CACHED per build (keyed by project): a build creates several lists, and
// re-resolving per call would decrypt the connection repeatedly and — if the connection were
// replaced or deactivated mid-build — scatter the lists across DIFFERENT portals, leaving a
// master list pointing at ids that do not all exist in one place.
func (b *AudienceBuilder) CreateList(ctx context.Context, projectID, name string, filter json.RawMessage) (string, error) {
	client, err := b.cachedClient(ctx, projectID)
	if err != nil {
		return "", err
	}
	l, cerr := client.CreateList(ctx, name, filter)
	if cerr != nil {
		// Pass the error through unwrapped so an UNCONFIRMED create (a 2xx with no parseable
		// list id) keeps its "verify before retrying" classification instead of being
		// flattened into a generic failure.
		return "", cerr
	}
	if l == nil {
		return "", fmt.Errorf("hubspot: create list %q returned no list", name)
	}
	return l.ListID, nil
}

// scopeKey carries the per-build client cache. Using the context rather than builder state
// keeps the cache's lifetime tied to the build that created it, and keeps concurrent builds
// from sharing (or invalidating) each other's clients.
type scopeKey struct{}

// BeginBuild returns a context scoped to one audience build. All CreateList calls made with it
// share ONE resolved client per project — a build creates several lists and they must all land
// in the same portal, or the master list references ids that do not all exist together.
//
// Outside a build scope each call resolves its own client, which is correct-but-slower rather
// than wrong.
func (b *AudienceBuilder) BeginBuild(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &buildScope{})
}

// cachedClient returns the client for a project, resolving it at most once per BUILD.
func (b *AudienceBuilder) cachedClient(ctx context.Context, projectID string) (*hubspot.Client, error) {
	scope, ok := ctx.Value(scopeKey{}).(*buildScope)
	if !ok {
		// No build scope: resolve fresh. Never reuse across builds.
		return b.client(ctx, projectID)
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if c, cached := scope.clients[projectID]; cached {
		return c, nil
	}
	c, err := b.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if scope.clients == nil {
		scope.clients = map[string]*hubspot.Client{}
	}
	scope.clients[projectID] = c
	return c, nil
}

// client resolves the project's HubSpot connection and builds a client from it, mirroring
// HubSpotDispatcher.Dispatch — the credentials live per project as encrypted connections.
func (b *AudienceBuilder) client(ctx context.Context, projectID string) (*hubspot.Client, error) {
	if strings.TrimSpace(projectID) == "" {
		// Fail loudly: without a project there is no connection to resolve, and silently
		// picking one would build the audience in the wrong portal.
		return nil, fmt.Errorf("audience build: a project id is required to resolve hubspot credentials")
	}
	res, rerr := b.creds.resolve(ctx, projectID, model.ProviderHubSpot)
	if rerr != nil {
		return nil, rerr
	}
	if res.status != model.StatusActive {
		return nil, fmt.Errorf("hubspot connection for project %s is %s, not active", projectID, res.status)
	}
	var creds hubspotCreds
	if uerr := json.Unmarshal(res.plaintext, &creds); uerr != nil {
		return nil, fmt.Errorf("decode hubspot credentials: %w", uerr)
	}
	if strings.TrimSpace(creds.PrivateAppToken) == "" {
		return nil, fmt.Errorf("hubspot credentials are incomplete (need privateAppToken)")
	}
	return hubspot.NewClient(
		hubspot.Credentials{PrivateAppToken: creds.PrivateAppToken},
		hubspot.AccountConfig{PortalID: res.providerConfig["portal_id"]},
		b.opts...,
	), nil
}

// yearIn extracts a 4-digit year (19xx/20xx) from an event name, so a brief whose details omit
// the year can still derive it from the name it already carries.
func yearIn(s string) string {
	for i := 0; i+4 <= len(s); i++ {
		c := s[i : i+4]
		if isFourDigitYear(c) && (c[0] == '1' || c[0] == '2') {
			// Reject a longer digit run (e.g. a 6-digit id) that merely contains 4 digits.
			if (i == 0 || s[i-1] < '0' || s[i-1] > '9') && (i+4 == len(s) || s[i+4] < '0' || s[i+4] > '9') {
				return c
			}
		}
	}
	return ""
}

// isFourDigitYear mirrors the warehouse client's own guard so the fallback above produces a
// value it will accept.
func isFourDigitYear(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
