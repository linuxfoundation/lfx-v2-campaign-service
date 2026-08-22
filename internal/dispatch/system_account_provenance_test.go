// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestStampProvenance_RecordsTheAccountThatServedTheCampaign covers the half of LFXV2-3050
// that lives in this package: turning the `fromSystem` flag — which resolve stamps on the
// credential and which was previously DISCARDED once the credential had been used — into a
// fact the persisted campaign row carries.
//
// The two arms are the two answers the column exists to distinguish, and they are asserted
// against a REAL resolve (not a hand-built `resolved`), so the test fails if the fallback
// stops tagging the credential as well as if stamping stops copying it.
func TestStampProvenance_RecordsTheAccountThatServedTheCampaign(t *testing.T) {
	t.Run("system fallback stamps true", func(t *testing.T) {
		// A project with NO row of its own; only the reserved system scope is populated,
		// so resolve must take the fallback.
		repo := &scopedConnReader{rows: map[string]*model.Connection{
			model.SystemProjectID: versionedConn(`{"sys":true}`, "sys-account", 1),
		}}
		src := newCredsSource(repo, &countingEncryptor{})

		res, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !res.fromSystem {
			t.Fatal("precondition failed: the fallback did not tag the credential fromSystem, " +
				"so this test would pass for the wrong reason")
		}

		c := &model.Campaign{}
		res.stampProvenance(c)

		if c.RanOnSystemAccount == nil {
			t.Fatal("RanOnSystemAccount is nil after a SYSTEM-served dispatch — the campaign " +
				"reads as \"unknown\", so LF-funded spend is invisible to attribution and the " +
				"campaign is missed when the LF credential's blast radius is computed")
		}
		if !*c.RanOnSystemAccount {
			t.Error("RanOnSystemAccount = false for a campaign created on the LF system " +
				"account: this bills LF spend to the project's own account, which is the " +
				"exact misattribution the column exists to prevent")
		}
	})

	t.Run("project's own connection stamps false", func(t *testing.T) {
		// The project HAS its own row, so resolve never consults the system scope.
		repo := &scopedConnReader{rows: map[string]*model.Connection{
			"cncf": versionedConn(`{"own":true}`, "own-account", 1),
		}}
		src := newCredsSource(repo, &countingEncryptor{})

		res, err := src.resolve(context.Background(), "cncf", model.ProviderGoogleAds)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.fromSystem {
			t.Fatal("precondition failed: a project-owned resolve is tagged fromSystem")
		}

		c := &model.Campaign{}
		res.stampProvenance(c)

		if c.RanOnSystemAccount == nil {
			t.Fatal("RanOnSystemAccount is nil after a PROJECT-served dispatch — a campaign we " +
				"positively know ran on the project's own account must say so, not read as " +
				"\"unknown\"; nil is reserved for rows that predate the column")
		}
		if *c.RanOnSystemAccount {
			t.Error("RanOnSystemAccount = true for a campaign created on the project's OWN " +
				"connection: this attributes the project's spend to the LF")
		}
	})
}

// TestStampProvenance_NilSafeOnBothSides pins that neither nil argument fabricates a fact.
//
// A dispatcher that fails before resolving has no credential to stamp with, and one that
// returns (nil, err) has no row to stamp. The deferred call in every Dispatch runs on BOTH
// of those exits, so this is a live path, not a defensive nicety: if a nil resolved wrote
// `false`, every pre-resolve failure would claim the project's own account paid.
func TestStampProvenance_NilSafeOnBothSides(t *testing.T) {
	var nilRes *resolved
	c := &model.Campaign{}
	nilRes.stampProvenance(c)
	if c.RanOnSystemAccount != nil {
		t.Errorf("a nil resolved stamped %v — it knows nothing about which account served the "+
			"campaign, and any non-nil value here is invented", *c.RanOnSystemAccount)
	}

	// And a nil campaign must not panic: the deferred stamp runs on the (nil, err) exits.
	res := &resolved{fromSystem: true}
	res.stampProvenance(nil)
}

// TestStampProvenance_CopiesRatherThanAliasing pins that each campaign gets its OWN bool.
//
// resolve returns a *resolved that the credential cache may hand to more than one caller
// (see credcache.go's note that a *resolved is copied per caller because fromSystem is
// stamped on it). Returning &r.fromSystem instead of a copy would alias every campaign
// stamped from one credential onto a single bool, so a later write through any of them
// would silently rewrite the provenance of campaigns already persisted.
func TestStampProvenance_CopiesRatherThanAliasing(t *testing.T) {
	res := &resolved{fromSystem: true}
	a, b := &model.Campaign{}, &model.Campaign{}
	res.stampProvenance(a)
	res.stampProvenance(b)

	if a.RanOnSystemAccount == b.RanOnSystemAccount {
		t.Fatal("two campaigns stamped from one credential share a *bool — a write through " +
			"one silently rewrites the other's recorded provenance")
	}
	*a.RanOnSystemAccount = false
	if b.RanOnSystemAccount == nil || !*b.RanOnSystemAccount {
		t.Error("mutating one campaign's flag changed another's")
	}
}
