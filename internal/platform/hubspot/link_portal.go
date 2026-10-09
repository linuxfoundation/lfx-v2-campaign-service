// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Link-portal fallback TTLs. A token's portal never changes (a private-app token is minted
// inside one portal), so a resolved id could be kept indefinitely; the hour bound only limits
// how long a REVOKED token's entry outlives it in memory. A DEFINITIVE failure (see
// cacheableLinkPortalFailure) is remembered for a minute so a revoked or under-scoped token
// costs one token-info call per minute rather than one per request.
const (
	linkPortalTTL        = time.Hour
	linkPortalFailureTTL = time.Minute
	linkPortalTimeout    = 5 * time.Second
	// linkPortalMaxEntries bounds the map: keys are per stored token, so a few exist in
	// practice, but every rotation adds one and an entry is otherwise only ever overwritten.
	linkPortalMaxEntries = 256
)

// linkPortals is process-wide because clients are not: the dispatcher and the audience builder
// each build a fresh *Client per request, so a per-client memo would re-ask HubSpot on every
// one of them. (The email-reference resolver does not opt in: it renders no links.)
var linkPortals = &linkPortalCache{entries: map[string]linkPortalEntry{}}

// linkPortalLookups coalesces concurrent cold misses for one token into a single token-info
// call. Without it a restart or TTL expiry fans out one lookup per in-flight request — every
// project resolving the shared LF token at once — which can draw 429s, and a transient failure
// is deliberately not cached, so the stampede would repeat.
var linkPortalLookups singleflight.Group

type linkPortalEntry struct {
	portalID string // "" records a failed lookup
	expires  time.Time
}

type linkPortalCache struct {
	mu      sync.Mutex
	entries map[string]linkPortalEntry
}

// linkPortalKey keys the cache on a digest, never the token itself, so the map holds no
// credential. baseURL is part of the key so two HubSpot endpoints (a test server and the real
// API) never share an answer for the same token.
func linkPortalKey(baseURL, token string) string {
	sum := sha256.Sum256([]byte(baseURL + "\x00" + token))
	return hex.EncodeToString(sum[:])
}

func (l *linkPortalCache) get(key string, now time.Time) (linkPortalEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return linkPortalEntry{}, false
	}
	if !now.Before(e.expires) {
		delete(l.entries, key)
		return linkPortalEntry{}, false
	}
	return e, true
}

// put stores e, first sweeping expired entries when the map is full and, if it is still
// full, dropping one live entry: a dropped entry costs one re-lookup, never a wrong answer.
func (l *linkPortalCache) put(key string, e linkPortalEntry, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.entries[key]; !exists && len(l.entries) >= linkPortalMaxEntries {
		for k, v := range l.entries {
			if !now.Before(v.expires) {
				delete(l.entries, k)
			}
		}
		for k := range l.entries {
			if len(l.entries) < linkPortalMaxEntries {
				break
			}
			delete(l.entries, k)
		}
	}
	l.entries[key] = e
}

// WithLinkPortalFallback returns a client whose app links are built from the portal its token
// authenticates into when the connection stores no portal_id.
//
// portal_id is optional connection config, and nothing that checks a connection reads it: the
// connection test answers OK with it blank (only a MISMATCH is logged), and every API call
// authenticates on the token alone. A row installed without it therefore looks healthy end to
// end while every listURL, emailEditURL and EmailDetailsURL comes back "" — and the LFX BFF
// refuses a composed or attached master list with no link, so the operator sees a 500 from an
// audience that HubSpot built correctly. That is how prod's LF system row failed on
// 2026-10-09: list search worked, UTM lookups worked, compose and attach-existing did not.
//
// The token's own portal is the right fallback, not a guess: a private-app token is minted
// inside exactly one portal, and every asset this client creates or reads lives there. A
// stored portal_id still WINS when present — an operator who configured one is never
// overridden here, and the connection test keeps reporting a configured value that disagrees
// with the token, which is the case that needs a human.
//
// Best-effort by design: a failed lookup returns the receiver unchanged, which is exactly the
// pre-fallback behaviour (blank links), and never fails the request that asked. The receiver
// is never mutated; a resolved portal is returned on a copy, so a *Client stays safe for
// concurrent use.
func (c *Client) WithLinkPortalFallback(ctx context.Context) *Client {
	if c == nil || c.account.PortalID != "" || c.creds.PrivateAppToken == "" {
		return c
	}
	key := linkPortalKey(c.baseURL, c.creds.PrivateAppToken)
	if e, ok := linkPortals.get(key, c.now()); ok {
		return c.withPortal(e.portalID)
	}

	// Detached from the caller's cancellation: the answer is shared by every caller of this
	// token, so one request that is cancelled or nearly out of budget must not decide it for
	// the others. The lookup keeps its own bound.
	v, err, _ := linkPortalLookups.Do(key, func() (any, error) {
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linkPortalTimeout)
		defer cancel()
		id, lerr := c.AuthenticatedPortalID(lookupCtx)
		id = strings.TrimSpace(id)
		switch {
		case lerr == nil && id != "":
			linkPortals.put(key, linkPortalEntry{portalID: id, expires: c.now().Add(linkPortalTTL)}, c.now())
		case cacheableLinkPortalFailure(lerr):
			linkPortals.put(key, linkPortalEntry{expires: c.now().Add(linkPortalFailureTTL)}, c.now())
		}
		return id, lerr
	})
	portalID, _ := v.(string)
	if err != nil || portalID == "" {
		// The error is typed and renders method and path only (see tokenInfoPath), so it is
		// safe to log; it carries no byte of the token or the response.
		slog.WarnContext(ctx, "hubspot connection has no portal_id and the token's portal could not be read; app links will be blank until one is configured",
			"error", err)
		return c
	}
	return c.withPortal(portalID)
}

// WithLinkPortal returns a client that builds links into portalID, for a caller that has
// already VERIFIED the token's portal (Dispatch's assertAudiencePortal) and must not pay for a
// second lookup. A stored portal_id still wins, and "" returns the receiver unchanged.
func (c *Client) WithLinkPortal(portalID string) *Client {
	if c == nil || c.account.PortalID != "" {
		return c
	}
	return c.withPortal(strings.TrimSpace(portalID))
}

// cacheableLinkPortalFailure reports whether a failed lookup is a verdict worth remembering.
//
// Only definitive answers are: a 401/403 or other 4xx (the token cannot read its own portal),
// or a response that carried no usable hubId. A transient failure — a transport error, a 429,
// a 5xx, or the lookup's own timeout — is NOT cached, because the BFF refuses a composed or
// attached master list with a blank link: caching one HubSpot blip would turn it into a
// minute of the exact 500s this fallback exists to prevent.
func cacheableLinkPortalFailure(err error) bool {
	if err == nil {
		return true // the call answered and named no portal
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var te *transportError
	if errors.As(err, &te) {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode != http.StatusTooManyRequests && ae.StatusCode < 500
	}
	var pe *preSendError
	return !errors.As(err, &pe)
}

// withPortal returns a copy of c that builds links into portalID. "" returns c itself.
func (c *Client) withPortal(portalID string) *Client {
	if portalID == "" {
		return c
	}
	cp := *c
	cp.account.PortalID = portalID
	return &cp
}
