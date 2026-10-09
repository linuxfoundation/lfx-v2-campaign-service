// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Link-portal fallback TTLs. A token's portal never changes (a private-app token is minted
// inside one portal), so a resolved id could be kept indefinitely; the hour bound only limits
// how long a REVOKED token's entry outlives it in memory. A failed lookup is remembered for a
// minute so a HubSpot outage costs one token-info call per minute per token rather than one
// per request — every list and email response would otherwise re-ask.
const (
	linkPortalTTL        = time.Hour
	linkPortalFailureTTL = time.Minute
	linkPortalTimeout    = 5 * time.Second
)

// linkPortals is process-wide because clients are not: the dispatcher and the audience builder
// each build a fresh *Client per request, so a per-client memo would re-ask HubSpot on every
// one of them. (The email-reference resolver does not opt in: it renders no links.)
var linkPortals = &linkPortalCache{entries: map[string]linkPortalEntry{}}

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
	if !ok || !now.Before(e.expires) {
		return linkPortalEntry{}, false
	}
	return e, true
}

func (l *linkPortalCache) put(key string, e linkPortalEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
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

	lookupCtx, cancel := context.WithTimeout(ctx, linkPortalTimeout)
	defer cancel()
	portalID, err := c.AuthenticatedPortalID(lookupCtx)
	portalID = strings.TrimSpace(portalID)
	if err != nil || portalID == "" {
		linkPortals.put(key, linkPortalEntry{expires: c.now().Add(linkPortalFailureTTL)})
		// The error is typed and renders method and path only (see tokenInfoPath), so it is
		// safe to log; it carries no byte of the token or the response.
		slog.WarnContext(ctx, "hubspot connection has no portal_id and the token's portal could not be read; app links will be blank until one is configured",
			"error", err)
		return c
	}
	linkPortals.put(key, linkPortalEntry{portalID: portalID, expires: c.now().Add(linkPortalTTL)})
	return c.withPortal(portalID)
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
