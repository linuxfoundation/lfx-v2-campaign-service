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
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// budgetWriterDispatcher is a dispatcher implementing ONLY BudgetWriter, so these tests drive
// the handler without the create path. err flips WriteBudget to its error arm; gotChange
// records what reached the dispatcher, so a test can assert the service forwarded the caller's
// amount and pacing rather than a silently-altered pair.
type budgetWriterDispatcher struct {
	err       error
	calls     int
	gotChange model.BudgetChange
}

func (d *budgetWriterDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (d *budgetWriterDispatcher) WriteBudget(_ context.Context, _ string, _ model.Provider, _ *model.Campaign, budget model.BudgetChange) error {
	d.calls++
	d.gotChange = budget
	return d.err
}

// budgetRepo is campaignEditRepo (which has a working ReplaceCampaign and a claim that does
// not bump the version) plus observation of the two lock releases. The cooldown release is
// what the UNCONFIRMED arm uses instead of an inline release, and "was the lock held?" cannot
// be inferred from anything else the fake records.
type budgetRepo struct {
	*campaignEditRepo
	releases  int
	cooldowns []time.Duration
}

func (r *budgetRepo) ReleaseCampaignLock(context.Context, domain.CampaignLockToken) error {
	r.releases++
	return nil
}

func (r *budgetRepo) ReleaseCampaignLockAfterCooldown(_ domain.CampaignLockToken, d time.Duration) {
	r.cooldowns = append(r.cooldowns, d)
}

// budgetService wires a BriefService around one stored campaign and one dispatcher registered
// for that campaign's platform.
func budgetService(t *testing.T, camp *model.Campaign, d PlatformDispatcher) (*BriefService, *budgetRepo) {
	t.Helper()
	camps := &budgetRepo{campaignEditRepo: &campaignEditRepo{cur: camp}}
	jobs := newFakeJobRepo()
	dispatchers := map[model.Provider]PlatformDispatcher{}
	if d != nil {
		dispatchers[camp.Platform] = d
	}
	orch := NewOrchestrator(camps, jobs, dispatchers)
	return NewBriefService(newFakeBriefRepo(), camps, jobs, orch), camps
}

// budgetCampaign is the ordinary subject: a provisioned, toggleable Google Ads campaign at
// version 3.
func budgetCampaign() *model.Campaign {
	return &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1",
		Platform: model.ProviderGoogleAds, PlatformCampaignID: "555",
		CampaignName: "KubeCon NA 2026 - Search",
		Status:       model.CampaignStatusCreated, Version: 3,
	}
}

func budgetPayload(amount float64, budgetType string, ifMatch string) *briefs.UpdateCampaignBudgetPayload {
	im := ifMatch
	return &briefs.UpdateCampaignBudgetPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
		IfMatch: &im, Budget: amount, BudgetType: budgetType,
	}
}

func TestUpdateCampaignBudget_HappyPathPersistsAmountAndLeavesStatusAlone(t *testing.T) {
	d := &budgetWriterDispatcher{}
	s, camps := budgetService(t, budgetCampaign(), d)

	ctx := ctxWithActor(&model.Actor{Username: "sinde"})
	res, err := s.UpdateCampaignBudget(ctx, budgetPayload(2500.50, "daily", "3"))
	if err != nil {
		t.Fatalf("UpdateCampaignBudget: %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("WriteBudget called %d times, want exactly 1", d.calls)
	}
	// The amount and the pacing must reach the dispatcher UNALTERED. A handler that rounded,
	// defaulted or swapped either one would still return 200 and still persist something.
	if d.gotChange.Amount != 2500.50 {
		t.Errorf("dispatcher got amount %v, want 2500.50", d.gotChange.Amount)
	}
	if d.gotChange.Type != model.BudgetDaily {
		t.Errorf("dispatcher got type %q, want %q", d.gotChange.Type, model.BudgetDaily)
	}
	if camps.got == nil {
		t.Fatal("ReplaceCampaign was never called on the happy path")
	}
	if camps.got.BudgetAmount == nil || *camps.got.BudgetAmount != 2500.50 {
		t.Errorf("persisted BudgetAmount = %v, want 2500.50", camps.got.BudgetAmount)
	}
	if camps.got.BudgetType == nil || *camps.got.BudgetType != model.BudgetDaily {
		t.Errorf("persisted BudgetType = %v, want %q", camps.got.BudgetType, model.BudgetDaily)
	}
	// Status is NOT this endpoint's business. Writing it here is how a 'created_degraded'
	// campaign would silently lose its reconciliation marker through a budget change.
	if camps.got.Status != model.CampaignStatusCreated {
		t.Errorf("status was modified by a budget change: got %q, want %q", camps.got.Status, model.CampaignStatusCreated)
	}
	// The write must be attributed to whoever asked. Resolving the actor AFTER the persist
	// context replaces ctx would silently produce a NULL author on every budget change.
	if camps.got.UpdatedBy == nil || camps.got.UpdatedBy.Username != "sinde" {
		t.Errorf("UpdatedBy = %+v, want the requesting actor", camps.got.UpdatedBy)
	}
	if res.Version != 4 {
		t.Errorf("result version = %d, want the bumped 4", res.Version)
	}
	if len(camps.indexPayloads) != 1 {
		t.Errorf("budget change must co-commit exactly one index event, got %d", len(camps.indexPayloads))
	}
	if len(camps.cooldowns) != 0 {
		t.Errorf("a confirmed change must not hold the lock for a cooldown, got %v", camps.cooldowns)
	}
}

// A 'created_degraded' campaign exists upstream and may be spending. It is deliberately
// ALLOWED here (unlike on an activate), and the marker must survive the write.
func TestUpdateCampaignBudget_CreatedDegradedIsAllowedAndKeepsItsMarker(t *testing.T) {
	camp := budgetCampaign()
	camp.Status = model.CampaignStatusCreatedDegraded
	d := &budgetWriterDispatcher{}
	s, camps := budgetService(t, camp, d)

	if _, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "lifetime", "3")); err != nil {
		t.Fatalf("a created_degraded campaign must accept a budget change: %v", err)
	}
	if camps.got.Status != model.CampaignStatusCreatedDegraded {
		t.Errorf("the reconciliation marker was lost: status = %q", camps.got.Status)
	}
	if d.gotChange.Type != model.BudgetLifetime {
		t.Errorf("dispatcher got type %q, want %q", d.gotChange.Type, model.BudgetLifetime)
	}
}

// Request validation runs BEFORE the row is loaded, the claim is taken, or the platform is
// contacted. Each case asserts zero platform calls, because a validator that runs after the
// mutate is not a validator.
func TestUpdateCampaignBudget_RejectsBadRequests(t *testing.T) {
	cases := []struct {
		name       string
		amount     float64
		budgetType string
	}{
		{"NaN", math.NaN(), "daily"},
		{"positive infinity", math.Inf(1), "daily"},
		{"negative infinity", math.Inf(-1), "daily"},
		{"zero", 0, "daily"},
		{"negative", -50, "daily"},
		{"over the maximum", maxCampaignBudget + 1, "daily"},
		// Positive, so it clears `budget <= 0`, but below half a micro — it rounds to zero
		// micros at the platform. Refused HERE or it takes the write lock and a live platform
		// read only to come back 503, which invites a retry that can never succeed.
		{"rounds to zero micros", 0.0000001, "daily"},
		{"empty budget_type", 100, ""},
		{"unknown budget_type", 100, "monthly"},
		{"uppercase budget_type", 100, "DAILY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &budgetWriterDispatcher{}
			s, camps := budgetService(t, budgetCampaign(), d)
			_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(tc.amount, tc.budgetType, "3"))
			var badReq *briefs.BadRequestError
			if !errors.As(err, &badReq) {
				t.Fatalf("expected 400 BadRequestError, got %T: %v", err, err)
			}
			if d.calls != 0 {
				t.Errorf("the platform was contacted %d times for a request that is invalid on its face", d.calls)
			}
			if camps.claims != 0 {
				t.Errorf("the write lock was claimed %d times for an invalid request", camps.claims)
			}
			if camps.got != nil {
				t.Error("the row must not be written for an invalid request")
			}
		})
	}
}

// The maximum is inclusive: the design states it as Maximum(1000000000), so the bound itself
// must be accepted rather than refused by an off-by-one.
func TestUpdateCampaignBudget_MaximumIsInclusive(t *testing.T) {
	d := &budgetWriterDispatcher{}
	s, _ := budgetService(t, budgetCampaign(), d)
	if _, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(maxCampaignBudget, "daily", "3")); err != nil {
		t.Fatalf("the maximum itself must be accepted: %v", err)
	}
}

// TestUpdateCampaignBudget_OneMicroIsAccepted pins the OTHER end of the range, and it is the
// end the design contract now publishes as its Minimum. One micro is the smallest amount that
// survives the platform's math.Round, so refusing it here would make the OpenAPI lower bound a
// value the service rejects — the same contract-looser-than-runtime defect in reverse.
func TestUpdateCampaignBudget_OneMicroIsAccepted(t *testing.T) {
	d := &budgetWriterDispatcher{}
	s, _ := budgetService(t, budgetCampaign(), d)
	if _, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(0.000001, "daily", "3")); err != nil {
		t.Fatalf("one micro is the declared minimum and must be accepted: %v", err)
	}
	if d.calls != 1 {
		t.Errorf("WriteBudget calls = %d, want 1", d.calls)
	}
}

func TestUpdateCampaignBudget_MissingIfMatchIsPreconditionRequired(t *testing.T) {
	d := &budgetWriterDispatcher{}
	s, _ := budgetService(t, budgetCampaign(), d)
	_, err := s.UpdateCampaignBudget(context.Background(), &briefs.UpdateCampaignBudgetPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Budget: 100, BudgetType: "daily",
	})
	var required *briefs.PreconditionRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("expected 428 PreconditionRequiredError, got %T: %v", err, err)
	}
	if d.calls != 0 {
		t.Error("the platform must not be contacted without an If-Match")
	}
}

// A stale ETag is a 412, and it is decided against the LOADED row before any state check — so
// a campaign that moved to a non-toggleable state concurrently still reports "refetch and
// retry" rather than a 409 about a row the client never saw.
func TestUpdateCampaignBudget_StaleIfMatchIsPreconditionFailed(t *testing.T) {
	camp := budgetCampaign()
	camp.Version = 7
	camp.Status = model.CampaignStatusPending
	d := &budgetWriterDispatcher{}
	s, camps := budgetService(t, camp, d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var preFailed *briefs.PreconditionFailedError
	if !errors.As(err, &preFailed) {
		t.Fatalf("expected 412 PreconditionFailedError, got %T: %v", err, err)
	}
	if d.calls != 0 {
		t.Error("the platform must not be contacted on a stale If-Match")
	}
	if camps.claims != 0 {
		t.Error("the write lock must not be claimed on a stale If-Match")
	}
}

// A campaign still provisioning has no settled upstream state to write against.
func TestUpdateCampaignBudget_NonToggleableStatusIsConflict(t *testing.T) {
	camp := budgetCampaign()
	camp.Status = model.CampaignStatusPending
	d := &budgetWriterDispatcher{}
	s, camps := budgetService(t, camp, d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected 409 ConflictError, got %T: %v", err, err)
	}
	if d.calls != 0 || camps.claims != 0 {
		t.Errorf("a refused state must reach neither the claim (%d) nor the platform (%d)", camps.claims, d.calls)
	}
}

// The email channel stages a draft for a human to send; there is no ad spend to set. Refused
// as a 400 (the request does not apply here at all) rather than a 409.
func TestUpdateCampaignBudget_EmailChannelIsRejected(t *testing.T) {
	camp := budgetCampaign()
	camp.Platform = model.ProviderHubSpot
	d := &budgetWriterDispatcher{}
	s, camps := budgetService(t, camp, d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("expected 400 BadRequestError for the email channel, got %T: %v", err, err)
	}
	if camps.claims != 0 {
		t.Error("the write lock must not be claimed for a channel that has no budget")
	}
}

// No upstream id means there is no upstream budget to address. Refused BEFORE the claim, so
// the orchestrator's identical guard is never the thing that catches it.
func TestUpdateCampaignBudget_UnprovisionedCampaignIsConflictBeforeTheClaim(t *testing.T) {
	camp := budgetCampaign()
	camp.PlatformCampaignID = ""
	d := &budgetWriterDispatcher{}
	s, camps := budgetService(t, camp, d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected 409 ConflictError, got %T: %v", err, err)
	}
	if camps.claims != 0 {
		t.Errorf("the write lock was claimed %d times for a campaign with no upstream id", camps.claims)
	}
}

// A platform whose dispatcher is registered but is NOT a BudgetWriter (plainDispatcher
// implements PlatformDispatcher and none of the optional capabilities) must answer 400 — the
// capability is missing, and no retry adds it. A registered non-writer is what exercises the
// type assertion; an absent dispatcher reaches the same sentinel by a different route.
func TestUpdateCampaignBudget_DispatcherWithoutTheCapabilityIsBadRequest(t *testing.T) {
	s, camps := budgetService(t, budgetCampaign(), plainDispatcher{})

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("expected 400 BadRequestError, got %T: %v", err, err)
	}
	if camps.got != nil {
		t.Error("the row must not be written when the platform cannot change budgets")
	}
}

// Every sentinel the dispatcher can return maps to one status, and NONE of them may persist
// the row: the platform either refused before mutating or failed definitely.
func TestUpdateCampaignBudget_DispatcherSentinelsMapToStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// want is a constructor for the expected error type, matched with errors.As.
		wantAs func(error) bool
	}{
		{"unsupported", ErrBudgetWriteUnsupported, isBadRequest},
		{"not provisioned", ErrCampaignNotProvisioned, isConflict},
		{"shared budget", ErrBudgetShared, isConflict},
		{"unwritable budget", ErrBudgetUnwritable, isConflict},
		// A refused AMOUNT is a permanent request fault, so 400 and never the 503 that
		// invites a retry of a request that can never succeed.
		{"rejected amount", ErrBudgetAmountRejected, isBadRequest},
		{"platform campaign absent", domain.ErrPlatformCampaignAbsent, isNotFound},
		{"unknown provenance", domain.ErrCampaignProvenanceUnknown, isConflict},
		{"account mismatch", ErrCampaignAccountMismatch, isConflict},
		{"system connection unusable", domain.ErrSystemConnectionNotUsable, isInternal},
		{"system connection missing", domain.ErrSystemConnectionMissing, isInternal},
		{"credential decryption failed", domain.ErrCredentialDecryptionFailed, isInternal},
		{"project connection unusable", domain.ErrConnectionNotUsable, isConflict},
		{"no connection at all", domain.ErrNotFound, isNotFound},
		{"definite platform failure", errors.New("google ads returned 400"), isUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &budgetWriterDispatcher{err: tc.err}
			s, camps := budgetService(t, budgetCampaign(), d)
			_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantAs(err) {
				t.Fatalf("unexpected error type %T: %v", err, err)
			}
			if camps.got != nil {
				t.Error("the row must not be written when the platform change did not happen")
			}
			// Every one of these is a settled outcome, so the lock is released inline rather
			// than held for the unconfirmed cooldown.
			if len(camps.cooldowns) != 0 {
				t.Errorf("a settled failure must not hold the lock for a cooldown, got %v", camps.cooldowns)
			}
			if camps.releases != 1 {
				t.Errorf("the claim lock was released %d times, want exactly 1", camps.releases)
			}
		})
	}
}

// The 400 for a refused amount CARRIES the adapter's own sentence, because that sentence is
// what lets the caller correct the request — and it must carry that sentence alone, not the
// rendered error chain, which accumulates the dispatcher's prefix and two sentinel texts
// around it. Anything without a client-safe reason still gets the generic message.
func TestUpdateCampaignBudget_RejectedAmountCarriesTheAdapterReasonOnly(t *testing.T) {
	reason := "daily budget 5 is below LinkedIn's minimum of $10 for a daily budget"
	d := &budgetWriterDispatcher{err: &reasonedBudgetAmountError{
		reason: reason,
		err:    fmt.Errorf("write linkedin campaign budget: %s: %w", reason, ErrBudgetAmountRejected),
	}}
	s, camps := budgetService(t, budgetCampaign(), d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(5, "daily", "3"))
	if !isBadRequest(err) {
		t.Fatalf("want a 400, got %T: %v", err, err)
	}
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("unexpected error type %T", err)
	}
	if !strings.Contains(bad.Message, reason) {
		t.Errorf("the 400 must carry the adapter's reason, got %q", bad.Message)
	}
	// The chain around the reason must NOT reach the caller: it names this service's own
	// internal wrapping, which tells an operator nothing they can act on.
	if strings.Contains(bad.Message, "write linkedin campaign budget") {
		t.Errorf("the 400 must not render the error chain, got %q", bad.Message)
	}
	if camps.got != nil {
		t.Error("the row must not be written when the amount was refused before the write")
	}
}

// reasonedBudgetAmountError mirrors the dispatch package's rejectedBudgetAmountError: it wraps
// the domain sentinel and exposes the client-safe reason through the same behavioral interface
// the service detects with errors.As. Defined here rather than imported because the dispatch
// type is unexported, and the service's contract is with the BEHAVIOUR, not with that type.
type reasonedBudgetAmountError struct {
	reason string
	err    error
}

func (e *reasonedBudgetAmountError) Error() string              { return e.err.Error() }
func (e *reasonedBudgetAmountError) Unwrap() error              { return e.err }
func (e *reasonedBudgetAmountError) BudgetAmountReason() string { return e.reason }

// A provenance-unknown error that ALSO carries the mismatch sentinel (which is how the
// dispatcher reports an absent provenance) must be reported as "re-dispatch", not as
// "reconnect the original account" — the row names no account to reconnect.
func TestUpdateCampaignBudget_AbsentProvenanceOutranksAccountMismatch(t *testing.T) {
	d := &budgetWriterDispatcher{err: errors.Join(domain.ErrCampaignProvenanceUnknown, ErrCampaignAccountMismatch)}
	s, _ := budgetService(t, budgetCampaign(), d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected 409 ConflictError, got %T: %v", err, err)
	}
	if !contains(conflict.Message, "re-dispatched") {
		t.Errorf("an absent provenance must route the caller to a re-dispatch, got %q", conflict.Message)
	}
}

// The client message for a shared budget must not leak upstream account or budget ids — those
// are connection configuration, not the caller's business.
func TestUpdateCampaignBudget_SharedBudgetMessageCarriesNoUpstreamIDs(t *testing.T) {
	d := &budgetWriterDispatcher{err: ErrBudgetShared}
	s, _ := budgetService(t, budgetCampaign(), d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected 409 ConflictError, got %T: %v", err, err)
	}
	if contains(conflict.Message, "555") {
		t.Errorf("client message must not carry the platform campaign id, got %q", conflict.Message)
	}
}

// UNCONFIRMED: the mutate may already have applied. The row is left untouched, the caller is
// told to verify, and the claim lock is held for the cooldown rather than released inline —
// otherwise the next caller claims the same still-unbumped version and writes the platform
// again while this call's outcome is unknown.
func TestUpdateCampaignBudget_UnconfirmedHoldsTheLockAndDoesNotPersist(t *testing.T) {
	d := &budgetWriterDispatcher{err: unconfirmedErr{}}
	s, camps := budgetService(t, budgetCampaign(), d)

	_, err := s.UpdateCampaignBudget(context.Background(), budgetPayload(100, "daily", "3"))
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected 503 ConnServiceUnavailableError, got %T: %v", err, err)
	}
	if camps.got != nil {
		t.Error("an unconfirmed outcome must not write the row — it might already be right, or not")
	}
	if camps.releases != 0 {
		t.Errorf("the lock was released inline %d times on an unconfirmed outcome", camps.releases)
	}
	if len(camps.cooldowns) != 1 {
		t.Fatalf("expected exactly one cooldown release, got %v", camps.cooldowns)
	}
	if camps.cooldowns[0] != unconfirmedLockCooldown {
		t.Errorf("cooldown = %v, want %v", camps.cooldowns[0], unconfirmedLockCooldown)
	}
}

func isBadRequest(err error) bool {
	var e *briefs.BadRequestError
	return errors.As(err, &e)
}

func isConflict(err error) bool {
	var e *briefs.ConflictError
	return errors.As(err, &e)
}

func isNotFound(err error) bool {
	var e *briefs.NotFoundError
	return errors.As(err, &e)
}

func isInternal(err error) bool {
	var e *briefs.InternalServerError
	return errors.As(err, &e)
}

func isUnavailable(err error) bool {
	var e *briefs.ConnServiceUnavailableError
	return errors.As(err, &e)
}
