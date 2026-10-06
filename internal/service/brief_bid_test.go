// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// bidWriterDispatcher is a dispatcher implementing ONLY BidWriter. gotChange records what reached
// the dispatcher, so a test can assert the service forwarded the caller's bid unaltered.
type bidWriterDispatcher struct {
	err       error
	calls     int
	gotChange model.BidChange
}

func (d *bidWriterDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *bidWriterDispatcher) WriteBid(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, bid model.BidChange) error {
	d.calls++
	d.gotChange = bid
	return d.err
}

// bidCampaign is the ordinary subject: a provisioned, toggleable Microsoft campaign at version 3.
func bidCampaign() *model.Campaign {
	c := budgetCampaign()
	c.Platform = model.ProviderMicrosoftAds
	return c
}

func bidPayload(amount float64, bidType string, ifMatch string) *briefs.UpdateCampaignBidPayload {
	im := ifMatch
	return &briefs.UpdateCampaignBidPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch: &im, Bid: amount, BidType: bidType,
	}
}

func TestUpdateCampaignBid_HappyPathPersistsBidAndLeavesEverythingElseAlone(t *testing.T) {
	d := &bidWriterDispatcher{}
	camp := bidCampaign()
	budget := 900.0
	camp.BudgetAmount = &budget
	s, camps := budgetService(t, camp, d)

	ctx := ctxWithActor(&model.Actor{Username: "sinde"})
	res, err := s.UpdateCampaignBid(ctx, bidPayload(2.35, "cpc", "3"))
	if err != nil {
		t.Fatalf("UpdateCampaignBid: %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("WriteBid called %d times, want exactly 1", d.calls)
	}
	if d.gotChange.Amount != 2.35 || d.gotChange.Type != model.BidTypeCPC {
		t.Errorf("dispatcher got %+v, want {2.35 cpc}", d.gotChange)
	}
	if camps.got == nil {
		t.Fatal("ReplaceCampaign was never called on the happy path")
	}
	if camps.got.MaxCPCBid == nil || *camps.got.MaxCPCBid != 2.35 {
		t.Errorf("persisted MaxCPCBid = %v, want 2.35", camps.got.MaxCPCBid)
	}
	// A bid change must not touch the budget or the status columns.
	if camps.got.BudgetAmount == nil || *camps.got.BudgetAmount != 900 {
		t.Errorf("budget was modified by a bid change: %v", camps.got.BudgetAmount)
	}
	if camps.got.Status != model.CampaignStatusCreated {
		t.Errorf("status was modified by a bid change: %q", camps.got.Status)
	}
	if camps.got.UpdatedBy == nil || camps.got.UpdatedBy.Username != "sinde" {
		t.Errorf("UpdatedBy = %+v, want the requesting actor", camps.got.UpdatedBy)
	}
	if res.Version != 4 {
		t.Errorf("result version = %d, want the bumped 4", res.Version)
	}
	if len(camps.indexPayloads) != 1 {
		t.Errorf("bid change must co-commit exactly one index event, got %d", len(camps.indexPayloads))
	}
	if len(camps.cooldowns) != 0 {
		t.Errorf("a confirmed change must not hold the lock for a cooldown, got %v", camps.cooldowns)
	}
}

// bid_type is optional with a design default of "cpc"; a direct caller leaving it empty gets the
// default rather than a 400.
func TestUpdateCampaignBid_EmptyBidTypeDefaultsToCPC(t *testing.T) {
	d := &bidWriterDispatcher{}
	s, _ := budgetService(t, bidCampaign(), d)
	if _, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "", "3")); err != nil {
		t.Fatalf("UpdateCampaignBid: %v", err)
	}
	if d.gotChange.Type != model.BidTypeCPC {
		t.Errorf("dispatcher got type %q, want cpc", d.gotChange.Type)
	}
}

func TestUpdateCampaignBid_CreatedDegradedIsAllowedAndKeepsItsMarker(t *testing.T) {
	camp := bidCampaign()
	camp.Status = model.CampaignStatusCreatedDegraded
	s, camps := budgetService(t, camp, &bidWriterDispatcher{})
	if _, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3")); err != nil {
		t.Fatalf("a created_degraded campaign must accept a bid change: %v", err)
	}
	if camps.got.Status != model.CampaignStatusCreatedDegraded {
		t.Errorf("the reconciliation marker was lost: status = %q", camps.got.Status)
	}
}

// Request validation runs BEFORE the row is loaded, the claim is taken, or the platform is
// contacted.
func TestUpdateCampaignBid_RejectsBadRequests(t *testing.T) {
	cases := []struct {
		name    string
		amount  float64
		bidType string
	}{
		{"NaN", math.NaN(), "cpc"},
		{"positive infinity", math.Inf(1), "cpc"},
		{"negative infinity", math.Inf(-1), "cpc"},
		{"zero", 0, "cpc"},
		{"negative", -1, "cpc"},
		{"over the maximum", maxCampaignBid + 1, "cpc"},
		{"rounds to zero micros", 0.0000001, "cpc"},
		{"unknown bid_type", 1, "cpm"},
		{"uppercase bid_type", 1, "CPC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &bidWriterDispatcher{}
			s, camps := budgetService(t, bidCampaign(), d)
			_, err := s.UpdateCampaignBid(context.Background(), bidPayload(tc.amount, tc.bidType, "3"))
			if !isBadRequest(err) {
				t.Fatalf("expected 400 BadRequestError, got %T: %v", err, err)
			}
			if d.calls != 0 || camps.claims != 0 || camps.got != nil {
				t.Errorf("an invalid request reached the platform (%d), the claim (%d) or the row (%v)", d.calls, camps.claims, camps.got != nil)
			}
		})
	}
}

func TestUpdateCampaignBid_BoundsAreInclusive(t *testing.T) {
	for _, amount := range []float64{maxCampaignBid, 0.000001} {
		d := &bidWriterDispatcher{}
		s, _ := budgetService(t, bidCampaign(), d)
		if _, err := s.UpdateCampaignBid(context.Background(), bidPayload(amount, "cpc", "3")); err != nil {
			t.Fatalf("bound %v must be accepted: %v", amount, err)
		}
		if d.calls != 1 {
			t.Errorf("WriteBid calls = %d, want 1", d.calls)
		}
	}
}

func TestUpdateCampaignBid_MissingIfMatchIsPreconditionRequired(t *testing.T) {
	d := &bidWriterDispatcher{}
	s, _ := budgetService(t, bidCampaign(), d)
	_, err := s.UpdateCampaignBid(context.Background(), &briefs.UpdateCampaignBidPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Bid: 1, BidType: "cpc",
	})
	var required *briefs.PreconditionRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("expected 428 PreconditionRequiredError, got %T: %v", err, err)
	}
	if d.calls != 0 {
		t.Error("the platform must not be contacted without an If-Match")
	}
}

func TestUpdateCampaignBid_StaleIfMatchIsPreconditionFailed(t *testing.T) {
	camp := bidCampaign()
	camp.Version = 7
	camp.Status = model.CampaignStatusPending
	d := &bidWriterDispatcher{}
	s, camps := budgetService(t, camp, d)
	_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
	var preFailed *briefs.PreconditionFailedError
	if !errors.As(err, &preFailed) {
		t.Fatalf("expected 412 PreconditionFailedError, got %T: %v", err, err)
	}
	if d.calls != 0 || camps.claims != 0 {
		t.Error("a stale If-Match must reach neither the claim nor the platform")
	}
}

func TestUpdateCampaignBid_RefusedStatesBeforeTheClaim(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Campaign)
		want   func(error) bool
	}{
		{"still provisioning", func(c *model.Campaign) { c.Status = model.CampaignStatusPending }, isConflict},
		{"email channel", func(c *model.Campaign) { c.Platform = model.ProviderHubSpot }, isBadRequest},
		{"no upstream id", func(c *model.Campaign) { c.PlatformCampaignID = "" }, isConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			camp := bidCampaign()
			tc.mutate(camp)
			d := &bidWriterDispatcher{}
			s, camps := budgetService(t, camp, d)
			_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
			if !tc.want(err) {
				t.Fatalf("unexpected error %T: %v", err, err)
			}
			if d.calls != 0 || camps.claims != 0 {
				t.Errorf("a refused state reached the claim (%d) or the platform (%d)", camps.claims, d.calls)
			}
		})
	}
}

// Google Ads and LinkedIn have no BidWriter today. A registered dispatcher without the
// capability is what exercises the type assertion; each platform's real dispatcher is held to it
// by the dispatch package's compile-time checks.
func TestUpdateCampaignBid_UnsupportedPlatformIsBadRequest(t *testing.T) {
	for _, p := range []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds} {
		t.Run(string(p), func(t *testing.T) {
			camp := bidCampaign()
			camp.Platform = p
			s, camps := budgetService(t, camp, plainDispatcher{})
			_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
			var badReq *briefs.BadRequestError
			if !errors.As(err, &badReq) {
				t.Fatalf("expected 400, got %T: %v", err, err)
			}
			if !strings.Contains(badReq.Message, "not supported") {
				t.Errorf("message = %q", badReq.Message)
			}
			if camps.got != nil {
				t.Error("the row must not be written when the platform cannot change bids")
			}
		})
	}
}

func TestUpdateCampaignBid_DispatcherSentinelsMapToStatuses(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		wantAs func(error) bool
	}{
		{"unsupported", ErrBidUnsupported, isBadRequest},
		{"not provisioned", ErrCampaignNotProvisioned, isConflict},
		{"automated strategy", ErrBidUnwritable, isConflict},
		{"rejected amount", ErrBidAmountRejected, isBadRequest},
		{"platform campaign absent", domain.ErrPlatformCampaignAbsent, isNotFound},
		{"unknown provenance", domain.ErrCampaignProvenanceUnknown, isConflict},
		{"account mismatch", ErrCampaignAccountMismatch, isConflict},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, isInternal},
		{"system connection missing", domain.ErrSystemConnectionMissing, isInternal},
		{"credential decryption failed", domain.ErrCredentialDecryptionFailed, isInternal},
		{"service defect", fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrServiceDefect), isInternal},
		{"no ad account selected", fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), isConflict},
		{"project connection unusable", domain.ErrConnectionNotUsable, isConflict},
		{"no connection at all", domain.ErrNotFound, isNotFound},
		{"definite platform failure", errors.New("microsoft returned 400"), isUnavailable},
		// The budget sentinels are NOT this endpoint's: a budget refusal leaking here would be a
		// defect, and it must not be dressed up as a bid refusal either — it is a definite failure.
		{"budget sentinel is not a bid refusal", ErrBudgetUnwritable, isUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &bidWriterDispatcher{err: tc.err}
			s, camps := budgetService(t, bidCampaign(), d)
			_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
			if err == nil || !tc.wantAs(err) {
				t.Fatalf("unexpected error type %T: %v", err, err)
			}
			if camps.got != nil {
				t.Error("the row must not be written when the platform change did not happen")
			}
			if len(camps.cooldowns) != 0 {
				t.Errorf("a settled failure must not hold the lock for a cooldown, got %v", camps.cooldowns)
			}
			if camps.releases != 1 {
				t.Errorf("the claim lock was released %d times, want exactly 1", camps.releases)
			}
		})
	}
}

// reasonedBidAmountError mirrors the dispatch package's rejectedBidAmountError.
type reasonedBidAmountError struct {
	reason string
	err    error
}

func (e *reasonedBidAmountError) Error() string           { return e.err.Error() }
func (e *reasonedBidAmountError) Unwrap() error           { return e.err }
func (e *reasonedBidAmountError) BidAmountReason() string { return e.reason }

func TestUpdateCampaignBid_RejectedAmountCarriesTheAdapterReasonOnly(t *testing.T) {
	reason := "Microsoft Advertising refused a max CPC bid of 0.02 as below the minimum bid for this ad account's currency"
	d := &bidWriterDispatcher{err: &reasonedBidAmountError{
		reason: reason,
		err:    fmt.Errorf("write microsoft campaign bid: %s: %w", reason, ErrBidAmountRejected),
	}}
	s, camps := budgetService(t, bidCampaign(), d)
	_, err := s.UpdateCampaignBid(context.Background(), bidPayload(0.02, "cpc", "3"))
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("want a 400, got %T: %v", err, err)
	}
	if !strings.Contains(bad.Message, reason) {
		t.Errorf("the 400 must carry the adapter's reason, got %q", bad.Message)
	}
	if strings.Contains(bad.Message, "write microsoft campaign bid") {
		t.Errorf("the 400 must not render the error chain, got %q", bad.Message)
	}
	if camps.got != nil {
		t.Error("the row must not be written when the amount was refused")
	}
}

// The 409 for an automated strategy names the remedy and no upstream ids.
func TestUpdateCampaignBid_UnwritableMessageNamesTheRemedyOnly(t *testing.T) {
	d := &bidWriterDispatcher{err: fmt.Errorf("campaign 555 uses the automated MaxConversions strategy: %w", ErrBidUnwritable)}
	s, _ := budgetService(t, bidCampaign(), d)
	_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected 409, got %T: %v", err, err)
	}
	if !strings.Contains(conflict.Message, "never changes the bid strategy") {
		t.Errorf("the 409 must say the strategy is never switched, got %q", conflict.Message)
	}
	if strings.Contains(conflict.Message, "555") || strings.Contains(conflict.Message, "MaxConversions") {
		t.Errorf("the 409 must not carry upstream detail, got %q", conflict.Message)
	}
}

func TestUpdateCampaignBid_AbsentProvenanceOutranksAccountMismatch(t *testing.T) {
	d := &bidWriterDispatcher{err: errors.Join(domain.ErrCampaignProvenanceUnknown, ErrCampaignAccountMismatch)}
	s, _ := budgetService(t, bidCampaign(), d)
	_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) || !contains(conflict.Message, "re-dispatched") {
		t.Fatalf("an absent provenance must route the caller to a re-dispatch, got %T: %v", err, err)
	}
}

func TestUpdateCampaignBid_UnconfirmedHoldsTheLockAndDoesNotPersist(t *testing.T) {
	d := &bidWriterDispatcher{err: unconfirmedErr{}}
	s, camps := budgetService(t, bidCampaign(), d)
	_, err := s.UpdateCampaignBid(context.Background(), bidPayload(1, "cpc", "3"))
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected 503, got %T: %v", err, err)
	}
	if !strings.Contains(unavailable.Message, "verify the bid in the platform before retrying") {
		t.Errorf("the 503 must tell the caller to verify upstream, got %q", unavailable.Message)
	}
	if camps.got != nil {
		t.Error("an unconfirmed outcome must not write the row")
	}
	if camps.releases != 0 || len(camps.cooldowns) != 1 || camps.cooldowns[0] != unconfirmedLockCooldown {
		t.Errorf("want the lock held for one cooldown of %v, got releases=%d cooldowns=%v", unconfirmedLockCooldown, camps.releases, camps.cooldowns)
	}
}
