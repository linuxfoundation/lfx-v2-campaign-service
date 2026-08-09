// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strings"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
)

// EventFetcher retrieves the bytes of an event page.
//
// Narrow on purpose. eventurl.Fetcher is the only production implementation and it is
// the only constructor in non-test code, so nothing can reach this seam with an
// unguarded HTTP client; the interface exists so tests need no listening socket, not
// so the SSRF guard becomes swappable.
type EventFetcher interface {
	Fetch(ctx context.Context, eventURL string) ([]byte, error)
}

// EventParser extracts event metadata from a fetched page body.
type EventParser interface {
	Parse(body []byte) eventurl.EventDetails
}

// SetEventURL injects the event-page fetcher and parser.
//
// Separate from NewBriefService for the same reason as SetIndexer: the ~40 existing
// constructor call sites (nearly all tests) must keep compiling, and a BriefService
// without these still serves every other method. A handler that needs them reports
// 503 rather than nil-panicking — see FetchEventURL.
func (s *BriefService) SetEventURL(f EventFetcher, p EventParser) {
	if f == nil || p == nil {
		return
	}
	s.mu.Lock()
	s.eventFetcher = f
	s.eventParser = p
	s.mu.Unlock()
}

// eventURLDeps snapshots the event-URL collaborators under the lock, mirroring deps().
func (s *BriefService) eventURLDeps() (EventFetcher, EventParser) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.eventFetcher, s.eventParser
}

// FetchEventURL fetches an event page and returns the metadata extracted from it, for
// pre-filling a brief. It creates and persists NOTHING — the caller reviews the result
// and submits it through create-brief.
//
// It deliberately does not consult the brief repositories, so it stays available during
// the cold-start window when the database has not yet bound. Its own 503 covers only the
// case where the fetcher itself was never wired.
func (s *BriefService) FetchEventURL(ctx context.Context, p *briefs.FetchEventURLPayload) (*briefs.EventDetails, error) {
	fetcher, parser := s.eventURLDeps()
	if fetcher == nil || parser == nil {
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "event URL fetching is unavailable"}
	}
	if err := validateProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	// TrimSpace DETECTS an empty URL; it does not rewrite the value that gets fetched.
	// A URL is not whitespace-insensitive, and trimming one before handing it to the
	// fetcher would fetch a URL the caller did not ask for.
	if strings.TrimSpace(p.URL) == "" {
		return nil, &briefs.BadRequestError{Code: "400", Message: "url is required"}
	}

	body, err := fetcher.Fetch(ctx, p.URL)
	if err != nil {
		return nil, mapEventURLErr(err)
	}

	details := parser.Parse(body)
	if details.Name == "" {
		// A page with no name is a client error, not an empty success: every consumer
		// of the event_details blob (internal/dispatch/reddit.go's briefFields among
		// them) refuses a brief without eventName, so returning a nameless record here
		// only defers the failure to dispatch, with nothing in between explaining it.
		return nil, mapEventURLErr(eventurl.ErrEventDetailsEmpty)
	}
	// The page's declared canonical wins, and the fetched URL is only the fallback.
	// Callers paste links carrying tracking parameters; the canonical is the address
	// the event actually lives at, and it is what an ad should send a human to.
	if details.URL == "" {
		details.URL = p.URL
	}
	return eventDetailsResult(details), nil
}

// eventDetailsResult converts the platform record into the Goa result type.
//
// Every field but extracted_from is optional, and an ABSENT field is represented as a
// nil pointer rather than a pointer to "": the two say different things to a UI
// pre-filling a form — "the page did not say" versus "the page said nothing" — and
// only the first should leave the field free to be authored.
func eventDetailsResult(d eventurl.EventDetails) *briefs.EventDetails {
	return &briefs.EventDetails{
		EventName:     optStr(d.Name),
		Description:   optStr(d.Description),
		Location:      optStr(d.Location),
		StartDate:     optStr(d.StartDate),
		EndDate:       optStr(d.EndDate),
		Image:         optStr(d.Image),
		URL:           optStr(d.URL),
		ExtractedFrom: d.ExtractedFrom,
	}
}

// mapEventURLErr maps an eventurl sentinel onto the brief service's advertised errors.
//
// Forbidden is a 400 and NOT a 403. A 403 says the CALLER lacks permission, which sends
// an operator looking at tokens and roles; nothing about the caller is at issue here.
// The URL they supplied names an address this service will not connect to, which is a
// defect in the request — the same class as a malformed one.
//
// A fetch failure is 502 in spirit and 503 by the surface this method advertises
// (BadRequest, NotFound, Conflict, InternalServerError, ServiceUnavailable). 503 is the
// closer of the two available: it says "not right now, retry", which is true of an
// origin that timed out, and it does not blame the caller for a page that was reachable
// yesterday. The message names the upstream so the distinction is not lost.
//
// The errors are matched with errors.Is because a fetch error wraps BOTH its sentinel
// and a redacted cause (eventurl's multi-unwrap fetchError); a type switch would see
// only the concrete wrapper.
func mapEventURLErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, eventurl.ErrEventURLInvalid):
		return &briefs.BadRequestError{Code: "400", Message: "event URL is invalid; it must be an absolute http or https URL"}
	case errors.Is(err, eventurl.ErrEventURLForbidden):
		return &briefs.BadRequestError{Code: "400", Message: "event URL resolves to an address this service will not connect to"}
	case errors.Is(err, eventurl.ErrEventDetailsEmpty):
		return &briefs.BadRequestError{Code: "400", Message: "no event details could be extracted from that page; it declares no event name"}
	case errors.Is(err, eventurl.ErrEventURLFetchFailed):
		return &briefs.ConnServiceUnavailableError{Code: "503", Message: "the event page could not be fetched"}
	}
	// Default-deny on the MESSAGE, not just the status. An unrecognized error is the
	// one whose text is least vouched for, and eventurl builds its messages to be
	// URL-free precisely because they are rendered to callers and logs — a fallthrough
	// that formatted %v would undo that for exactly the errors nobody audited.
	return &briefs.InternalServerError{Code: "500", Message: "event URL processing failed"}
}
