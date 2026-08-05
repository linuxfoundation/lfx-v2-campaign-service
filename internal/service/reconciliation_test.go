// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	recon "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_reconciliation"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeReconRepo records what the handler passed down and returns canned results.
type fakeReconRepo struct {
	items []model.ReconciliationItem
	total int64
	err   error

	releaseErr error
	// captured arguments from the last ReleaseDispatchClaimByID call
	calls          int
	gotProjectID   string
	gotBriefID     string
	gotCampaignID  string
	gotVersion     int64
	gotMinAge      time.Duration
	gotListMinAge  time.Duration
	gotListLimit   int
	gotListProject string
}

func (f *fakeReconRepo) ListReconciliationItems(_ context.Context, projectID string, minClaimAge time.Duration, limit int) ([]model.ReconciliationItem, int64, error) {
	f.gotListProject, f.gotListMinAge, f.gotListLimit = projectID, minClaimAge, limit
	return f.items, f.total, f.err
}

func (f *fakeReconRepo) ReleaseDispatchClaimByID(_ context.Context, projectID, briefID, campaignID string, expectedVersion int64, minAge time.Duration) (*model.ReconciliationItem, error) {
	f.calls++
	f.gotProjectID, f.gotBriefID, f.gotCampaignID = projectID, briefID, campaignID
	f.gotVersion, f.gotMinAge = expectedVersion, minAge
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	return &model.ReconciliationItem{
		Kind:       model.ReconcileStuckClaim,
		BriefID:    briefID,
		CampaignID: campaignID,
		Status:     model.CampaignStatusPending,
		Version:    expectedVersion,
		Detail:     "Claim released.",
	}, nil
}

// TestReleaseDispatchClaim_RequiresVerifiedAbsent is the guard that keeps the operator's
// judgement in the loop. The service CANNOT check whether a paid campaign exists
// upstream, so a release must never proceed on a default. Critically, it must not even
// reach the repository — the assertion on calls==0 is what proves the refusal happens
// before any mutation is attempted.
func TestReleaseDispatchClaim_RequiresVerifiedAbsent(t *testing.T) {
	repo := &fakeReconRepo{}
	svc := NewReconciliationService(repo)

	_, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch:        strPtr("3"),
		VerifiedAbsent: false,
	})
	var badReq *recon.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("expected a 400 when verified_absent is false, got %v", err)
	}
	if repo.calls != 0 {
		t.Errorf("the repository must not be reached when the operator has not asserted verification (calls=%d)", repo.calls)
	}
}

// TestReleaseDispatchClaim_RequiresIfMatch verifies the conditional-request gate: without
// If-Match there is no version to check, so the TOCTOU protection would not exist.
func TestReleaseDispatchClaim_RequiresIfMatch(t *testing.T) {
	repo := &fakeReconRepo{}
	svc := NewReconciliationService(repo)

	_, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		VerifiedAbsent: true,
	})
	var preReq *recon.PreconditionRequiredError
	if !errors.As(err, &preReq) {
		t.Fatalf("expected a 428 when If-Match is absent, got %v", err)
	}
	if repo.calls != 0 {
		t.Errorf("the repository must not be reached without an If-Match version (calls=%d)", repo.calls)
	}
}

// TestReleaseDispatchClaim_RejectsWeakValidator pins RFC 7232 §3.1: If-Match uses the
// strong comparison function, so a weak tag must not authorize this write.
func TestReleaseDispatchClaim_RejectsWeakValidator(t *testing.T) {
	repo := &fakeReconRepo{}
	svc := NewReconciliationService(repo)

	_, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch:        strPtr(`W/"3"`),
		VerifiedAbsent: true,
	})
	var badReq *recon.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("expected a 400 for a weak validator, got %v", err)
	}
	if repo.calls != 0 {
		t.Errorf("the repository must not be reached for a weak validator (calls=%d)", repo.calls)
	}
}

// TestReleaseDispatchClaim_PassesFloorAndParsedVersion verifies the handler forwards the
// operator's version and the DERIVED release floor rather than letting the repository
// pick its own bound. Passing a zero floor would silently disable the age backstop.
func TestReleaseDispatchClaim_PassesFloorAndParsedVersion(t *testing.T) {
	repo := &fakeReconRepo{}
	svc := NewReconciliationService(repo)

	if _, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch:        strPtr(`"7"`), // quoted strong tag must be accepted
		VerifiedAbsent: true,
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if repo.gotVersion != 7 {
		t.Errorf("expectedVersion = %d, want 7 (a quoted strong ETag must parse)", repo.gotVersion)
	}
	if repo.gotMinAge != model.ClaimReleaseFloor {
		t.Errorf("minAge = %v, want ClaimReleaseFloor (%v)", repo.gotMinAge, model.ClaimReleaseFloor)
	}
	if repo.gotProjectID != "cncf" || repo.gotBriefID != "b1" || repo.gotCampaignID != "c1" {
		t.Errorf("scoping args not forwarded: project=%q brief=%q campaign=%q",
			repo.gotProjectID, repo.gotBriefID, repo.gotCampaignID)
	}
}

// TestReleaseDispatchClaim_MapsConflict verifies a non-releasable claim surfaces as 409
// with a message that tells the operator WHY, rather than the generic "already exists"
// the shared brief mapper would produce.
func TestReleaseDispatchClaim_MapsConflict(t *testing.T) {
	repo := &fakeReconRepo{releaseErr: domain.ErrConflict}
	svc := NewReconciliationService(repo)

	_, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch:        strPtr("3"),
		VerifiedAbsent: true,
	})
	var conflict *recon.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a 409 for a non-releasable claim, got %v", err)
	}
	if conflict.Message == "the resource already exists" {
		t.Error("the 409 must explain why the claim is not releasable, not report a uniqueness violation")
	}
}

// TestReleaseDispatchClaim_MapsPreconditionFailed verifies a version mismatch (the
// re-claimed-live-claim case) reaches the caller as 412 so they re-read the report.
func TestReleaseDispatchClaim_MapsPreconditionFailed(t *testing.T) {
	repo := &fakeReconRepo{releaseErr: domain.ErrPreconditionFailed}
	svc := NewReconciliationService(repo)

	_, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch:        strPtr("3"),
		VerifiedAbsent: true,
	})
	var preFailed *recon.PreconditionFailedError
	if !errors.As(err, &preFailed) {
		t.Fatalf("expected a 412 on a version mismatch, got %v", err)
	}
}

// TestGetReconciliation_ReportsTruncationAndResolvability covers the read path: the
// report must carry an honest total when truncated, and must faithfully propagate each
// item's resolvable verdict — an operator acting on a wrongly-resolvable item is exactly
// the failure this endpoint exists to prevent.
func TestGetReconciliation_ReportsTruncationAndResolvability(t *testing.T) {
	repo := &fakeReconRepo{
		items: []model.ReconciliationItem{
			{Kind: model.ReconcileStuckClaim, BriefID: "b1", CampaignID: "c1",
				Status: model.CampaignStatusPending, Age: 30 * time.Minute, Version: 1, Resolvable: true},
			{Kind: model.ReconcileUnconfirmedCampaign, BriefID: "b1", CampaignID: "c2",
				Status: "unconfirmed", PlatformCampaignID: "cid-9", Age: time.Hour, Version: 2},
			{Kind: model.ReconcilePartialAudience, BriefID: "b1", AudienceID: "a1",
				Status: "building", Age: 2 * time.Hour, Version: 1},
		},
		total: 12,
	}
	svc := NewReconciliationService(repo)

	rep, err := svc.GetReconciliation(context.Background(), &recon.GetReconciliationPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("get reconciliation: %v", err)
	}
	if !rep.Truncated {
		t.Error("a report returning 3 of 12 items must be marked truncated")
	}
	if rep.Total != 12 {
		t.Errorf("total = %d, want the true count 12", rep.Total)
	}
	if !rep.Items[0].Resolvable {
		t.Error("a bare stranded claim must stay resolvable through the mapping")
	}
	for _, it := range rep.Items[1:] {
		if it.Resolvable {
			t.Errorf("%s must not be reported resolvable", it.Kind)
		}
	}
	// The ETag is what the operator feeds back as If-Match, so it must be populated.
	if rep.Items[0].Etag == nil || *rep.Items[0].Etag != "1" {
		t.Error("each item must carry the ETag the operator supplies as If-Match")
	}
	if rep.Items[1].PlatformCampaignID == nil || *rep.Items[1].PlatformCampaignID != "cid-9" {
		t.Error("a recorded upstream id must be surfaced — it is the evidence the operator acts on")
	}
	if rep.Items[2].AudienceID == nil || *rep.Items[2].AudienceID != "a1" {
		t.Error("an audience item must carry its audience id")
	}
}

// TestGetReconciliation_ExcludesHealthyInFlightDispatches verifies the handler asks for a
// report age above the orchestrator's provider-call bound, so a dispatch that is merely
// slow is never presented to an operator as needing attention.
func TestGetReconciliation_ExcludesHealthyInFlightDispatches(t *testing.T) {
	repo := &fakeReconRepo{}
	svc := NewReconciliationService(repo)

	if _, err := svc.GetReconciliation(context.Background(), &recon.GetReconciliationPayload{ProjectID: "cncf"}); err != nil {
		t.Fatalf("get reconciliation: %v", err)
	}
	if repo.gotListMinAge <= providerCallTimeout {
		t.Errorf("report min age (%v) must exceed providerCallTimeout (%v), or a healthy in-flight dispatch is reported as stuck",
			repo.gotListMinAge, providerCallTimeout)
	}
	if repo.gotListLimit <= 0 {
		t.Error("the inventory must be bounded, or a pathological project produces an unbounded response")
	}
	if repo.gotListProject != "cncf" {
		t.Errorf("project scoping not forwarded: got %q", repo.gotListProject)
	}
}

// TestReconciliationServiceUnavailableWithoutDatabase verifies the no-database mode
// returns the typed 503 the OpenAPI contract advertises, matching the other services,
// rather than nil-panicking on an unwired repo.
func TestReconciliationServiceUnavailableWithoutDatabase(t *testing.T) {
	svc := NewReconciliationService(nil)

	if _, err := svc.GetReconciliation(context.Background(), &recon.GetReconciliationPayload{ProjectID: "cncf"}); err == nil {
		t.Error("expected a typed 503 from the read path with no database wired")
	} else {
		var unavail *recon.ConnServiceUnavailableError
		if !errors.As(err, &unavail) {
			t.Errorf("expected ConnServiceUnavailableError, got %v", err)
		}
	}
	if _, err := svc.ReleaseDispatchClaim(context.Background(), &recon.ReleaseDispatchClaimPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: strPtr("1"), VerifiedAbsent: true,
	}); err == nil {
		t.Error("expected a typed 503 from the write path with no database wired")
	}
}

// TestClaimReleaseFloorExceedsDispatchBound is the invariant that makes the age floor
// meaningful. A live dispatch is bounded by providerCallTimeout plus the detached
// persist/release window; a floor at or below that would let the API release a claim
// whose provider call is still running, and a concurrent dispatch could then create a
// SECOND paid campaign. The floor must stay comfortably above the real bound.
//
// dispatchQueueTimeout (10m) is deliberately NOT a term of that bound, and this is the
// subtle part. A platform goroutine waits for the semaphore slot BEFORE calling
// dispatchPlatform, which is where ClaimCampaignDispatch inserts the row — so no claim
// row exists during the queue wait, and a queued dispatch cannot own a claim whose age
// is accruing. If that ordering were ever inverted (claiming before acquiring the slot),
// a claim could legitimately sit 'pending' for up to dispatchQueueTimeout +
// providerCallTimeout = 12m, and this 15m floor would lose most of its margin. The
// second assertion below fails if someone shrinks the floor to where that inverted
// ordering would become unsafe, so the risk surfaces as a test failure either way.
func TestClaimReleaseFloorExceedsDispatchBound(t *testing.T) {
	liveDispatchBound := providerCallTimeout + persistResultTimeout
	if model.ClaimReleaseFloor <= liveDispatchBound {
		t.Fatalf("ClaimReleaseFloor (%v) must exceed the live-dispatch bound (%v = providerCallTimeout %v + persistResultTimeout %v): "+
			"a shorter floor lets the API release a claim whose provider call is still in flight, allowing a duplicate paid campaign",
			model.ClaimReleaseFloor, liveDispatchBound, providerCallTimeout, persistResultTimeout)
	}
	// Defence in depth: keep the floor above the worst case that WOULD apply if the
	// claim were ever taken before the semaphore wait.
	worstCaseIfClaimedBeforeQueueing := dispatchQueueTimeout + providerCallTimeout + persistResultTimeout
	if model.ClaimReleaseFloor <= worstCaseIfClaimedBeforeQueueing {
		t.Errorf("ClaimReleaseFloor (%v) should also exceed dispatchQueueTimeout + providerCallTimeout + persistResultTimeout (%v), "+
			"so the floor stays safe even if the claim is ever moved before the semaphore acquire",
			model.ClaimReleaseFloor, worstCaseIfClaimedBeforeQueueing)
	}
}

// TestReconcileReportMinAgeBelowReleaseFloor pins the deliberate asymmetry between
// SEEING a stuck state and ACTING on one. Reporting early is useful and harmless;
// acting early is not. If the report age ever rose above the release floor, operators
// would only see items they were already permitted to release, hiding the unconfirmed
// states that most need human eyes.
func TestReconcileReportMinAgeBelowReleaseFloor(t *testing.T) {
	if reconcileReportMinAge >= model.ClaimReleaseFloor {
		t.Errorf("reconcileReportMinAge (%v) must stay below ClaimReleaseFloor (%v) so operators can SEE a stuck state before they may act on it",
			reconcileReportMinAge, model.ClaimReleaseFloor)
	}
	if reconcileReportMinAge <= providerCallTimeout {
		t.Errorf("reconcileReportMinAge (%v) must exceed providerCallTimeout (%v) so a healthy in-flight dispatch is never listed",
			reconcileReportMinAge, providerCallTimeout)
	}
}
