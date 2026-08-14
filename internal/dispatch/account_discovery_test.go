// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// Both dispatchers must SATISFY the AccountLister interface, or Orchestrator.ReadAccounts
// type-asserts, misses, and answers ErrAccountsUnsupported — the endpoint exists and always
// fails. A compile-time assertion catches that at build rather than at runtime.
var (
	_ service.AccountLister = (*LinkedInDispatcher)(nil)
	_ service.AccountLister = (*MicrosoftDispatcher)(nil)
)

// The point of account discovery: it must work on a connection that has chosen NO account.
//
// The endpoint exists to answer "which ad account should this connection use?", so requiring
// one makes it reachable only by connections that no longer need it. Both dispatchers'
// credential resolvers hard-fail on a missing account id for DISPATCH — correctly — and the
// discovery paths deliberately tolerate exactly that one error and nothing else.
//
// Asserted by the error NOT being the account-not-selected sentinel. These fixtures cannot
// reach the real platform, so the call fails at the network; what matters is that it got
// that far, because a resolver still demanding an account id would have refused before
// contacting anything.
func TestListAccountsWorksWithoutASelectedAccount(t *testing.T) {
	t.Run("linkedin", func(t *testing.T) {
		conn := activeLinkedInConn(goodLinkedInCreds)
		conn.AccountID = "" // the state discovery exists to rescue
		d := NewLinkedInDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})

		_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds)
		if errors.Is(err, domain.ErrAccountNotSelected) {
			t.Fatal("discovery refused a connection with no account id — that is the exact connection it exists to serve, so it can never complete")
		}
	})

	t.Run("microsoft", func(t *testing.T) {
		conn := activeMicrosoftConn(goodMicrosoftCreds)
		conn.AccountID = ""
		d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{})

		_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderMicrosoftAds)
		if errors.Is(err, domain.ErrAccountNotSelected) {
			t.Fatal("discovery refused a connection with no account id — that is the exact connection it exists to serve, so it can never complete")
		}
	})
}

// Discovery must NOT become a way around the checks dispatch enforces. Sharing the resolver
// is what keeps the two from drifting; without it a credential rejected at dispatch could be
// accepted here, which makes a discovery endpoint actively misleading rather than merely
// permissive. Each case is a defect that is NOT "no account chosen".
func TestListAccountsStillRejectsAnUnusableConnection(t *testing.T) {
	cases := []struct {
		name string
		conn func() *model.Connection
	}{
		{"inactive connection", func() *model.Connection {
			c := activeLinkedInConn(goodLinkedInCreds)
			c.Status = model.StatusInactive
			return c
		}},
		{"undecodable credentials", func() *model.Connection {
			return activeLinkedInConn(`{not json`)
		}},
		{"incomplete credentials", func() *model.Connection {
			return activeLinkedInConn(`{"AccessToken":""}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewLinkedInDispatcher(fakeConnReader{conn: tc.conn()}, identityEncryptor{})
			_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds)
			if err == nil {
				t.Fatal("discovery accepted a connection dispatch would reject")
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error %v does not carry ErrConnectionNotUsable, so the endpoint cannot map it to the right status", err)
			}
		})
	}
}

// The label is what a picker RENDERS, so it must never be empty for an account carrying any
// identifying information — a blank row is unpickable, and the id is what actually gets
// stored. Each fallback step is exercised because each one is a different way a real
// response arrives: LinkedIn omits Name freely, and Microsoft's is nillable in its schema.
func TestAccountLabelsAreNeverEmpty(t *testing.T) {
	t.Run("linkedin falls back to the id", func(t *testing.T) {
		if got := linkedInAccountLabel(linkedin.AdAccount{ID: "507404993"}); got != "507404993" {
			t.Errorf("label = %q, want the id as a fallback", got)
		}
	})
	t.Run("linkedin appends a non-active status", func(t *testing.T) {
		got := linkedInAccountLabel(linkedin.AdAccount{ID: "507404993", Name: "LF", Status: "CANCELED"})
		if got != "LF (CANCELED)" {
			t.Errorf("label = %q, want the status appended so the user sees why it may not work", got)
		}
	})
	t.Run("linkedin appends nothing for ACTIVE", func(t *testing.T) {
		// An ACTIVE account is the normal case; labelling it would be noise on every row.
		if got := linkedInAccountLabel(linkedin.AdAccount{ID: "507404993", Name: "LF", Status: "ACTIVE"}); got != "LF" {
			t.Errorf("label = %q, want no status suffix for an active account", got)
		}
	})
	t.Run("linkedin appends nothing for an ABSENT status", func(t *testing.T) {
		// Absence is not a claim either way; labelling it would invent one.
		if got := linkedInAccountLabel(linkedin.AdAccount{ID: "507404993", Name: "LF"}); got != "LF" {
			t.Errorf("label = %q, want no suffix when the status field was absent", got)
		}
	})
}
