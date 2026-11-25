// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// phraseSearcherMock is an EmailSearcher that can ALSO search by phrase, recording which of the
// two the handler called.
type phraseSearcherMock struct {
	mockEmailSearcherDispatcher
	gotPhrases   []string
	phraseCalled bool
	phraseErr    error
}

func (m *phraseSearcherMock) SearchEmailsAnyPhrase(_ context.Context, _ string, platform model.Provider, phrases []string) ([]model.MarketingEmail, error) {
	m.phraseCalled = true
	m.gotPlatform = platform
	m.gotPhrases = phrases
	if m.phraseErr != nil {
		return nil, m.phraseErr
	}
	return m.emails, nil
}

func TestListHubspotEmails_MatchSearchesByPhrase(t *testing.T) {
	searcher := &phraseSearcherMock{mockEmailSearcherDispatcher: mockEmailSearcherDispatcher{
		emails: []model.MarketingEmail{{ID: "1", Name: "KubeCon + CloudNativeCon NA 2026 | Registration", State: "PUBLISHED"}},
	}}
	result, err := newEmailSvc(searcher).ListHubspotEmails(context.Background(),
		&conn.ListHubspotEmailsPayload{ProjectID: "p", Match: []string{"KubeCon", "CloudNativeCon"}})
	if err != nil {
		t.Fatalf("ListHubspotEmails: %v", err)
	}
	if !searcher.phraseCalled {
		t.Fatal("match must run the phrase search, not the substring search")
	}
	if len(searcher.gotPhrases) != 2 || searcher.gotPhrases[1] != "CloudNativeCon" {
		t.Errorf("phrases = %v, want them forwarded verbatim", searcher.gotPhrases)
	}
	if len(result.Emails) != 1 || result.Emails[0].ID != "1" {
		t.Errorf("emails = %+v", result.Emails)
	}
}

// q and match are different questions; combining them would make an empty answer ambiguous.
func TestListHubspotEmails_MatchAndQueryAreExclusive(t *testing.T) {
	searcher := &phraseSearcherMock{}
	q := "registration"
	_, err := newEmailSvc(searcher).ListHubspotEmails(context.Background(),
		&conn.ListHubspotEmailsPayload{ProjectID: "p", Q: &q, Match: []string{"KubeCon"}})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a 400", err)
	}
	if searcher.phraseCalled || searcher.gotQuery != "" {
		t.Error("a refused request must not reach the portal")
	}
}

// Phrases of punctuation only would otherwise reach the walk as no filter at all -- the
// unfiltered listing presented as a brand-scoped one.
func TestListHubspotEmails_MatchWithNothingToMatchIsRefused(t *testing.T) {
	searcher := &phraseSearcherMock{}
	_, err := newEmailSvc(searcher).ListHubspotEmails(context.Background(),
		&conn.ListHubspotEmailsPayload{ProjectID: "p", Match: []string{"+", " - "}})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a 400", err)
	}
	if searcher.phraseCalled {
		t.Error("a refused request must not reach the portal")
	}
}

// A dispatcher without the phrase capability is the contract's "not supported" 400, exactly as
// a dispatcher without email search is.
func TestListHubspotEmails_MatchUnsupportedIs400(t *testing.T) {
	_, err := newEmailSvc(&mockEmailSearcherDispatcher{emails: []model.MarketingEmail{}}).ListHubspotEmails(context.Background(),
		&conn.ListHubspotEmailsPayload{ProjectID: "p", Match: []string{"KubeCon"}})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want a 400", err)
	}
}
