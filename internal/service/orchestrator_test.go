// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// fakeJobRepo records job status transitions.
type fakeJobRepo struct {
	mu             sync.Mutex
	jobs           map[string]*model.CampaignJob
	counter        int
	failStuckCalls int
	// pruneCalls counts PruneTerminalJobs calls, and pruneOlderThan/pruneLimit record the
	// arguments of the LAST one, so a test can assert the sweeper passes the configured
	// window through rather than silently substituting its own.
	pruneCalls     int
	pruneOlderThan time.Duration
	pruneLimit     int
}

func newFakeJobRepo() *fakeJobRepo { return &fakeJobRepo{jobs: map[string]*model.CampaignJob{}} }

func (r *fakeJobRepo) CreateJob(_ context.Context, briefID string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(briefID)
}

// createLocked inserts a queued job. Caller must hold r.mu.
func (r *fakeJobRepo) createLocked(briefID string) (*model.CampaignJob, error) {
	r.counter++
	id := "job-" + string(rune('a'+r.counter))
	j := &model.CampaignJob{ID: id, BriefID: briefID, Status: model.JobQueued}
	r.jobs[id] = j
	return j, nil
}

// CreateJobForApprovedBrief mirrors the unconditional create for the orchestrator
// tests (which don't wire a brief store, so the approval guard is exercised
// separately by the brief-service TOCTOU test with its own version-aware fake).
func (r *fakeJobRepo) CreateJobForApprovedBrief(_ context.Context, briefID string, _ int64) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(briefID)
}

func (r *fakeJobRepo) GetJob(_ context.Context, _, id string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, errors.New("not found")
	}
	// Return a snapshot so callers don't race with concurrent UpdateJobStatus.
	cp := *j
	return &cp, nil
}

func (r *fakeJobRepo) UpdateJobStatus(_ context.Context, id string, status model.JobStatus, result []byte, jobErr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	j.Status = status
	j.Result = result
	j.Error = jobErr
	return nil
}

func (r *fakeJobRepo) failStuckCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failStuckCalls
}

func (r *fakeJobRepo) FailStuckJobs(_ context.Context, jobErr string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStuckCalls++
	var n int64
	for _, j := range r.jobs {
		if j.Status == model.JobQueued || j.Status == model.JobRunning {
			j.Status = model.JobFailed
			j.Error = jobErr
			n++
		}
	}
	return n, nil
}

func (r *fakeJobRepo) pruneCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pruneCalls
}

func (r *fakeJobRepo) lastPruneArgs() (time.Duration, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pruneOlderThan, r.pruneLimit
}

// PruneTerminalJobs mirrors the real repo's CONTRACT, not just its signature: it deletes only
// jobs whose status is terminal and whose UpdatedAt is older than the window, and it honours
// the batch bound. A stub that deleted everything (or nothing) would let a broken sweeper pass
// — the point of these tests is that the wrong rows are not removed.
func (r *fakeJobRepo) PruneTerminalJobs(_ context.Context, olderThan time.Duration, limit int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneCalls++
	r.pruneOlderThan = olderThan
	r.pruneLimit = limit
	if olderThan <= 0 {
		olderThan = 180 * 24 * time.Hour
	}
	if limit <= 0 {
		limit = 1000
	}
	cutoff := time.Now().Add(-olderThan)
	// Delete in a deterministic order so the batch bound is testable: map iteration order is
	// randomised, so an unordered stub would drop an arbitrary subset under a LIMIT.
	ids := make([]string, 0, len(r.jobs))
	for id := range r.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var n int64
	for _, id := range ids {
		if n >= int64(limit) {
			break
		}
		j := r.jobs[id]
		if !j.Status.Terminal() || !j.UpdatedAt.Before(cutoff) {
			continue
		}
		delete(r.jobs, id)
		n++
	}
	return n, nil
}

// fakeCampaignRepo records upserted campaigns and simulates the claim table.
type fakeCampaignRepo struct {
	mu       sync.Mutex
	upserted []*model.Campaign
	// adopted records campaigns bound via AdoptCampaign, kept separate from upserted so a
	// test can tell "bound an existing upstream campaign" from "wrote one we created".
	adopted []*model.Campaign
	// indexPayloads records the co-committed index messages, so a test can assert a campaign is
	// indexed rather than only persisted.
	indexPayloads [][]byte
	// existing maps briefID+"|"+platform to a pre-existing campaign, letting a
	// test simulate a brief already dispatched to a platform (idempotency guard).
	existing map[string]*model.Campaign
	// adoptBriefVersion, when non-zero, is the brief version AdoptCampaign's locked re-read
	// would find. A mismatch against the caller's expectedVersion is ErrStaleApproval.
	adoptBriefVersion int64
	// byPlatformErr, when set, is returned by GetCampaignByPlatform to simulate a
	// transient lookup failure.
	byPlatformErr error
	// claimErr, when set, is returned by ClaimCampaignDispatch.
	claimErr error
	// byID, when set, backs GetCampaign so a test can drive the service's campaign-scoped
	// handlers (e.g. the status toggle) which look a campaign up by its own id.
	byID map[string]*model.Campaign
	// claimVersionErr, when set, is returned by ClaimCampaignVersion.
	claimVersionErr error
	// claimActors records the `by` argument of every ClaimCampaignDispatch call, so a
	// test can assert the DISPATCHING actor reached the claim INSERT — which is the
	// only INSERT this row ever gets — rather than only checking the model field.
	claimActors []*model.Actor
}

func (r *fakeCampaignRepo) GetCampaign(_ context.Context, _, _, campaignID string) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.byID[campaignID]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, errors.New("unused")
}

// ListCampaignsForBrief mirrors the real query's semantics rather than merely satisfying the
// interface: it excludes soft-deleted rows and returns the SAME (platform, variant) ordering
// the SQL guarantees. A fake that returned insertion order would let a brief-metrics test
// pass against a handler that had stopped depending on a stable order — which is the whole
// contract the ORDER BY exists to provide.
func (r *fakeCampaignRepo) ListCampaignsForBrief(_ context.Context, projectID, briefID string) ([]*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.Campaign, 0)
	seen := make(map[string]bool)
	for _, c := range append(append([]*model.Campaign{}, r.upserted...), r.adopted...) {
		if c == nil || c.BriefID != briefID || c.ProjectID != projectID || c.Status == "deleted" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	for _, c := range r.existing {
		if c == nil || c.BriefID != briefID || c.ProjectID != projectID || c.Status == "deleted" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	// byID too: it is what the campaign-scoped handler tests populate, and a brief-wide read
	// that could not see those rows would report every such brief as empty — the fake would
	// then be answering a different question than the repository it stands in for.
	for _, c := range r.byID {
		if c == nil || c.BriefID != briefID || c.ProjectID != projectID || c.Status == "deleted" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].Variant < out[j].Variant
	})
	return out, nil
}

func (r *fakeCampaignRepo) GetCampaignByPlatform(_ context.Context, _ string, briefID string, platform model.Provider, variant string) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byPlatformErr != nil {
		return nil, r.byPlatformErr
	}
	// The real slot key is (brief, platform, VARIANT) — migration 000022. This fake ignored
	// the variant and keyed on (brief, platform) alone, which made EVERY variant look
	// occupied once any one of them was: a Demand Gen adoption onto a brief holding a Search
	// campaign was reported as a conflict the real repository would have accepted. A fake
	// that does not model the constraint hides the bug the constraint exists to catch.
	//
	// Keys may be written either way. The variant-qualified form is preferred; the bare
	// (brief, platform) form is accepted as meaning the DEFAULT slot, so the many existing
	// single-variant fixtures keep working without being rewritten.
	if c, ok := r.existing[slotKey(briefID, platform, variant)]; ok {
		return c, nil
	}
	if c, ok := r.existing[legacySlotKey(briefID, platform)]; ok && model.NormalizeVariant(variant) == model.VariantDefault {
		return c, nil
	}
	return nil, domain.ErrNotFound
}

// ClaimCampaignDispatch simulates INSERT ... ON CONFLICT DO NOTHING: if an entry
// for (brief, platform) already exists it's a conflict (not claimed) returning
// the existing row; otherwise it inserts a pending placeholder and claims.
// storeRow writes a campaign under its variant-qualified slot key, and ALSO under the bare
// (brief, platform) key when it occupies the DEFAULT slot.
//
// The dual write is for the TESTS, not the schema: many assertions read a row back as
// existing["b1|linkedin-ads"], and those fixtures are single-variant, so the two keys name
// the same row. Writing only the qualified form would break them for a reason unrelated to
// what they assert. Writes are still keyed correctly — a demand-gen row never lands on the
// bare key, so it cannot be mistaken for a Search row.
func (r *fakeCampaignRepo) storeRow(c *model.Campaign) {
	if r.existing == nil {
		r.existing = map[string]*model.Campaign{}
	}
	r.existing[slotKey(c.BriefID, c.Platform, c.Variant)] = c
	if model.NormalizeVariant(c.Variant) == model.VariantDefault {
		r.existing[legacySlotKey(c.BriefID, c.Platform)] = c
	}
}

// slotKey is THE key for this fake, mirroring the real partial unique index from migration
// 000022: (brief_id, platform, variant). Every method that reads, writes or deletes a row
// goes through it.
//
// It exists because they drifted. GetCampaignByPlatform and AdoptCampaign were made
// variant-aware while ClaimCampaignDispatch, DeleteDispatchClaim and UpsertCampaign kept
// keying on (brief, platform) alone — so a Demand Gen dispatch missed on the read, then had
// its claim answered with the brief's SEARCH row, which is the duplicate-paid-campaign shape
// the slot key exists to prevent. A fake whose methods disagree about the key models no
// schema at all.
func slotKey(briefID string, platform model.Provider, variant string) string {
	return briefID + "|" + string(platform) + "|" + model.NormalizeVariant(variant)
}

// legacySlotKey is the bare (brief, platform) form many fixtures still use. It means the
// DEFAULT slot, so reads accept it for that variant only — new writes always use slotKey.
func legacySlotKey(briefID string, platform model.Provider) string {
	return briefID + "|" + string(platform)
}

func (r *fakeCampaignRepo) ClaimCampaignDispatch(_ context.Context, projectID, briefID string, platform model.Provider, variant, jobID string, by *model.Actor) (bool, *model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Recorded BEFORE the error and conflict returns: the question the binding test asks
	// is what the orchestrator PASSED, which it did on every one of these paths.
	r.claimActors = append(r.claimActors, by)
	if r.claimErr != nil {
		return false, nil, r.claimErr
	}
	key := slotKey(briefID, platform, variant)
	if c, ok := r.existing[key]; ok {
		return false, c, nil
	}
	// A fixture written in the bare form occupies the DEFAULT slot only.
	if model.NormalizeVariant(variant) == model.VariantDefault {
		if c, ok := r.existing[legacySlotKey(briefID, platform)]; ok {
			return false, c, nil
		}
	}
	// Both actor columns, because claimCampaignDispatchQuery inserts `created_by, updated_by`
	// from the SAME $5. A fake that stamped only CreatedBy would hand every orchestrator test
	// a claimed row production cannot create, and the creation-time updated_by invariant would
	// have no fake capable of catching a regression in it.
	pending := &model.Campaign{ProjectID: projectID, BriefID: briefID, Platform: platform, Variant: model.NormalizeVariant(variant), JobID: &jobID, Status: "pending", CreatedBy: by, UpdatedBy: by}
	if r.existing == nil {
		r.existing = map[string]*model.Campaign{}
	}
	r.existing[key] = pending
	return true, pending, nil
}

func (r *fakeCampaignRepo) DeleteDispatchClaim(_ context.Context, briefID string, platform model.Provider, variant string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Only this slot's claim: releasing on the bare key would free a DIFFERENT variant's
	// pending row, which the real DELETE (keyed on all three columns) cannot do.
	for _, key := range []string{slotKey(briefID, platform, variant), legacySlotKey(briefID, platform)} {
		if key == legacySlotKey(briefID, platform) && model.NormalizeVariant(variant) != model.VariantDefault {
			continue
		}
		if c, ok := r.existing[key]; ok && c.Status == "pending" {
			delete(r.existing, key)
		}
	}
	return nil
}

func (r *fakeCampaignRepo) UpsertCampaign(_ context.Context, c *model.Campaign, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Version = 1
	r.upserted = append(r.upserted, c)
	// Run the payload builder the way the real repo does — inside the "transaction", after the
	// row is written — and record it, so a test can assert the campaign is actually indexed
	// rather than merely persisted.
	if indexPayload != nil {
		payload, perr := indexPayload(c)
		if perr != nil {
			return nil, perr
		}
		r.indexPayloads = append(r.indexPayloads, payload)
	}
	// Mirror the real ON CONFLICT (brief_id, platform) DO UPDATE: the (brief,
	// platform) row is updated in place, so a subsequent lookup sees the new
	// platform_campaign_id/status.
	r.storeRow(c)
	return c, nil
}

// AdoptCampaign mirrors CampaignRepo.AdoptCampaign: an INSERT that DECLINES rather than
// updating when the (brief, platform) pair already has a live row. Modelling the refusal is
// the point — a fake that simply overwrote, as UpsertCampaign does, would let a handler that
// clobbers an existing binding pass every adoption test.
// It also models the two guards the real statement gets from the database and not from Go: the
// second live unique index (one upstream campaign, one live binding per project) and the locked
// re-read of the brief's approval. A fake missing either lets a handler that skips them pass.
func (r *fakeCampaignRepo) AdoptCampaign(_ context.Context, c *model.Campaign, expectedVersion int64, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adoptBriefVersion != 0 && r.adoptBriefVersion != expectedVersion {
		return nil, domain.ErrStaleApproval
	}
	// Keyed by (brief, platform, VARIANT), matching the real partial unique index from
	// migration 000022. Keying on (brief, platform) alone made this fake reject a Demand Gen
	// adoption onto a brief holding a Search campaign -- a pair the real index accepts -- so
	// the fake enforced a constraint stricter than production and hid the bug that the
	// service-side pre-check was refusing the same pair.
	variant := model.NormalizeVariant(c.Variant)
	for _, key := range []string{
		c.BriefID + "|" + string(c.Platform) + "|" + variant,
		// The bare form means the DEFAULT slot, so existing single-variant fixtures still
		// collide correctly without being rewritten.
		func() string {
			if variant == model.VariantDefault {
				return c.BriefID + "|" + string(c.Platform)
			}
			return ""
		}(),
	} {
		if key == "" {
			continue
		}
		if existing, ok := r.existing[key]; ok && existing.Status != "deleted" {
			return nil, fmt.Errorf("%w: brief %s already has a live %s campaign", domain.ErrConflict, c.BriefID, c.Platform)
		}
	}
	for _, other := range r.existing {
		// No project comparison: 000020's index is keyed (platform, platform_campaign_id)
		// only, and a fake that filtered by project would accept a binding the real schema
		// rejects -- the exact cross-project collision the global key exists for.
		if other.Status != "deleted" &&
			other.Platform == c.Platform && other.PlatformCampaignID == c.PlatformCampaignID {
			return nil, fmt.Errorf("%w: %s campaign %s is bound to brief %s",
				domain.ErrPlatformCampaignAlreadyBound, c.Platform, c.PlatformCampaignID, other.BriefID)
		}
	}
	c.Version = 1
	if indexPayload != nil {
		payload, perr := indexPayload(c)
		if perr != nil {
			return nil, perr
		}
		r.indexPayloads = append(r.indexPayloads, payload)
	}
	if r.existing == nil {
		r.existing = map[string]*model.Campaign{}
	}
	// Stored variant-qualified so a later lookup for a DIFFERENT slot on the same brief does
	// not find this row -- the whole point of the slot key.
	r.storeRow(c)
	r.adopted = append(r.adopted, c)
	return c, nil
}

func (r *fakeCampaignRepo) ReplaceCampaign(context.Context, *model.Campaign, int64, domain.CampaignLockToken, domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (r *fakeCampaignRepo) VerifyClaimedVersion(_ context.Context, _, _, campaignID string, expectedVersion int64, _ domain.CampaignLockToken) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byID[campaignID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if c.Version != expectedVersion {
		return nil, domain.ErrPreconditionFailed
	}
	cp := *c
	return &cp, nil
}

// ClaimCampaignVersion mirrors CampaignRepo.ClaimCampaignVersion: it gates on the expected
// version and reports precondition-failed / not-found, and it returns the row's snapshot
// UNCHANGED.
//
// The version is deliberately not bumped here. Production leaves it to ReplaceCampaign so the
// increment co-commits with the outbox event; a fake that bumps at claim time models a
// lifecycle production cannot provide, and any test built on it would pass against code that
// double-bumps or that reads a version the real claim never produces.
func (r *fakeCampaignRepo) ClaimCampaignVersion(_ context.Context, _, _, campaignID string, expectedVersion int64) (*model.Campaign, domain.CampaignLockToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimVersionErr != nil {
		return nil, domain.CampaignLockToken{}, r.claimVersionErr
	}
	c, ok := r.byID[campaignID]
	if !ok {
		return nil, domain.CampaignLockToken{}, domain.ErrNotFound
	}
	if c.Version != expectedVersion {
		return nil, domain.CampaignLockToken{}, domain.ErrPreconditionFailed
	}
	cp := *c
	return &cp, domain.NewCampaignLockToken(campaignID, &cp), nil
}

func (r *fakeCampaignRepo) ReleaseCampaignLock(context.Context, domain.CampaignLockToken) error {
	return nil
}

func (r *fakeCampaignRepo) ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken, time.Duration) {
}

// DeleteCampaign mirrors the real repo's guard ORDER and its soft-delete semantics:
// missing/already-deleted → ErrNotFound, mid-dispatch 'pending' → ErrConflict, then
// the version check, and finally a status flip to 'deleted' that leaves the row in
// place (so a re-dispatch to the same pair can claim the freed slot).
func (r *fakeCampaignRepo) DeleteCampaign(_ context.Context, _, _, campaignID string, expectedVersion int64, _ *model.Actor, _ domain.CampaignIndexPayloadFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byID[campaignID]
	if !ok || c.Status == model.CampaignStatusDeleted {
		return domain.ErrNotFound
	}
	if c.Status == model.CampaignStatusPending {
		return domain.ErrConflict
	}
	if c.Version != expectedVersion {
		return domain.ErrPreconditionFailed
	}
	c.Status = model.CampaignStatusDeleted
	c.Version++
	// The freed slot: a deleted row no longer satisfies the live-only lookup, so
	// GetCampaignByPlatform/ClaimCampaignDispatch see the pair as undispatched.
	delete(r.existing, c.BriefID+"|"+string(c.Platform))
	return nil
}

// okDispatcher always succeeds.
type okDispatcher struct{}

func (okDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// failDispatcher always fails.
type failDispatcher struct{}

func (failDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("boom")
}

// nilDispatcher returns (nil, nil) — a misbehaving dispatcher that must be
// handled as a failure rather than panicking on the ownership stamp.
type nilDispatcher struct{}

func (nilDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil //nolint:nilnil // deliberately exercises the nil-campaign guard
}

// partialOrphanDispatcher returns a retained-partial campaign carrying a degraded
// status + a reconcile Result but NO upstream id (a group-orphan / unconfirmed
// partial), ALONGSIDE an error. It exercises the orchestrator's status-preservation:
// the row must persist with the degraded status (not be flattened to "pending"), and
// must NOT be reusable on a retry (its status is non-terminal).
type partialOrphanDispatcher struct{ status string }

func (d partialOrphanDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{
			Status:       d.status,
			Result:       json.RawMessage(`{"campaignGroupId":"g1"}`),
			CampaignName: "n",
		}, // no PlatformCampaignID: the campaign was not created, only the group
		errors.New("campaign create ambiguous after group created")
}

// degradedCreatedDispatcher returns a campaign that WAS created (has an id) with a
// terminal created_degraded status ALONGSIDE an error — the LinkedIn shape when the
// campaign lands but fewer creatives than requested succeed.
type degradedCreatedDispatcher struct{}

func (degradedCreatedDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{
			PlatformCampaignID: "pc-" + string(p),
			Status:             "created_degraded",
			CampaignName:       "n",
		},
		errors.New("only 2 of 3 creatives created")
}

func waitForTerminal(t *testing.T, jobs *fakeJobRepo, id string) *model.CampaignJob {
	t.Helper()
	for i := 0; i < 100; i++ {
		j, _ := jobs.GetJob(context.Background(), "", id)
		if j.Status.Terminal() {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not reach terminal status")
	return nil
}

// waitForFinalized waits until the run's finalize write has landed (a non-empty
// result recorded), regardless of whether the resulting status is terminal. A
// job whose only outcomes are single-flight SKIPs finalizes to a non-terminal
// 'running' status (its skipped pairs are owned by another dispatch), so such a
// job never satisfies waitForTerminal — this helper observes its finalize instead.
func waitForFinalized(t *testing.T, jobs *fakeJobRepo, id string) *model.CampaignJob {
	t.Helper()
	for i := 0; i < 100; i++ {
		j, _ := jobs.GetJob(context.Background(), "", id)
		if len(j.Result) > 0 {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job result was never finalized")
	return nil
}

func TestOrchestrator_AllSucceed(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
	if len(camps.upserted) != 2 {
		t.Errorf("upserted %d campaigns, want 2", len(camps.upserted))
	}
}

func TestOrchestrator_PartialFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: failDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobPartial {
		t.Errorf("status = %s, want partial", j.Status)
	}
}

// TestOrchestrator_PreservesDegradedStatusOnRetainedOrphan verifies the fix for the
// PR #37 review finding: a dispatcher-set degraded status (group_created / unconfirmed)
// on a retained-partial orphan is PERSISTED on the campaign row (not flattened to
// "pending"). This runs the persist/preserve half end to end (Start -> retain ->
// UpsertCampaign). The "not reusable on a later dispatch" half is asserted directly via
// isReusableCampaign(row) below (the reuse gate itself is table-locked in
// TestIsReusableCampaign) rather than by driving a second Start.
func TestOrchestrator_PreservesDegradedStatusOnRetainedOrphan(t *testing.T) {
	for _, status := range []string{"group_created", "unconfirmed"} {
		t.Run(status, func(t *testing.T) {
			jobs := newFakeJobRepo()
			camps := &fakeCampaignRepo{}
			orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
				model.ProviderLinkedInAds: partialOrphanDispatcher{status: status},
			})
			brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
			id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderLinkedInAds}, nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitForTerminal(t, jobs, id)

			camps.mu.Lock()
			defer camps.mu.Unlock()
			// The group-orphan MUST be recorded (id-less rows were previously dropped):
			// exactly one row upserted, carrying the reconcile Result blob with the group
			// id — otherwise the orphan is undiscoverable and the claim blocks the pair
			// with no record.
			if len(camps.upserted) != 1 {
				t.Fatalf("upserted %d campaigns, want 1 (the id-less orphan must be persisted, not dropped)", len(camps.upserted))
			}
			row := camps.existing["b1|"+string(model.ProviderLinkedInAds)]
			if row == nil {
				t.Fatal("expected a persisted orphan row")
			}
			// The retained orphan row was persisted with the DEGRADED status, not pending.
			if row.Status != status {
				t.Errorf("persisted status = %q, want the preserved %q (not flattened to pending)", row.Status, status)
			}
			// PlatformCampaignID stays empty (no campaign created), keeping the row out of
			// the id-keyed idempotency fast-path; the reconcile detail rides in Result.
			if row.PlatformCampaignID != "" {
				t.Errorf("orphan must have no upstream campaign id, got %q", row.PlatformCampaignID)
			}
			if !strings.Contains(string(row.Result), "campaignGroupId") {
				t.Errorf("orphan Result must carry the group id for reconciliation, got %q", row.Result)
			}
			// And the orphan is NOT reusable: isReusableCampaign must reject a
			// non-terminal status so the fast path can't report a false success — a later
			// dispatch then hits the retained claim and returns reconciliation-required
			// rather than reusing the orphan as a completed campaign.
			if isReusableCampaign(row) {
				t.Errorf("a %q orphan must NOT be reusable as a completed campaign", status)
			}
		})
	}
}

// TestOrchestrator_PreservesCreatedDegradedWithID verifies that a terminal
// created_degraded status returned ALONGSIDE an error but WITH a real upstream id (the
// campaign was created, only a sub-step degraded) is preserved on the persisted row —
// not flattened to "pending" — and remains reusable (a re-dispatch can't repair the
// degraded sub-step, so it must not needlessly re-create). Addresses PR #37 copilot.
func TestOrchestrator_PreservesCreatedDegradedWithID(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderLinkedInAds: degradedCreatedDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)

	row := camps.existing["b1|"+string(model.ProviderLinkedInAds)]
	if row == nil {
		t.Fatal("expected a persisted row for the created-degraded campaign")
	}
	if row.Status != "created_degraded" {
		t.Errorf("status = %q, want the preserved created_degraded (not flattened to pending)", row.Status)
	}
	if row.PlatformCampaignID == "" {
		t.Error("the created campaign id must be persisted")
	}
	// created_degraded WITH an id IS reusable — the campaign exists and can't be repaired
	// by a re-dispatch.
	if !isReusableCampaign(row) {
		t.Error("created_degraded with an id must be reusable (the campaign exists)")
	}
}

// TestIsReusableCampaign locks the reuse gate: a completed campaign (an id + a status
// that is neither the bare 'pending' claim nor a partial-orphan status) is reusable;
// every partial-orphan/claim status, and any id-less row, is not.
func TestIsReusableCampaign(t *testing.T) {
	cases := []struct {
		status string
		id     string
		want   bool
	}{
		{"created", "pc-1", true},
		{"created_degraded", "pc-1", true},
		{"active", "pc-1", true},         // any non-claim, non-orphan status + id is complete
		{"", "pc-1", true},               // legacy: an id with no explicit status is complete
		{"pending", "pc-1", false},       // a bare/retained claim with an id
		{"group_created", "pc-1", false}, // partial-orphan status is never reusable, even with an id
		{"unconfirmed", "", false},       // ambiguous, no id
		{"group_created", "", false},     // group-only orphan, no id
		{"created", "", false},           // terminal status but no id (never created)
	}
	for _, tc := range cases {
		got := isReusableCampaign(&model.Campaign{Status: tc.status, PlatformCampaignID: tc.id})
		if got != tc.want {
			t.Errorf("isReusableCampaign(status=%q,id=%q) = %v, want %v", tc.status, tc.id, got, tc.want)
		}
	}
	if isReusableCampaign(nil) {
		t.Error("nil campaign must not be reusable")
	}
}

func TestOrchestrator_NoDispatcherFails(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil) // no dispatchers
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
}

func TestOrchestrator_NilCampaignFailsWithoutPanic(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: nilDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (nil campaign must not persist)", len(camps.upserted))
	}
}

// countingDispatcher records how many times Dispatch is called, to prove the
// idempotency guard skips the upstream create.
type countingDispatcher struct {
	mu    sync.Mutex
	calls int
}

func (d *countingDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_SkipsAlreadyDispatchedPlatform verifies that a brief already
// carrying a campaign with an upstream id for a platform does NOT re-invoke the
// platform's create API (which would spend money on a duplicate).
func TestOrchestrator_SkipsAlreadyDispatchedPlatform(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {ID: "existing-c1", PlatformCampaignID: "pc-google-ads"},
	}}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (existing campaign must be reused)", calls)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (no re-create)", len(camps.upserted))
	}
	// The reuse path must report the upstream platform campaign id (not the DB
	// row id), so campaign_id means the same thing as on the create path.
	if !strings.Contains(string(j.Result), "pc-google-ads") {
		t.Errorf("result = %s, want it to carry the upstream campaign id pc-google-ads", j.Result)
	}
	if strings.Contains(string(j.Result), "existing-c1") {
		t.Errorf("result = %s, must not leak the DB row id existing-c1", j.Result)
	}
}

// TestOrchestrator_PendingOrphanWithIDIsNotAFastPathSuccess verifies the fast path
// does NOT report a retained partial orphan as a completed success. A mid-flow failure
// persists a row with a `pending` status AND a non-empty upstream id (recorded for
// reconciliation). A later CreateCampaigns must NOT short-circuit to success on that id
// alone — the orphan is distinguishable from a concurrent claim, so it is classified as
// a reconciliation-required FAILURE (not a skip that would let the job report succeeded),
// which is what this test asserts.
func TestOrchestrator_PendingOrphanWithIDIsNotAFastPathSuccess(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		// pending status WITH an upstream id — a retained orphan, not a completed campaign.
		"b1|" + string(model.ProviderGoogleAds): {ID: "c1", Status: "pending", PlatformCampaignID: "pc-orphan"},
	}}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	// A retained partial orphan (pending row WITH an upstream id) is distinguishable
	// from a concurrent claim, and NOTHING will revisit it — so the retry must NOT be
	// reported as successful. It is classified as a reconciliation-required FAILURE,
	// not a skip (a skip would let aggregateStatus mark an all-skipped job succeeded and
	// hide the orphan). Assert the job is not succeeded and the failure names the orphan
	// for reconciliation.
	if j.Status == model.JobSucceeded {
		t.Errorf("a retained orphan must not make the retry succeed; job=%s result=%s", j.Status, j.Result)
	}
	if !strings.Contains(string(j.Result), "reconciliation required") {
		t.Errorf("the result should flag the orphan for reconciliation, got: %s", j.Result)
	}
}

// TestOrchestrator_IDlessOrphanWithResultIsNotASkipSuccess covers the id-LESS orphan:
// an ambiguous create persists a pending row with an EMPTY PlatformCampaignID but a
// non-empty Result reconcile blob. On retry, ClaimCampaignDispatch returns not-claimed
// with that row; it must NOT be classified as a live concurrent SKIP (which would let
// aggregateStatus mark the retry job succeeded and hide the orphan) — it is a
// reconciliation-required FAILURE, distinguished from a bare claim by the Result blob.
func TestOrchestrator_IDlessOrphanWithResultIsNotASkipSuccess(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		// pending, NO upstream id, but carries a Result reconcile blob.
		"b1|" + string(model.ProviderGoogleAds): {ID: "c1", Status: "pending", PlatformCampaignID: "", Result: []byte(`{"campaignName":"orphan-name"}`)},
	}}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status == model.JobSucceeded {
		t.Errorf("an id-less orphan carrying a Result must not make the retry succeed; job=%s result=%s", j.Status, j.Result)
	}
	if !strings.Contains(string(j.Result), "reconciliation required") {
		t.Errorf("the result should flag the id-less orphan for reconciliation, got: %s", j.Result)
	}
}

// TestOrchestrator_ClaimErrorIsFailure verifies that a failure to claim the
// dispatch slot is recorded as a platform failure and the dispatcher is never
// called (so no create can duplicate).
func TestOrchestrator_ClaimErrorIsFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{claimErr: errors.New("db down")}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (must not create when the claim failed)", calls)
	}
}

// TestOrchestrator_IdempotencyLookupErrorIsFailure verifies that a REAL DB error
// from the idempotency lookup (GetCampaignByPlatform) — anything other than
// ErrNotFound — is surfaced as a platform failure and the dispatcher is never
// called. Otherwise a transient read failure would be treated like "no existing
// campaign" and dispatch could duplicate an existing-but-unloaded campaign.
func TestOrchestrator_IdempotencyLookupErrorIsFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{byPlatformErr: errors.New("db connection reset")}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (a lookup error must not fall through to dispatch)", calls)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (no create on a lookup error)", len(camps.upserted))
	}
}

// TestOrchestrator_AlreadyClaimedPendingSkips verifies that when another worker
// holds the pending claim (no upstream id yet), this worker does not dispatch and
// the skip is NOT recorded as a terminal failure: a single skipped platform is a
// deferral to the owning dispatch, so the job terminalizes as succeeded (not
// failed, which would be spurious; not left running, which the staleness sweeper
// would later fail), rather than the old behavior of falsely failing it.
func TestOrchestrator_AlreadyClaimedPendingSkips(t *testing.T) {
	jobs := newFakeJobRepo()
	// Seed a pending claim (no upstream id) for the pair, so ClaimCampaignDispatch
	// returns not-claimed with a still-pending row.
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {ID: "c1", Status: "pending", PlatformCampaignID: ""},
	}}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	// A skipped platform (owned by a concurrent dispatch) is a deferral, not a
	// failure — the job terminalizes as SUCCEEDED (not stuck-running, which the
	// recovery sweeper would later fail; not failed, which would be spurious).
	j := waitForFinalized(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded (a lone skipped platform is a deferral, terminalizes succeeded)", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (another worker holds the claim)", calls)
	}
	if !strings.Contains(string(j.Result), "another concurrent dispatch owns") {
		t.Errorf("result = %s, want a concurrent-owner skip message", j.Result)
	}
	if !strings.Contains(string(j.Result), "\"skipped\":true") {
		t.Errorf("result = %s, want the platform marked skipped:true", j.Result)
	}
}

// TestClaimCampaignDispatch_ConcurrentSingleWinner exercises the ACTUAL race the
// single-flight claim guards against: N goroutines calling ClaimCampaignDispatch
// for the SAME (brief, platform) at the same time. Exactly one must win
// (claimed=true) and every loser must cleanly observe claimed=false with no error
// and the SAME pending row — the ON CONFLICT (brief_id, platform) DO NOTHING
// arbitration the design leans on. The prior claim tests only pre-seed a claimed
// row and call Start once, so they never run two claimers concurrently.
func TestClaimCampaignDispatch_ConcurrentSingleWinner(t *testing.T) {
	// The fake repo models ON CONFLICT DO NOTHING under a mutex: first caller
	// inserts + returns claimed=true; every later caller sees the existing row and
	// returns claimed=false — the same arbitration Postgres provides.
	repo := &fakeCampaignRepo{}

	const n = 32
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		mu     sync.Mutex
		wins   int
		errs   int
		rowIDs = map[*model.Campaign]struct{}{}
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			claimed, row, err := repo.ClaimCampaignDispatch(
				context.Background(), "cncf", "b1", model.ProviderGoogleAds, model.VariantDefault, "job1", nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			if claimed {
				wins++
			}
			if row != nil {
				rowIDs[row] = struct{}{}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if errs != 0 {
		t.Errorf("got %d errors; every claimer (winner or loser) must return nil error", errs)
	}
	if wins != 1 {
		t.Errorf("exactly one goroutine must win the claim, got %d winners", wins)
	}
	if len(rowIDs) != 1 {
		t.Errorf("all claimers must observe the SAME pending row, got %d distinct rows", len(rowIDs))
	}
}

// TestOrchestrator_SkipDoesNotFailAlongsideSuccess verifies that when one
// platform succeeds and another is skipped (owned by a concurrent dispatch), the
// job is not falsely reported failed/partial: with a real success and no real
// failure, an outstanding skip (a deferral to the owner) terminalizes the job as
// succeeded rather than a spurious failure/partial or a stuck running state.
func TestOrchestrator_SkipDoesNotFailAlongsideSuccess(t *testing.T) {
	jobs := newFakeJobRepo()
	// LinkedIn is already held pending by another dispatch; Google Ads is free.
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderLinkedInAds): {ID: "c1", Status: "pending", PlatformCampaignID: ""},
	}}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	j := waitForFinalized(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded (a skip alongside a success terminalizes succeeded, not failed/partial/stuck)", j.Status)
	}
}

func TestAggregateStatus(t *testing.T) {
	cases := []struct {
		name    string
		results []platformResult
		want    model.JobStatus
	}{
		{"all ok", []platformResult{{OK: true}, {OK: true}}, model.JobSucceeded},
		{"all fail", []platformResult{{OK: false}, {OK: false}}, model.JobFailed},
		{"mixed", []platformResult{{OK: true}, {OK: false}}, model.JobPartial},
		// A single-flight SKIP is a deferral to the owning dispatch, not a failure
		// and not this job's work to finish: it terminalizes as succeeded (not stuck
		// running, which the sweeper would later fail).
		{"only skipped", []platformResult{{Skipped: true}}, model.JobSucceeded},
		{"skip + ok", []platformResult{{OK: true}, {Skipped: true}}, model.JobSucceeded},
		// A real failure still surfaces even when another platform was skipped.
		{"skip + fail", []platformResult{{OK: false}, {Skipped: true}}, model.JobPartial},
		{"ok + fail + skip", []platformResult{{OK: true}, {OK: false}, {Skipped: true}}, model.JobPartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateStatus(tc.results); got != tc.want {
				t.Errorf("aggregateStatus = %s, want %s", got, tc.want)
			}
		})
	}
}

// emptyIDDispatcher returns a non-nil campaign with no upstream id — a
// misbehaving dispatcher that must be recorded as a failure, not ok.
type emptyIDDispatcher struct{}

func (emptyIDDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "", Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_EmptyUpstreamIDIsFailure verifies a dispatched campaign with
// no PlatformCampaignID is reported as a failure (not ok) and not persisted.
func TestOrchestrator_EmptyUpstreamIDIsFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: emptyIDDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d, want 0 (empty upstream id must not persist)", len(camps.upserted))
	}
}

// TestOrchestrator_ReusesExistingWhenDispatcherGone verifies the idempotency
// guard runs before dispatcher resolution: an already-persisted platform is
// reported ok on retry even if its dispatcher is no longer registered.
func TestOrchestrator_ReusesExistingWhenDispatcherGone(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {ID: "c1", PlatformCampaignID: "pc-google-ads"},
	}}
	// No dispatchers registered at all.
	orch := NewOrchestrator(camps, jobs, nil)
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded (existing campaign reused despite no dispatcher)", j.Status)
	}
	if !strings.Contains(string(j.Result), "pc-google-ads") {
		t.Errorf("result = %s, want the reused upstream id", j.Result)
	}
}

// panicDispatcher panics — a misbehaving dispatcher that must not crash the
// process; the orchestrator must record it as a failure.
type panicDispatcher struct{}

func (panicDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	panic("boom in dispatcher")
}

// TestOrchestrator_RecoversFromDispatcherPanic verifies a panicking dispatcher
// is recovered and recorded as a failure rather than crashing the goroutine.
func TestOrchestrator_RecoversFromDispatcherPanic(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: panicDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	// The panic value must not leak into the client-facing result.
	if strings.Contains(string(j.Result), "boom in dispatcher") {
		t.Errorf("result leaked the panic value: %s", j.Result)
	}
}

// persistErrCampaignRepo fails UpsertCampaign with a raw DB-like error.
type persistErrCampaignRepo struct{ fakeCampaignRepo }

func (r *persistErrCampaignRepo) UpsertCampaign(context.Context, *model.Campaign, domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return nil, errors.New("pq: duplicate key value violates unique constraint \"campaigns_pkey\"")
}

// TestOrchestrator_PersistErrorIsSanitized verifies a raw persistence error is
// not surfaced verbatim in the client-facing job result.
func TestOrchestrator_PersistErrorIsSanitized(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &persistErrCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if strings.Contains(string(j.Result), "pq:") || strings.Contains(string(j.Result), "constraint") {
		t.Errorf("result leaked raw DB error: %s", j.Result)
	}
	// The message is sanitized but the upstream id is preserved so the orphaned
	// campaign isn't lost.
	if !strings.Contains(string(j.Result), "failed to record it") {
		t.Errorf("result = %s, want the sanitized message", j.Result)
	}
	if !strings.Contains(string(j.Result), "pc-google-ads") {
		t.Errorf("result = %s, want the upstream id preserved for reconciliation", j.Result)
	}
}

// claimCountingCampaignRepo records that each dispatch went through the claim.
type claimCountingCampaignRepo struct {
	fakeCampaignRepo
	cmu    sync.Mutex
	claims int
}

func (r *claimCountingCampaignRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, p model.Provider, variant, jobID string, by *model.Actor) (bool, *model.Campaign, error) {
	r.cmu.Lock()
	r.claims++
	r.cmu.Unlock()
	// Forward the RECEIVED variant, not a hardcoded default. A wrapper that substitutes
	// the slot key would claim the default slot for every dispatch routed through it and
	// hide exactly the variant-routing regression this PR exists to prevent — the same
	// "a fake that does not model the key hides the bug" class the PR argues elsewhere.
	return r.fakeCampaignRepo.ClaimCampaignDispatch(ctx, projectID, briefID, p, variant, jobID, by)
}

// TestOrchestrator_DispatchGoesThroughClaim verifies each per-platform dispatch
// claims the (brief, platform) single-flight slot.
func TestOrchestrator_DispatchGoesThroughClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &claimCountingCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)
	camps.cmu.Lock()
	defer camps.cmu.Unlock()
	if camps.claims != 2 {
		t.Errorf("ClaimCampaignDispatch called %d times, want 2 (one per platform)", camps.claims)
	}
}

// blockingDispatcher blocks until released, to test shutdown draining.
type blockingDispatcher struct {
	started chan struct{}
	release chan struct{}
}

func (d *blockingDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	close(d.started)
	<-d.release
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_ShutdownDrainsInFlight verifies Shutdown waits for an
// in-flight dispatch to finish before returning.
func TestOrchestrator_ShutdownDrainsInFlight(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	disp := &blockingDispatcher{started: make(chan struct{}), release: make(chan struct{})}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started // dispatch is now in-flight

	shutdownReturned := make(chan error, 1)
	go func() {
		shutdownReturned <- orch.Shutdown(context.Background(), 5*time.Second)
	}()

	// Shutdown must NOT return while the dispatch is blocked.
	select {
	case <-shutdownReturned:
		t.Fatal("Shutdown returned before in-flight dispatch finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(disp.release) // let dispatch complete
	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after dispatch completed")
	}
}

// TestOrchestrator_ShutdownGraceHonorsContextCancel verifies that during the
// post-cancel grace wait, a caller CANCEL of ctx (not just its deadline) ends
// the wait promptly instead of blocking the full CancelGracePeriod.
func TestOrchestrator_ShutdownGraceHonorsContextCancel(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	<-ctxSeen

	// A cancelable ctx with no deadline; the dispatch never releases, so Shutdown
	// enters the grace wait. Cancelling ctx must unblock it well before the full
	// CancelGracePeriod.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(ctx, 10*time.Millisecond) }()

	// Let the drain window elapse so Shutdown is in the grace wait, then cancel.
	time.Sleep(40 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after ctx cancel during grace")
	}
	if elapsed := time.Since(start); elapsed >= CancelGracePeriod {
		t.Errorf("grace wait took %v after cancel; did not observe ctx cancellation", elapsed)
	}
	close(disp.release)
}

// TestOrchestrator_RecoverySweeperStopsOnShutdown verifies the background
// recovery sweeper is tracked by the wait group and stops promptly on Shutdown
// (it must not block the drain until a ticker fires), so Shutdown returns
// quickly with no in-flight dispatch.
func TestOrchestrator_RecoverySweeperStopsOnShutdown(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil)
	orch.StartRecoverySweeper()

	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(context.Background(), 5*time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return promptly; sweeper likely blocked the drain")
	}
	// The sweeper interval (5m) is far longer than this test, so FailStuckJobs
	// must not have been called by a tick — it stopped on the stop signal.
	if c := jobs.failStuckCallCount(); c != 0 {
		t.Errorf("FailStuckJobs called %d times; sweeper should have stopped before any tick", c)
	}
}

// TestOrchestrator_StartRejectedAfterShutdown verifies Start refuses new work
// once Shutdown has been initiated.
func TestOrchestrator_StartRejectedAfterShutdown(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	if err := orch.Shutdown(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	if _, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil); err == nil {
		t.Fatal("expected Start to be rejected after Shutdown")
	}
}

// TestFailStuckJobs verifies the recovery scan fails only non-terminal jobs.
func TestFailStuckJobs(t *testing.T) {
	jobs := newFakeJobRepo()
	jobs.jobs["j-queued"] = &model.CampaignJob{ID: "j-queued", Status: model.JobQueued}
	jobs.jobs["j-running"] = &model.CampaignJob{ID: "j-running", Status: model.JobRunning}
	jobs.jobs["j-done"] = &model.CampaignJob{ID: "j-done", Status: model.JobSucceeded}

	n, err := jobs.FailStuckJobs(context.Background(), "restarted")
	if err != nil {
		t.Fatalf("FailStuckJobs: %v", err)
	}
	if n != 2 {
		t.Errorf("failed %d jobs, want 2 (queued+running)", n)
	}
	if jobs.jobs["j-done"].Status != model.JobSucceeded {
		t.Errorf("terminal job was altered: %s", jobs.jobs["j-done"].Status)
	}
	if jobs.jobs["j-queued"].Status != model.JobFailed || jobs.jobs["j-running"].Status != model.JobFailed {
		t.Error("non-terminal jobs were not failed")
	}
}

// TestOrchestrator_ShutdownCancelsOnTimeout verifies that when the drain deadline
// expires, Shutdown cancels the in-flight run's context (rather than leaving it
// running against a closing pool).
func TestOrchestrator_ShutdownCancelsOnTimeout(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	dctx := <-ctxSeen

	// Drain with an already-past deadline so Shutdown times out immediately and
	// cancels the run context.
	deadctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = orch.Shutdown(deadctx, time.Millisecond)

	select {
	case <-dctx.Done():
		// good: the dispatch context was cancelled by Shutdown's timeout path.
	case <-time.After(time.Second):
		t.Error("dispatch context was not cancelled after drain timeout")
	}
	close(disp.release)
}

// TestOrchestrator_ShutdownGraceBoundedByContext verifies that when the drain
// deadline elapses while a dispatch is still stuck, the post-cancel grace wait
// does not exceed the caller's context budget (it must not add a full,
// wall-clock CancelGracePeriod on top of an already-expired deadline).
func TestOrchestrator_ShutdownGraceBoundedByContext(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	<-ctxSeen // drain the captured ctx so Dispatch can proceed to <-release

	// Short drain budget; the dispatch never releases, so Shutdown must hit the
	// deadline path and then wait at most the remaining budget for the grace,
	// NOT the full CancelGracePeriod (which is >> this budget).
	const budget = 50 * time.Millisecond
	deadctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_ = orch.Shutdown(deadctx, time.Millisecond)
	elapsed := time.Since(start)

	// Allow generous slack for scheduling, but it must be far below the full
	// wall-clock CancelGracePeriod that the old (unbounded) timer would impose.
	if elapsed >= CancelGracePeriod {
		t.Errorf("Shutdown waited %v (>= full CancelGracePeriod %v); grace not bounded by context", elapsed, CancelGracePeriod)
	}
	close(disp.release)
}

// TestOrchestrator_ShutdownGivesGraceWhenBudgetRemains verifies the two phases
// are budgeted separately: when the drain window elapses but the OUTER ctx still
// has budget, Shutdown actually spends a post-cancel grace waiting for the
// cancelled dispatch to unwind — it must NOT return immediately (which would let
// Container.Close close the pool mid-finalize). This guards the regression where
// Close passed a ctx limited to only the drain timeout, leaving zero grace.
func TestOrchestrator_ShutdownGivesGraceWhenBudgetRemains(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	dctx := <-ctxSeen // the dispatch's own context, cancelled by rootCancel

	// Outer budget comfortably exceeds the tiny drain window, so after drain
	// times out there is real grace budget left.
	outerCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	graceObserved := make(chan struct{})
	go func() {
		// The dispatch releases only once it observes its context cancellation —
		// i.e. during the grace phase, proving grace actually ran.
		<-dctx.Done()
		close(graceObserved)
		close(disp.release)
	}()

	start := time.Now()
	_ = orch.Shutdown(outerCtx, 20*time.Millisecond)
	elapsed := time.Since(start)

	select {
	case <-graceObserved:
	default:
		t.Fatal("dispatch context was never cancelled; grace phase did not run")
	}
	// Shutdown must have waited past the drain window (the grace phase happened),
	// but well within the outer budget.
	if elapsed < 20*time.Millisecond {
		t.Errorf("Shutdown returned in %v, before the drain window elapsed; grace phase was skipped", elapsed)
	}
	if elapsed >= time.Second {
		t.Errorf("Shutdown waited %v, at/over the full outer budget", elapsed)
	}
}

type ctxCapturingDispatcher struct {
	started chan struct{}
	release chan struct{}
	ctxSeen chan context.Context
}

func (d *ctxCapturingDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.ctxSeen <- ctx
	close(d.started)
	<-d.release
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_NoDispatcherDoesNotLeavePendingClaim verifies that when no
// dispatcher is registered, no pending claim row is left behind (which would
// permanently block the pair).
func TestOrchestrator_NoDispatcherDoesNotLeavePendingClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil) // no dispatchers
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	waitForTerminal(t, jobs, id)
	camps.mu.Lock()
	defer camps.mu.Unlock()
	// No claim should have been inserted (dispatcher checked first), so existing
	// is empty and no pending row blocks the pair.
	if _, ok := camps.existing["b1|"+string(model.ProviderGoogleAds)]; ok {
		t.Error("a pending claim row was left behind for a platform with no dispatcher")
	}
}

// preCreateErrDispatcher fails with an error that signals no upstream create.
type preCreateErr struct{}

func (preCreateErr) Error() string          { return "invalid input" }
func (preCreateErr) NoUpstreamCreate() bool { return true }

type preCreateErrDispatcher struct{}

func (preCreateErrDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, preCreateErr{}
}

// TestOrchestrator_PreCreateErrorReleasesClaim verifies that a dispatcher error
// signalling no-upstream-create releases the claim (so the pair can be retried),
// unlike an ambiguous error which retains it.
func TestOrchestrator_PreCreateErrorReleasesClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: preCreateErrDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	waitForTerminal(t, jobs, id)
	camps.mu.Lock()
	defer camps.mu.Unlock()
	// The pre-create error should have released the pending claim.
	if _, ok := camps.existing["b1|"+string(model.ProviderGoogleAds)]; ok {
		t.Error("pre-create dispatcher error should have released the pending claim")
	}
}

// partialResultDispatcher exercises the platform clients' partial-result
// contract: it returns a non-nil campaign carrying the created upstream id
// ALONGSIDE a (non-pre-create) error, as reddit/twitter clients do when the
// campaign POST succeeded but a later step failed.
type partialResultDispatcher struct{}

func (partialResultDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "pc-orphan-" + string(p), Status: "active", CampaignName: "n"},
		errors.New("ad group creation failed after campaign was created")
}

// TestOrchestrator_PartialDispatchErrorPersistsUpstreamID verifies that when
// Dispatch returns a partial campaign (a created upstream id) together with an
// error, the retained pending row is stamped with that upstream id so the
// orphaned upstream campaign is reconcilable — and the claim is NOT released.
func TestOrchestrator_PartialDispatchErrorPersistsUpstreamID(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: partialResultDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	camps.mu.Lock()
	defer camps.mu.Unlock()
	// The claim must be RETAINED (not released) — the upstream campaign may exist.
	row, ok := camps.existing["b1|"+string(model.ProviderGoogleAds)]
	if !ok {
		t.Fatal("pending claim should be retained after a partial dispatch error, not released")
	}
	// The retained row must now carry the orphaned upstream id (reconcilable) and
	// remain 'pending' (a recoverable orphan, not a completed campaign).
	if row.PlatformCampaignID != "pc-orphan-"+string(model.ProviderGoogleAds) {
		t.Errorf("retained row PlatformCampaignID = %q, want the orphaned upstream id", row.PlatformCampaignID)
	}
	if row.Status != "pending" {
		t.Errorf("retained row Status = %q, want pending (recoverable orphan)", row.Status)
	}
	// The partial campaign must have been persisted via UpsertCampaign.
	if len(camps.upserted) != 1 {
		t.Errorf("upserted %d campaigns, want 1 (partial upstream id persisted)", len(camps.upserted))
	}
}

// preCreateWithPartialDispatcher returns a NON-NIL campaign ALONGSIDE an error that
// claims NoUpstreamCreate. That pairing is self-contradictory: the error disowns the
// create while the campaign is evidence something was built for an upstream resource.
//
// This is the fixture shape the release guard must EXCLUDE, which is the whole point of
// writing it. PR #125's reap predicate was "verified" only against fixtures matching its
// own premise, so the test could never contradict the belief under test. A guard whose
// safety argument is a predicate is only pinned by a fixture that the predicate must
// refuse — so this dispatcher deliberately produces the disagreement, and the assertions
// below demand the claim survive it.
//
// withID toggles between the two real partial shapes: an id-carrying partial, and the
// id-less group-orphan whose id is empty BY DESIGN while a paid resource exists upstream.
// Both must retain; keying the guard on the id would release the second one.
type preCreateWithPartialDispatcher struct{ withID bool }

func (d preCreateWithPartialDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	c := &model.Campaign{
		Status:       "unconfirmed",
		Result:       json.RawMessage(`{"campaignGroupId":"g1"}`),
		CampaignName: "n",
	}
	if d.withID {
		c.PlatformCampaignID = "pc-orphan-" + string(p)
	}
	return c, preCreateErr{}
}

// TestOrchestrator_PreCreateErrorWithPartialRetainsClaim pins the independent
// `campaign == nil` precondition on releasing a dispatch claim.
//
// Releasing here would delete the pending row and free the (brief, platform) slot, so the
// next dispatch would create a SECOND paid campaign for a brief that already has one. The
// dispatcher's NoUpstreamCreate is only its own assertion about its error; a non-nil
// campaign is evidence from the same return that contradicts it, and the money-losing
// direction is to believe the assertion.
//
// MUTATION CHECK (verified, compiling): reverting the guard to `if
// dispatchErrIsPreCreate(derr) {` fails both subtests — the claim is released and the row
// vanishes. Narrowing it to `campaign == nil || campaign.PlatformCampaignID == ""` still
// compiles and still fails id_less, which is why that subtest exists separately.
func TestOrchestrator_PreCreateErrorWithPartialRetainsClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		withID bool
	}{
		{"id_carrying_partial", true},
		// The decisive case: the id is empty by design, so a guard keyed on the id
		// would route this straight back into the release path.
		{"id_less_group_orphan", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := newFakeJobRepo()
			camps := &fakeCampaignRepo{}
			orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
				model.ProviderGoogleAds: preCreateWithPartialDispatcher{withID: tc.withID},
			})
			brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
			id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if j := waitForTerminal(t, jobs, id); j.Status != model.JobFailed {
				t.Errorf("status = %s, want failed", j.Status)
			}

			camps.mu.Lock()
			defer camps.mu.Unlock()
			row, ok := camps.existing[slotKey("b1", model.ProviderGoogleAds, model.VariantDefault)]
			if !ok {
				t.Fatal("claim was RELEASED after a pre-create error carrying a non-nil campaign; " +
					"the freed slot lets the next dispatch create a duplicate PAID campaign")
			}
			// Non-terminal, so a retry reconciles instead of reading it as a success.
			if row.Status != "pending" && row.Status != "unconfirmed" {
				t.Errorf("retained row Status = %q, want a non-terminal reconcilable status", row.Status)
			}
			// The orphan must be RECORDED, not just blocked — an anonymous claim is
			// indistinguishable from a live concurrent dispatch on a later retry.
			if len(camps.upserted) != 1 {
				t.Fatalf("upserted %d campaigns, want 1 (the partial must be persisted to be reconcilable)", len(camps.upserted))
			}
			if len(camps.upserted[0].Result) == 0 {
				t.Error("persisted partial carries no Result blob, so the orphan is not reconcilable")
			}
		})
	}
}

// gatedDispatcher holds the claim WINNER inside Dispatch until the test releases it.
//
// It is NOT an n-party rendezvous, and deliberately so: the single-flight claim means only
// ONE caller ever reaches Dispatch for a given (brief, platform) — the losers are skipped
// before the provider is called. A barrier sized to N could therefore never be satisfied by
// arrivals, and a test that appeared to wait for N of them would in truth be waiting for
// nothing. What this gate buys is real: it pins the winner INSIDE the provider call while
// the losing goroutines run their claim attempts, so the losers observe a live 'pending'
// row rather than a race that has already resolved. That is the ordering the skip path is
// specified against.
type gatedDispatcher struct {
	// entered is closed by the winner on arrival, so the test can wait for the dispatch
	// to be genuinely in flight rather than sleeping and hoping.
	entered chan struct{}
	// release gates the winner's return until the test opens it.
	release chan struct{}

	mu      sync.Mutex
	arrived int
}

// arrivals reports how many callers reached Dispatch. The single-flight claim makes the
// expected answer exactly 1, and the test asserts that rather than assuming it.
func (d *gatedDispatcher) arrivals() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.arrived
}

func (d *gatedDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	d.arrived++
	first := d.arrived == 1
	d.mu.Unlock()
	if first {
		close(d.entered)
	}
	<-d.release
	return &model.Campaign{
		Status:       "unconfirmed",
		Result:       json.RawMessage(`{"campaignGroupId":"g1"}`),
		CampaignName: "n",
	}, preCreateErr{}
}

// TestOrchestrator_ConcurrentPreCreatePartialsKeepOneClaim proves the retain guard holds on
// the CONTENDED path: while one dispatch sits inside the provider call and will come back
// with the contradictory pairing, N-1 concurrent dispatches for the same (brief, platform)
// attempt the claim and must be SKIPPED — and none of them, nor the winner, may release the
// row.
//
// The losers are the point. They run their claim attempt against a live 'pending' row (the
// gate holds the winner in Dispatch until they have), so this exercises the skip path that
// the serial test cannot reach. With the guard reverted, the winner's release deletes the
// shared row and reopens the slot for a duplicate PAID create.
//
// MUTATION CHECK (verified, compiling): reverting the guard to `if
// dispatchErrIsPreCreate(derr) {` fails this test.
func TestOrchestrator_ConcurrentPreCreatePartialsKeepOneClaim(t *testing.T) {
	const parties = 4

	d := &gatedDispatcher{entered: make(chan struct{}), release: make(chan struct{})}

	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: d,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}

	ids := make([]string, 0, parties)
	var mu sync.Mutex
	var starters sync.WaitGroup
	start := func() {
		defer starters.Done()
		id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
		if err != nil {
			return
		}
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()
	}

	// Launch the winner first and WAIT for it to be inside Dispatch. Without this the
	// losers could all run before any claim exists, and the test would degenerate into
	// the serial case that TestOrchestrator_PreCreateErrorWithPartialRetainsClaim
	// already covers.
	starters.Add(1)
	go start()
	select {
	case <-d.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch reached the provider")
	}

	for range parties - 1 {
		starters.Add(1)
		go start()
	}
	starters.Wait()

	// Start returning is NOT the loser finishing: dispatch runs asynchronously, so a
	// loser's skip is recorded after its Start has already returned. Wait for every
	// LOSER's job to finalize BEFORE releasing the winner — that is what guarantees each
	// loser resolved its claim attempt against the still-held 'pending' row rather than
	// against a slot the winner had already settled. (The winner cannot finalize yet; it
	// is parked in Dispatch, so it is excluded here and waited for after the release.)
	mu.Lock()
	launched := append([]string(nil), ids...)
	mu.Unlock()
	if len(launched) != parties {
		t.Fatalf("Start succeeded for %d of %d parties; the contended path needs all of them", len(launched), parties)
	}
	// Poll until exactly one job remains unfinalized. That one is the winner, parked in
	// Dispatch behind the gate; every other job has recorded its skip. Polling rather
	// than sampling once is what makes this settled instead of racing — a loser that has
	// merely returned from Start has not yet written its result.
	deadline := time.Now().Add(5 * time.Second)
	var pending []string
	for time.Now().Before(deadline) {
		pending = pending[:0]
		for _, id := range launched {
			if j, _ := jobs.GetJob(context.Background(), "", id); len(j.Result) == 0 {
				pending = append(pending, id)
			}
		}
		if len(pending) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("jobs still running = %d, want exactly 1 (the claim winner held in Dispatch)", len(pending))
	}

	// Every claim attempt has now resolved against the held row, so let the winner finish.
	close(d.release)

	for _, id := range launched {
		waitForFinalized(t, jobs, id)
	}

	if got := d.arrivals(); got != 1 {
		t.Errorf("dispatches that reached the provider = %d, want 1: the single-flight claim "+
			"must skip the losers rather than re-dispatching them", got)
	}

	// The losers must be recorded as SKIPPED, which is what makes this the contended
	// path rather than a second serial run. The count is compared against a literal, not
	// against `parties`, so shrinking `parties` cannot quietly turn this into the serial
	// case that TestOrchestrator_PreCreateErrorWithPartialRetainsClaim already covers —
	// it fails instead.
	var skipped int
	for _, id := range ids {
		j, _ := jobs.GetJob(context.Background(), "", id)
		var results []platformResult
		if err := json.Unmarshal(j.Result, &results); err != nil {
			t.Fatalf("decode job result: %v", err)
		}
		for _, r := range results {
			if r.Skipped {
				skipped++
			}
		}
	}
	const wantSkipped = 3 // parties-1 losers, pinned as a literal on purpose (see above)
	if skipped != wantSkipped {
		t.Errorf("skipped platform results = %d, want %d (one per claim loser); "+
			"without losers this test degenerates into the serial case", skipped, wantSkipped)
	}

	camps.mu.Lock()
	defer camps.mu.Unlock()
	if _, ok := camps.existing[slotKey("b1", model.ProviderGoogleAds, model.VariantDefault)]; !ok {
		t.Fatal("no claim survived the concurrent pre-create partials; the slot is free and " +
			"the next dispatch will create a duplicate PAID campaign")
	}
}

// blockingSweepJobRepo blocks inside FailStuckJobs until its context is
// cancelled, letting a test prove that cancelling the sweeper's context
// interrupts an in-flight sweep promptly (rather than the sweep running to its
// own timeout against a closing pool).
type blockingSweepJobRepo struct {
	fakeJobRepo
	entered chan struct{}
}

func (r *blockingSweepJobRepo) FailStuckJobs(ctx context.Context, _ string) (int64, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	<-ctx.Done() // block until the sweeper's context is cancelled
	return 0, ctx.Err()
}

// TestOrchestrator_SweeperInterruptedOnShutdown verifies that a sweep already
// blocked in the DB is interrupted PROMPTLY when Shutdown cancels the sweeper's
// dedicated context, and that Shutdown still completes within budget. Uses a
// tiny sweep interval so a sweep starts quickly, and a repo whose FailStuckJobs
// blocks until its context is cancelled.
func TestOrchestrator_SweeperInterruptedOnShutdown(t *testing.T) {
	jobs := &blockingSweepJobRepo{
		fakeJobRepo: fakeJobRepo{jobs: map[string]*model.CampaignJob{}},
		entered:     make(chan struct{}, 1),
	}
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil)
	// Drive the sweeper on a very short interval so a sweep begins promptly. The
	// sweeper reads sweeperCtx, so overriding the interval doesn't affect the
	// cancellation path under test.
	orch.sweeperCtx, orch.sweeperCancel = context.WithCancel(context.Background())
	orch.wg.Add(1)
	go func() {
		defer orch.wg.Done()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-orch.sweeperCtx.Done():
				return
			case <-ticker.C:
				sctx, cancel := context.WithTimeout(orch.sweeperCtx, jobFinalizeTimeout)
				_, _ = jobs.FailStuckJobs(sctx, "x")
				cancel()
			}
		}
	}()

	// Wait until a sweep is actually blocked inside FailStuckJobs.
	select {
	case <-jobs.entered:
	case <-time.After(time.Second):
		t.Fatal("sweep never started")
	}

	// Shutdown must cancel the sweeper's context (interrupting the blocked sweep)
	// and return quickly — well under the jobFinalizeTimeout the sweep would
	// otherwise wait for.
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- orch.Shutdown(context.Background(), 5*time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return; blocked sweep was not interrupted")
	}
	if elapsed := time.Since(start); elapsed >= jobFinalizeTimeout {
		t.Errorf("Shutdown took %v (>= jobFinalizeTimeout %v); sweep was not interrupted promptly", elapsed, jobFinalizeTimeout)
	}
}

// ctxAssertingCampaignRepo asserts UpsertCampaign is invoked with a live (not
// cancelled) context — proving the post-provider persist runs on a context
// detached from the cancelled dispatch context.
type ctxAssertingCampaignRepo struct {
	fakeCampaignRepo
	upsertCtxErr error // context error observed inside UpsertCampaign
	upsertCalled chan struct{}
}

func (r *ctxAssertingCampaignRepo) UpsertCampaign(ctx context.Context, c *model.Campaign, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	r.mu.Lock()
	r.upsertCtxErr = ctx.Err()
	r.mu.Unlock()
	// Pass the builder THROUGH rather than dropping it: swallowing it here would hide whether
	// a shutdown-window persist still co-commits its index message.
	got, err := r.fakeCampaignRepo.UpsertCampaign(ctx, c, indexPayload)
	// Signalled AFTER the embedded repo has recorded the row, not before. The waiting test
	// reads len(upserted) the moment this fires; closing first made that a race it usually
	// won on an idle laptop and lost on a loaded CI runner, reporting "persisted 0 campaigns"
	// against an implementation that was persisting correctly.
	close(r.upsertCalled)
	return got, err
}

// TestOrchestrator_PersistSurvivesDispatchCancel verifies that a provider result
// completing AFTER the dispatch context is cancelled (the phase-two shutdown
// grace) is still persisted: the upsert must run on a detached context, not the
// cancelled dispatch context, so the record of the created upstream campaign is
// not lost.
func TestOrchestrator_PersistSurvivesDispatchCancel(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &ctxAssertingCampaignRepo{upsertCalled: make(chan struct{})}
	ctxSeen := make(chan context.Context, 1)
	// The dispatcher returns its campaign only after observing cancellation, so the
	// persist step necessarily runs while the dispatch ctx is already cancelled.
	disp := &cancelThenReturnDispatcher{ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-ctxSeen // dispatch is in-flight

	// Drain with an already-past deadline so Shutdown immediately cancels the run's
	// context, but give a real outer budget so the grace phase lets it finish.
	outerCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = orch.Shutdown(outerCtx, time.Millisecond) }()

	// The upsert must be reached and must see a LIVE context (detached), then the
	// job reaches a terminal state with the campaign persisted.
	select {
	case <-camps.upsertCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("UpsertCampaign was never called; persist did not survive dispatch cancel")
	}
	camps.mu.Lock()
	upsertErr := camps.upsertCtxErr
	upsertCount := len(camps.upserted)
	camps.mu.Unlock()
	if upsertErr != nil {
		t.Errorf("UpsertCampaign ran on a cancelled context (%v); persist must use a detached context", upsertErr)
	}
	if upsertCount != 1 {
		t.Errorf("persisted %d campaigns, want 1 (created upstream campaign must be recorded)", upsertCount)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
}

// cancelThenReturnDispatcher waits until its context is cancelled, then returns a
// successful campaign — forcing the orchestrator's persist step to run while the
// dispatch context is already cancelled.
type cancelThenReturnDispatcher struct {
	ctxSeen chan context.Context
}

func (d *cancelThenReturnDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.ctxSeen <- ctx
	<-ctx.Done() // return only after Shutdown cancels the dispatch context
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestBriefETagIsQuoted verifies the emitted ETag is a quoted entity-tag.
func TestBriefETagIsQuoted(t *testing.T) {
	if got := briefETag(3); got != `"3"` {
		t.Errorf("briefETag(3) = %q, want \"3\"", got)
	}
	// And the parser round-trips it.
	v, err := parseBriefIfMatch(strPtr(briefETag(7)))
	if err != nil || v != 7 {
		t.Errorf("round-trip of briefETag(7) = %d, %v; want 7, nil", v, err)
	}
}

// metricsOnlyDispatcher implements PlatformDispatcher + MetricsReader, recording the
// ReadMetrics call. Dispatch is never expected to be exercised by these tests.
type metricsOnlyDispatcher struct {
	metrics     *model.CampaignMetrics
	err         error
	gotWindow   model.MetricsWindow
	gotDeadline time.Time // deadline extracted from the context passed to ReadMetrics
}

func (metricsOnlyDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch should not be called in these tests")
}

func (d *metricsOnlyDispatcher) ReadMetrics(ctx context.Context, _ string, _ model.Provider, _ *model.Campaign, window model.MetricsWindow) (*model.CampaignMetrics, error) {
	d.gotWindow = window
	// Record the deadline to verify the caller enforced a timeout.
	if deadline, ok := ctx.Deadline(); ok {
		d.gotDeadline = deadline
	}
	return d.metrics, d.err
}

// nonMetricsDispatcher implements only PlatformDispatcher — no MetricsReader — the same
// shape as a platform with no metrics-read capability wired (e.g. hubspot today).
type nonMetricsDispatcher struct{}

func (nonMetricsDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch should not be called in these tests")
}

func TestOrchestrator_ReadCampaignMetrics_HappyPath(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &metricsOnlyDispatcher{metrics: &model.CampaignMetrics{
		CampaignID: "555", Window: model.MetricsWindowLast30Days, Impressions: 1000, Clicks: 40, Ctr: 0.04,
	}}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	campaign := &model.Campaign{PlatformCampaignID: "555"}

	m, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, campaign, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadCampaignMetrics: %v", err)
	}
	if m.Impressions != 1000 || m.Clicks != 40 {
		t.Errorf("got %+v", m)
	}
	if disp.gotWindow != model.MetricsWindowLast30Days {
		t.Errorf("window passed to dispatcher = %q, want last_30_days", disp.gotWindow)
	}
}

func TestOrchestrator_ReadCampaignMetrics_NotProvisioned(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &metricsOnlyDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})

	if _, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, &model.Campaign{}, model.MetricsWindowLast30Days); !errors.Is(err, ErrCampaignNotProvisioned) {
		t.Errorf("err = %v, want ErrCampaignNotProvisioned", err)
	}
	if _, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, nil, model.MetricsWindowLast30Days); !errors.Is(err, ErrCampaignNotProvisioned) {
		t.Errorf("err = %v, want ErrCampaignNotProvisioned", err)
	}
}

func TestOrchestrator_ReadCampaignMetrics_NoDispatcherRegistered(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{})

	campaign := &model.Campaign{PlatformCampaignID: "555"}
	if _, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, campaign, model.MetricsWindowLast30Days); !errors.Is(err, ErrMetricsUnsupported) {
		t.Errorf("err = %v, want ErrMetricsUnsupported", err)
	}
}

func TestOrchestrator_ReadCampaignMetrics_DispatcherNotAMetricsReader(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: nonMetricsDispatcher{},
	})

	campaign := &model.Campaign{PlatformCampaignID: "555"}
	if _, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, campaign, model.MetricsWindowLast30Days); !errors.Is(err, ErrMetricsUnsupported) {
		t.Errorf("err = %v, want ErrMetricsUnsupported", err)
	}
}

func TestOrchestrator_ReadCampaignMetrics_DispatcherErrorPropagates(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	wantErr := errors.New("boom")
	disp := &metricsOnlyDispatcher{err: wantErr}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})

	campaign := &model.Campaign{PlatformCampaignID: "555"}
	if _, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, campaign, model.MetricsWindowLast30Days); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestOrchestrator_ReadCampaignMetrics_NilNilReaderResultIsError(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &metricsOnlyDispatcher{} // metrics == nil, err == nil
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})

	campaign := &model.Campaign{PlatformCampaignID: "555"}
	m, err := orch.ReadCampaignMetrics(context.Background(), "proj-1", model.ProviderGoogleAds, campaign, model.MetricsWindowLast30Days)
	if err == nil {
		t.Fatal("expected an error when the MetricsReader returns (nil, nil), got nil")
	}
	if m != nil {
		t.Errorf("expected a nil result alongside the error, got %+v", m)
	}
}

func TestOrchestrator_ReadCampaignMetrics_EnforcesCallTimeout(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &metricsOnlyDispatcher{metrics: &model.CampaignMetrics{
		CampaignID: "555", Window: model.MetricsWindowLast30Days, Impressions: 100, Clicks: 5, Ctr: 0.05,
	}}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	campaign := &model.Campaign{PlatformCampaignID: "555"}

	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bracket the call itself, not just the assertion, so the tolerance window covers exactly
	// the wall-clock span the deadline could have been derived from — computing it only after
	// the call let slow CI scheduling between the call and the check widen the window without
	// actually loosening the tolerance on the deadline's derivation.
	beforeCall := time.Now()
	_, err := orch.ReadCampaignMetrics(callCtx, "proj-1", model.ProviderGoogleAds, campaign, model.MetricsWindowLast30Days)
	afterCall := time.Now()
	if err != nil {
		t.Fatalf("ReadCampaignMetrics: %v", err)
	}

	// Verify the dispatcher received a context with a deadline approximately metricsCallTimeout.
	if disp.gotDeadline.IsZero() {
		t.Error("dispatcher did not receive a context with a deadline")
	}

	// The deadline should be within [beforeCall, afterCall] + metricsCallTimeout (20 seconds).
	expectedMinDeadline := beforeCall.Add(20 * time.Second)
	expectedMaxDeadline := afterCall.Add(20 * time.Second)
	if disp.gotDeadline.Before(expectedMinDeadline) || disp.gotDeadline.After(expectedMaxDeadline) {
		t.Errorf("deadline %v not within [%v, %v] (beforeCall/afterCall + 20s)",
			disp.gotDeadline, expectedMinDeadline, expectedMaxDeadline)
	}
}

// TestOrchestrator_CampaignIndexCoCommitsWithoutACallerToken pins that a created campaign is
// indexed via the OUTBOX, carrying no caller credential.
//
// Campaign creation is ASYNC: the dispatch runs on the orchestrator's root context, long after
// the request returned. Publishing directly with the JWT captured at Start meant a slow dispatch
// could publish with an EXPIRED token — which the indexer rejects — and with no outbox row there
// was nothing to retry, so the campaign stayed permanently unsearchable. Co-committing removes
// both the expiry dependency and the single-shot delivery.
func TestOrchestrator_CampaignIndexCoCommitsWithoutACallerToken(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	// A real (non-Noop) publisher: an orchestrator left on the default Noop deliberately
	// enqueues nothing (the NATS_URL="" path), which is not what this test is about.
	orch.SetIndexer(&failingIndexer{})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}

	id, _ := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds}, nil)
	waitForTerminal(t, jobs, id)

	camps.mu.Lock()
	defer camps.mu.Unlock()
	if len(camps.indexPayloads) != 1 {
		t.Fatalf("a created campaign must co-commit exactly one index message, got %d", len(camps.indexPayloads))
	}

	var msg indexer.Transaction
	if err := json.Unmarshal(camps.indexPayloads[0], &msg); err != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", err)
	}
	if msg.Action != indexer.ActionCreated {
		t.Errorf("action = %q, want %q", msg.Action, indexer.ActionCreated)
	}
	// The stored payload must carry NO credential. The caller's token would be expired by the
	// time a delayed relay pass publishes it, and the outbox is JSONB retained for audit with
	// no pruning — so writing a live JWT there would persist it indefinitely. The relay stamps
	// a service credential at publish time instead.
	if auth := msg.Headers["authorization"]; auth != "" {
		t.Errorf("stored payload carries an authorization header (%q); the relay must stamp it at publish time", auth)
	}
}

// nilResultAccountLister is an AccountLister that returns (nil, nil) — the contract
// violation ReadAccounts has to convert into an error.
type nilResultAccountLister struct{}

func (nilResultAccountLister) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (nilResultAccountLister) ListAccounts(_ context.Context, _ string, _ model.Provider) ([]model.AccessibleAccount, error) {
	return nil, nil
}

// TestOrchestrator_ReadAccounts_NilNilListerResultIsError pins the guard that turns a
// (nil, nil) lister into a 503.
//
// A nil slice with no error is indistinguishable from "no accounts" at the call site but is
// not the same thing: the handler builds a response from the result, so a lister that lost
// its result without reporting a failure would surface as an empty account picker and the
// operator would conclude the credential can reach nothing. The metrics path has the exact
// same guard and the exact same test; the account tests otherwise only cover a non-nil
// EMPTY slice, which returns through the happy path and never evaluates this branch.
func TestOrchestrator_ReadAccounts_NilNilListerResultIsError(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: nilResultAccountLister{},
	})

	accounts, err := orch.ReadAccounts(context.Background(), "proj-1", model.ProviderGoogleAds)
	if err == nil {
		t.Fatal("expected an error when the AccountLister returns (nil, nil), got nil")
	}
	if errors.Is(err, domain.ErrAccountsUnsupported) {
		t.Errorf("err = %v, but ErrAccountsUnsupported is a 400 meaning the platform has no "+
			"lister at all — this platform HAS one and it misbehaved, which is a 503", err)
	}
	if accounts != nil {
		t.Errorf("accounts = %+v, want nil alongside the error", accounts)
	}
}

// accountListerRecordingDeadline is an AccountLister that records the deadline it was
// handed, so the bound on the synchronous platform call can be asserted rather than
// assumed.
type accountListerRecordingDeadline struct {
	gotDeadline time.Time
	hadDeadline bool
}

func (d *accountListerRecordingDeadline) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (d *accountListerRecordingDeadline) ListAccounts(ctx context.Context, _ string, _ model.Provider) ([]model.AccessibleAccount, error) {
	d.gotDeadline, d.hadDeadline = ctx.Deadline()
	return []model.AccessibleAccount{{ID: "1234567890"}}, nil
}

type emailSearcherRecordingDeadline struct {
	gotDeadline time.Time
	hadDeadline bool
}

func (d *emailSearcherRecordingDeadline) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (d *emailSearcherRecordingDeadline) SearchEmails(ctx context.Context, _ string, _ model.Provider, _ string) ([]model.MarketingEmail, error) {
	d.gotDeadline, d.hadDeadline = ctx.Deadline()
	return []model.MarketingEmail{{ID: "112233"}}, nil
}

// TestOrchestrator_SearchEmailsBoundsThePlatformCall is the email-search half of the pair
// below, and it matters MORE here rather than less: SearchEmails walks cursor pages, so an
// unbounded call is not one hung request but up to maxListPages of them, and the shared
// accountsCallTimeout is the only thing standing between a slow portal and a pinned request
// goroutine. Without this test, deleting the WithTimeout in Orchestrator.SearchEmails leaves
// every email test green — the mocks ignore the context entirely.
func TestOrchestrator_SearchEmailsBoundsThePlatformCall(t *testing.T) {
	disp := &emailSearcherRecordingDeadline{}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderHubSpot: disp,
	})

	// No deadline on the caller's context, deliberately: the bound must be imposed here rather
	// than inherited from whatever the request happened to carry.
	beforeCall := time.Now()
	emails, err := orch.SearchEmails(context.Background(), "proj-1", model.ProviderHubSpot, "kubecon")
	afterCall := time.Now()
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(emails) != 1 {
		t.Fatalf("emails = %+v, want the one the searcher returned", emails)
	}

	if !disp.hadDeadline {
		t.Fatal("the email searcher received a context with NO deadline; a slow portal would pin the request goroutine across every page of the walk")
	}
	expectedMin := beforeCall.Add(accountsCallTimeout)
	expectedMax := afterCall.Add(accountsCallTimeout)
	if disp.gotDeadline.Before(expectedMin) || disp.gotDeadline.After(expectedMax) {
		t.Errorf("deadline %v not within [%v, %v] (call bracket + accountsCallTimeout=%v)",
			disp.gotDeadline, expectedMin, expectedMax, accountsCallTimeout)
	}
}

// TestOrchestrator_ReadAccountsBoundsThePlatformCall pins that ReadAccounts hands the
// AccountLister a context bounded by accountsCallTimeout.
//
// Account discovery is a SYNCHRONOUS call made while an HTTP request is held open, and it
// reaches an external provider — Google Ads' listAccessibleCustomers plus, on an MCC
// credential, a second customer_client search. An unbounded call there does not fail, it
// HANGS: the handler's own context may carry no deadline at all, and nothing else in this
// path imposes one, so a provider that stops responding would pin a request goroutine
// indefinitely. The metrics path is asserted the same way; without this test the bound
// could be dropped in a refactor and every test would still pass.
func TestOrchestrator_ReadAccountsBoundsThePlatformCall(t *testing.T) {
	disp := &accountListerRecordingDeadline{}
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})

	// The caller's context deliberately carries NO deadline — the bound must come from the
	// orchestrator, not be inherited. Bracket the call so the tolerance window covers exactly
	// the span the deadline could have been derived from.
	beforeCall := time.Now()
	accounts, err := orch.ReadAccounts(context.Background(), "proj-1", model.ProviderGoogleAds)
	afterCall := time.Now()
	if err != nil {
		t.Fatalf("ReadAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want the one the lister returned", accounts)
	}

	if !disp.hadDeadline {
		t.Fatal("the account lister received a context with NO deadline; a hung provider would pin the request goroutine")
	}
	expectedMin := beforeCall.Add(accountsCallTimeout)
	expectedMax := afterCall.Add(accountsCallTimeout)
	if disp.gotDeadline.Before(expectedMin) || disp.gotDeadline.After(expectedMax) {
		t.Errorf("deadline %v not within [%v, %v] (call bracket + accountsCallTimeout=%v)",
			disp.gotDeadline, expectedMin, expectedMax, accountsCallTimeout)
	}
}

// accountNotSelectedErr is the shape Meta's requireMetaAccountID (and Google Ads' connection
// validator) produce for a connection parked in the credentials-only bootstrap state: a
// pre-create fault carrying the ErrConnectionNotUsable / ErrAccountNotSelected pair.
// It models internal/dispatch's preCreateError faithfully, Unwrap included — without that
// method errors.Is cannot reach the sentinels and unusableConnectionReason answers
// "unclassified" no matter what the production code does, which would make this test pass
// against the very thing it exists to pin.
type accountNotSelectedErr struct{ err error }

func (e accountNotSelectedErr) Error() string        { return e.err.Error() }
func (e accountNotSelectedErr) Unwrap() error        { return e.err }
func (accountNotSelectedErr) NoUpstreamCreate() bool { return true }

type accountNotSelectedDispatcher struct{}

func (accountNotSelectedDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, accountNotSelectedErr{err: fmt.Errorf("%w: %w: meta connection has no ad account selected",
		domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected)}
}

// TestOrchestrator_PreCreateFailureLogsAClassifiedReason pins the ONLY place the
// account_not_selected classification is observable on the path that actually creates
// campaigns.
//
// Every mention of that token — Meta's requireMetaAccountID, Google Ads' connection
// validator, design/connection.go's rule for relaxing Required("account_id"), and the
// api-catalog paragraph that justifies credentials-only connections — rests on an operator
// being able to tell a missing account selection apart from a bad credential. None of them
// can rest on the job result: dispatchPlatform sets it to "platform campaign creation
// failed" for every dispatcher error alike. So the log line is the whole mechanism, and
// before this it carried only err.Error() — the classification existed in prose describing a
// field that was never emitted.
//
// Asserting the ATTRIBUTE rather than the message text is the point: err.Error() happens to
// contain the sentinel's own wording, so a test that grepped the rendered line would pass
// against the unclassified version and prove nothing.
func TestOrchestrator_PreCreateFailureLogsAClassifiedReason(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds: accountNotSelectedDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderMetaAds}, nil)
	j := waitForTerminal(t, jobs, id)

	// The job result stays generic — that is the premise, not a defect, and the docs now say
	// so. If this ever starts carrying the reason, the docs asserting otherwise are stale.
	if strings.Contains(string(j.Result), "account_not_selected") || strings.Contains(j.Error, "account_not_selected") {
		t.Errorf("job result = %q / error = %q; the reason is documented as absent from the polled "+
			"result, so the api-catalog and design/connection.go paragraphs saying the detail is "+
			"log-only need revisiting", string(j.Result), j.Error)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.recs {
		if !strings.Contains(rec.Message, "before upstream create") {
			continue
		}
		// Filtered by job id: slog.Default is process-wide and a sibling test's dispatch
		// goroutine can still be draining into this handler.
		var reason, gotJob, gotProject string
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				reason = a.Value.String()
			case "job_id":
				gotJob = a.Value.String()
			case "project_id":
				gotProject = a.Value.String()
			}
			return true
		})
		if gotJob != id {
			continue
		}
		// The reason names the DEFECT; the project names the connection carrying it. run is
		// parented on o.rootCtx (Start), so nothing request-scoped survives into this record
		// and the attribute has to be passed explicitly — without it an operator holding
		// reason=account_not_selected still has to resolve the job id against the database
		// before they can repair anything.
		if gotProject != brief.ProjectID {
			t.Errorf("pre-create dispatch failure logged project_id=%q, want %q", gotProject, brief.ProjectID)
		}
		if reason != "account_not_selected" {
			t.Fatalf("pre-create dispatch failure logged reason=%q, want \"account_not_selected\". "+
				"Without a structured reason the classification reaches nothing at all: the job "+
				"result is collapsed to a fixed string, so this attribute is the only thing that "+
				"distinguishes an unselected account from an unusable credential", reason)
		}
		return
	}
	t.Fatal("no \"before upstream create\" log record was emitted, so the reason has nowhere to live")
}

// undecodableCredsDispatcher fails the way the credential path actually fails: the blob
// decrypted fine and then would not JSON-decode, so the error text is an encoding/json
// message — and encoding/json QUOTES ITS INPUT. The canary below stands in for what that
// quoting drags along, which on this path is decrypted credential material.
type undecodableCredsDispatcher struct{}

const credentialCanary = "sk_live_CANARY_DO_NOT_LOG"

func (undecodableCredsDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, accountNotSelectedErr{err: fmt.Errorf(
		"decoding meta credentials: invalid character 'x' looking for beginning of value in %q: %w: %w",
		credentialCanary, domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable)}
}

// TestOrchestrator_ClassifiedPreCreateFailureLogsNoCause is the other half of the reason
// gate, and the half that makes the substitution real rather than decorative.
//
// The reason token is logged INSTEAD of the cause, not alongside it. That is not a style
// choice: two conditions in the vocabulary — credentials_undecodable and
// credentials_incomplete — are detected by decoding the DECRYPTED blob, and an
// encoding/json error quotes its input. internal/dispatch/creds.go states the rule at the
// point the 400 is built, in the imperative, because the tempting edit is precisely to add
// the cause back for debuggability: "it logs a fixed reason token and nothing else ... Do
// not 'restore' logging of the cause on the 400 path."
//
// A prose rule with no test is a rule that gets undone by the next person who wants a
// better error message — this PR's first draft of the gate is the proof, since it added
// "error", derr to exactly this arm. So the assertion sweeps EVERY attribute's rendered
// value for the canary rather than checking that one key is absent: the leak does not care
// which key carries it.
func TestOrchestrator_ClassifiedPreCreateFailureLogsNoCause(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds: undecodableCredsDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderMetaAds}, nil)
	j := waitForTerminal(t, jobs, id)

	// The polled result is collapsed to a fixed string by dispatchPlatform, so it cannot
	// leak either — but assert it, because that collapse is what makes the log the only
	// channel and a future change there would move the leak rather than remove it.
	if strings.Contains(string(j.Result), credentialCanary) || strings.Contains(j.Error, credentialCanary) {
		t.Errorf("job result = %q / error = %q carries the credential canary", string(j.Result), j.Error)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.recs {
		if !strings.Contains(rec.Message, "before upstream create") {
			continue
		}
		var reason, gotJob string
		var leaked []string
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				reason = a.Value.String()
			case "job_id":
				gotJob = a.Value.String()
			}
			if strings.Contains(a.Value.String(), credentialCanary) {
				leaked = append(leaked, a.Key)
			}
			return true
		})
		if gotJob != id {
			continue
		}
		if reason != "credentials_undecodable" {
			t.Fatalf("logged reason=%q, want \"credentials_undecodable\"", reason)
		}
		if len(leaked) != 0 {
			t.Fatalf("attribute(s) %v carry the decrypted-credential canary %q. The reason token "+
				"replaces the cause on this arm precisely so a json decode error cannot quote the "+
				"blob it failed on into an operator-facing log", leaked, credentialCanary)
		}
		if strings.Contains(rec.Message, credentialCanary) {
			t.Fatalf("the log MESSAGE carries the canary: %q", rec.Message)
		}
		return
	}
	t.Fatal("no \"before upstream create\" log record was emitted")
}

// systemCredsDispatcher fails exactly as undecodableCredsDispatcher does, but on the
// LF-owned SYSTEM row rather than on the project's own connection.
//
// The sentinel order is the point. internal/dispatch/creds.go:188-191 wraps
// ErrSystemConnectionNotUsable ALONGSIDE ErrConnectionNotUsable rather than instead of it —
// domain/errors.go says so in as many words — so errors.Is reports BOTH, and a single broad
// arm matches first and answers a question nobody asked.
type systemCredsDispatcher struct{}

func (systemCredsDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, accountNotSelectedErr{err: fmt.Errorf(
		"decoding meta credentials: invalid character 'x' looking for beginning of value in %q: %w: %w: %w",
		credentialCanary, domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable,
		domain.ErrCredentialsUndecodable)}
}

// TestOrchestrator_SystemConnectionPreCreateFailurePagesTheOperator pins the OTHER thing the
// reason gate must not throw away.
//
// Suppressing the cause is what that arm exists for. Suppressing the SCOPE was collateral:
// once every ErrConnectionNotUsable took one branch, a broken LF fallback row — which
// domain.ErrSystemConnectionNotUsable defines as the operator's page, and which means every
// project without its own connection is failing — logged identically to one project's
// misconfigured connection. The synchronous handlers had always split the two
// (brief.go:577, brief.go:914, connection.go:276); the asynchronous path is the only one on
// which a campaign is ever created, and it did not.
//
// The assertions are deliberately paired: the scope must be distinguished AND the cause must
// still be gone. A split that reintroduced the raw error on the new arm would be a
// regression of the fix this test sits next to, so the canary sweep runs here too.
func TestOrchestrator_SystemConnectionPreCreateFailurePagesTheOperator(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds: systemCredsDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderMetaAds}, nil)
	waitForTerminal(t, jobs, id)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.recs {
		var reason, gotJob string
		var leaked []string
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				reason = a.Value.String()
			case "job_id":
				gotJob = a.Value.String()
			}
			if strings.Contains(a.Value.String(), credentialCanary) {
				leaked = append(leaked, a.Key)
			}
			return true
		})
		if gotJob != id {
			continue
		}
		if !strings.Contains(rec.Message, "the LF system connection is not usable") {
			t.Fatalf("logged message = %q, want the LF-system wording. A system-row defect that "+
				"reads like one project's broken connection sends nobody to the row that is "+
				"actually broken, and every project without its own connection is failing", rec.Message)
		}
		if reason != "credentials_undecodable" {
			t.Fatalf("logged reason=%q, want \"credentials_undecodable\"", reason)
		}
		if len(leaked) != 0 || strings.Contains(rec.Message, credentialCanary) {
			t.Fatalf("attribute(s) %v (message %q) carry the decrypted-credential canary %q on the "+
				"system arm — splitting the scope out must not restore the cause", leaked, rec.Message, credentialCanary)
		}
		return
	}
	t.Fatal("no pre-create failure log record was emitted for this job")
}

type malformedConfigDispatcher struct{}

func (malformedConfigDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	// Deliberately NOT an ErrConnectionNotUsable chain: a malformed platform config is
	// pre-create (nothing was sent upstream) but says nothing about the connection.
	return nil, accountNotSelectedErr{err: errors.New("metaConfig is not valid JSON")}
}

// TestOrchestrator_UnclassifiablePreCreateFailureOmitsTheReason is the other half of the
// contract pinned above, and the half that is easy to lose.
//
// dispatchErrIsPreCreate keys on NoUpstreamCreate, which is set by more than connection
// faults — a malformed platform config, a brief-validation failure and a
// connection-repository error all release the claim too. unusableConnectionReason is
// defined over ErrConnectionNotUsable chains, so emitting it unconditionally would stamp
// reason="unclassified" on every one of those.
//
// That is worse than carrying no attribute, which is why it is worth a test rather than a
// comment: an alert or dashboard grouping by `reason` cannot tell "we classified this and
// learned nothing" from "no classification applies here" — the first invents a bucket that
// looks like a real finding and grows with unrelated traffic. Omitting the key says which
// one it is.
func TestOrchestrator_UnclassifiablePreCreateFailureOmitsTheReason(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	jobs := newFakeJobRepo()
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds: malformedConfigDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderMetaAds}, nil)
	waitForTerminal(t, jobs, id)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.recs {
		if !strings.Contains(rec.Message, "before upstream create") {
			continue
		}
		var reason, gotJob string
		var hasReason bool
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				reason, hasReason = a.Value.String(), true
			case "job_id":
				gotJob = a.Value.String()
			}
			return true
		})
		if gotJob != id {
			continue
		}
		if hasReason {
			t.Fatalf("a non-connection pre-create failure logged reason=%q; it must carry no "+
				"reason attribute at all. unusableConnectionReason has no vocabulary for this "+
				"error, so any value it returns is a classification that was never made", reason)
		}
		return
	}
	t.Fatal("no \"before upstream create\" log record was emitted for the unclassifiable error")
}

// settingsReaderDispatcher implements PlatformDispatcher + SettingsReader, recording the
// context deadline so a test can verify the orchestrator bounded the call.
type settingsReaderDispatcher struct {
	readback    *model.CampaignSettingsReadback
	err         error
	called      bool
	gotDeadline time.Time
}

func (settingsReaderDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("Dispatch should not be called in these tests")
}

func (d *settingsReaderDispatcher) ReadSettings(ctx context.Context, _ string, _ model.Provider, _ *model.Campaign) (*model.CampaignSettingsReadback, error) {
	d.called = true
	if deadline, ok := ctx.Deadline(); ok {
		d.gotDeadline = deadline
	}
	return d.readback, d.err
}

// TestOrchestrator_ReadCampaignSettings_NotProvisionedNeverContactsThePlatform: a row with
// no upstream id has no counterpart, so the platform must not be contacted at all.
func TestOrchestrator_ReadCampaignSettings_NotProvisionedNeverContactsThePlatform(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &settingsReaderDispatcher{}
	o := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	camp := &model.Campaign{ID: "c1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "   "}
	_, err := o.ReadCampaignSettings(context.Background(), "p1", model.ProviderGoogleAds, camp)
	if !errors.Is(err, ErrCampaignNotProvisioned) {
		t.Fatalf("err = %v, want ErrCampaignNotProvisioned", err)
	}
	if disp.called {
		t.Error("the platform was contacted for an unprovisioned campaign")
	}
}

// TestOrchestrator_ReadCampaignSettings_DispatcherNotASettingsReader: a platform without the
// capability yields a clean, permanent "not supported".
func TestOrchestrator_ReadCampaignSettings_DispatcherNotASettingsReader(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	o := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: nonMetricsDispatcher{}})

	camp := &model.Campaign{ID: "c1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "ga-1"}
	_, err := o.ReadCampaignSettings(context.Background(), "p1", model.ProviderGoogleAds, camp)
	if !errors.Is(err, domain.ErrSettingsReadbackUnsupported) {
		t.Fatalf("err = %v, want ErrSettingsReadbackUnsupported", err)
	}
}

// TestOrchestrator_ReadCampaignSettings_NilResultIsAnError: a SettingsReader returning
// (nil, nil) is a contract violation. The handler dereferences the result on a nil error,
// so this must become an ordinary error rather than a panic.
func TestOrchestrator_ReadCampaignSettings_NilResultIsAnError(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &settingsReaderDispatcher{} // nil readback, nil error
	o := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	camp := &model.Campaign{ID: "c1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "ga-1"}
	if _, err := o.ReadCampaignSettings(context.Background(), "p1", model.ProviderGoogleAds, camp); err == nil {
		t.Fatal("expected an error when the SettingsReader returns (nil, nil), got nil")
	}
}

// TestOrchestrator_ReadCampaignSettings_BoundsTheCall: the read runs on the HTTP request
// goroutine, so it must carry a deadline or a hung platform would outlive the response.
func TestOrchestrator_ReadCampaignSettings_BoundsTheCall(t *testing.T) {
	camps, jobs := &fakeCampaignRepo{}, newFakeJobRepo()
	disp := &settingsReaderDispatcher{readback: &model.CampaignSettingsReadback{CampaignID: "c1"}}
	o := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	camp := &model.Campaign{ID: "c1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "ga-1"}

	// Bracket the call itself, not just the assertion, so the tolerance window covers exactly
	// the wall-clock span the deadline could have been derived from — the same discipline the
	// metrics timeout test applies.
	beforeCall := time.Now()
	_, err := o.ReadCampaignSettings(context.Background(), "p1", model.ProviderGoogleAds, camp)
	afterCall := time.Now()
	if err != nil {
		t.Fatalf("ReadCampaignSettings: %v", err)
	}

	if disp.gotDeadline.IsZero() {
		t.Error("ReadSettings was called with no deadline; a synchronous platform read must be bounded")
	}

	// Assert the deadline is approximately settingsCallTimeout after the call, not merely that
	// SOME deadline exists. A regression widening settingsCallTimeout past the server's write
	// timeout would leave the existence check green while defeating the bound this test pins.
	expectedMinDeadline := beforeCall.Add(settingsCallTimeout)
	expectedMaxDeadline := afterCall.Add(settingsCallTimeout)
	if disp.gotDeadline.Before(expectedMinDeadline) || disp.gotDeadline.After(expectedMaxDeadline) {
		t.Errorf("deadline %v not within [%v, %v] (beforeCall/afterCall + settingsCallTimeout)",
			disp.gotDeadline, expectedMinDeadline, expectedMaxDeadline)
	}

	// And pin the bound that actually matters, against a constant this test does NOT derive
	// from settingsCallTimeout. The check above brackets the deadline against the very
	// constant under test, so widening settingsCallTimeout moves the expectation with it and
	// the regression survives. The contract is that a synchronous read completes inside the
	// server's write timeout — exceed it and the platform call outlives the response it was
	// read for, which is the failure this test exists to prevent.
	if settingsCallTimeout >= constants.DefaultWriteTimeout {
		t.Errorf("settingsCallTimeout %v must be strictly less than the server write timeout %v; a read that outlives the response cannot be returned",
			settingsCallTimeout, constants.DefaultWriteTimeout)
	}
}
