// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
)

type fakeEventFetcher struct {
	body   []byte
	err    error
	called []string
}

func (f *fakeEventFetcher) Fetch(_ context.Context, eventURL string) ([]byte, error) {
	f.called = append(f.called, eventURL)
	return f.body, f.err
}

type fakeEventParser struct {
	details eventurl.EventDetails
	gotBody []byte
}

func (p *fakeEventParser) Parse(body []byte) eventurl.EventDetails {
	p.gotBody = body
	return p.details
}

// wiredEventService returns a BriefService with ONLY the event-URL collaborators set —
// no repositories, no orchestrator. That is the point: FetchEventURL must not consult
// them, so a service that would 503 from ready() still serves this endpoint.
func wiredEventService(f EventFetcher, p EventParser) *BriefService {
	s := NewBriefService(nil, nil, nil, nil)
	s.SetEventURL(f, p)
	return s
}

func TestFetchEventURLReturnsExtractedDetails(t *testing.T) {
	fetcher := &fakeEventFetcher{body: []byte("<html>page</html>")}
	parser := &fakeEventParser{details: eventurl.EventDetails{
		Name:          "KubeCon Europe 2026",
		Description:   "The cloud native conference",
		Location:      "Amsterdam",
		StartDate:     "March 2026",
		Image:         "https://example.org/hero.png",
		URL:           "https://example.org/kubecon",
		ExtractedFrom: "jsonld",
	}}
	s := wiredEventService(fetcher, parser)

	res, err := s.FetchEventURL(context.Background(), &briefs.FetchEventURLPayload{
		ProjectID: "cncf",
		URL:       "https://example.org/kubecon?utm_source=slack",
	})
	if err != nil {
		t.Fatalf("FetchEventURL: %v", err)
	}
	if res.ExtractedFrom != "jsonld" {
		t.Errorf("ExtractedFrom = %q, want jsonld", res.ExtractedFrom)
	}
	if res.EventName == nil || *res.EventName != "KubeCon Europe 2026" {
		t.Errorf("EventName = %v, want KubeCon Europe 2026", res.EventName)
	}
	// The page's declared canonical wins over the tracking-parameter URL that was pasted.
	if res.URL == nil || *res.URL != "https://example.org/kubecon" {
		t.Errorf("URL = %v, want the page's declared canonical", res.URL)
	}
	// EndDate was never extracted, so it must be absent rather than an empty string —
	// "the page did not say" and "the page said nothing" are different pre-fill answers.
	if res.EndDate != nil {
		t.Errorf("EndDate = %q, want nil for a field the page did not supply", *res.EndDate)
	}
	// The URL is fetched exactly as supplied: it is not whitespace- or parameter-
	// insensitive, and rewriting it would fetch a page the caller did not ask for.
	if len(fetcher.called) != 1 || fetcher.called[0] != "https://example.org/kubecon?utm_source=slack" {
		t.Errorf("fetched %v, want the URL verbatim", fetcher.called)
	}
	if string(parser.gotBody) != "<html>page</html>" {
		t.Errorf("parser got %q, want the fetched body", parser.gotBody)
	}
}

func TestFetchEventURLFallsBackToTheFetchedURL(t *testing.T) {
	parser := &fakeEventParser{details: eventurl.EventDetails{Name: "Summit", ExtractedFrom: "fallback"}}
	s := wiredEventService(&fakeEventFetcher{body: []byte("x")}, parser)

	res, err := s.FetchEventURL(context.Background(), &briefs.FetchEventURLPayload{
		ProjectID: "cncf",
		URL:       "https://example.org/summit",
	})
	if err != nil {
		t.Fatalf("FetchEventURL: %v", err)
	}
	if res.URL == nil || *res.URL != "https://example.org/summit" {
		t.Errorf("URL = %v, want the fetched URL when the page declares none", res.URL)
	}
}

func TestFetchEventURLRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		url       string
	}{
		{"empty project slug", "", "https://example.org/e"},
		{"project slug is a UUID", "6b1e9b3e-0000-0000-0000-000000000000", "https://example.org/e"},
		{"empty url", "cncf", ""},
		{"whitespace-only url", "cncf", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &fakeEventFetcher{body: []byte("x")}
			s := wiredEventService(fetcher, &fakeEventParser{details: eventurl.EventDetails{Name: "E"}})

			_, err := s.FetchEventURL(context.Background(), &briefs.FetchEventURLPayload{
				ProjectID: tc.projectID,
				URL:       tc.url,
			})
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("err = %v, want *briefs.BadRequestError", err)
			}
			// Nothing was fetched: an invalid request must not reach the network at all.
			if len(fetcher.called) != 0 {
				t.Errorf("fetched %v on an invalid request", fetcher.called)
			}
		})
	}
}

func TestFetchEventURLEmptyNameIsABadRequest(t *testing.T) {
	// A page that parses but yields no name: every consumer of the event_details blob
	// refuses a brief without eventName, so returning 200 here only defers the failure.
	s := wiredEventService(
		&fakeEventFetcher{body: []byte("<html/>")},
		&fakeEventParser{details: eventurl.EventDetails{Description: "orphan description"}},
	)

	res, err := s.FetchEventURL(context.Background(), &briefs.FetchEventURLPayload{
		ProjectID: "cncf",
		URL:       "https://example.org/e",
	})
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want *briefs.BadRequestError", err)
	}
}

func TestFetchEventURLUnwiredIsUnavailable(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)

	_, err := s.FetchEventURL(context.Background(), &briefs.FetchEventURLPayload{
		ProjectID: "cncf",
		URL:       "https://example.org/e",
	})
	var unavail *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("err = %v, want *briefs.ConnServiceUnavailableError", err)
	}
}

func TestMapEventURLErr(t *testing.T) {
	// The wrapped forms matter as much as the sentinels: eventurl returns a multi-unwrap
	// error carrying both its sentinel and a redacted cause, so a type switch would see
	// only the wrapper and every one of these would fall through to 500.
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"invalid", eventurl.ErrEventURLInvalid, "400"},
		{"invalid wrapped", fmt.Errorf("%w: malformed URL", eventurl.ErrEventURLInvalid), "400"},
		{"forbidden", eventurl.ErrEventURLForbidden, "400"},
		{"forbidden wrapped", fmt.Errorf("%w: 169.254.169.254", eventurl.ErrEventURLForbidden), "400"},
		{"details empty", eventurl.ErrEventDetailsEmpty, "400"},
		{"fetch failed", eventurl.ErrEventURLFetchFailed, "503"},
		{"fetch failed wrapped", fmt.Errorf("%w: %w", eventurl.ErrEventURLFetchFailed, io.EOF), "503"},
		{"unrecognized", errors.New("https://secret.example/?token=abc failed"), "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapEventURLErr(tc.err)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("mapEventURLErr(nil) = %v, want nil", got)
				}
				return
			}
			var code, msg string
			switch e := got.(type) {
			case *briefs.BadRequestError:
				code, msg = e.Code, e.Message
			case *briefs.ConnServiceUnavailableError:
				code, msg = e.Code, e.Message
			case *briefs.InternalServerError:
				code, msg = e.Code, e.Message
			default:
				t.Fatalf("unexpected error type %T", got)
			}
			if code != tc.want {
				t.Errorf("code = %s, want %s", code, tc.want)
			}
			// Default-deny on the MESSAGE too: the unrecognized case must not render the
			// original text, which is the one whose contents nothing vouched for.
			if tc.name == "unrecognized" && msg != "event URL processing failed" {
				t.Errorf("message = %q, want the fixed text with no detail from the cause", msg)
			}
		})
	}
}
