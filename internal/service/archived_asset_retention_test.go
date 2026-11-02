// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// fakeAssetPruner answers each batch from a script and records what it was asked. Once the script
// runs out it answers a full batch, so a pass that does not stop on its own runs to the cap.
type fakeAssetPruner struct {
	mu      sync.Mutex
	script  []pruneAnswer
	windows []time.Duration
	limits  []int
}

type pruneAnswer struct {
	n   int64
	ran bool
	err error
}

func (f *fakeAssetPruner) PruneArchivedBriefAssets(_ context.Context, olderThan time.Duration, limit int) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows = append(f.windows, olderThan)
	f.limits = append(f.limits, limit)
	if len(f.script) == 0 {
		return int64(limit), true, nil
	}
	a := f.script[0]
	f.script = f.script[1:]
	return a.n, a.ran, a.err
}

func (f *fakeAssetPruner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.windows)
}

func newPruneOrchestrator(t *testing.T, p domain.ArchivedCreativeAssetPruner) *Orchestrator {
	t.Helper()
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), nil)
	orch.SetArchivedAssetPruner(p)
	t.Cleanup(func() { _ = orch.Shutdown(context.Background(), time.Second) })
	return orch
}

// One pass drains the backlog in batches and stops at the first short batch, asking for the
// 90-day window the product decided (#171) and the batch size whose short answer ends the pass.
func TestArchivedAssetPrune_DrainsBatchesUntilAShortOne(t *testing.T) {
	p := &fakeAssetPruner{script: []pruneAnswer{{100, true, nil}, {100, true, nil}, {37, true, nil}}}
	orch := newPruneOrchestrator(t, p)

	if got := orch.runArchivedAssetPrune(); got != 237 {
		t.Errorf("pass deleted %d, want 237", got)
	}
	if got := p.calls(); got != 3 {
		t.Errorf("pass ran %d batches, want 3: it must stop at the first short batch", got)
	}
	for i, w := range p.windows {
		if w != 90*24*time.Hour {
			t.Errorf("batch %d window = %s, want 2160h (90 days)", i, w)
		}
		if p.limits[i] != archivedAssetBatchSize {
			t.Errorf("batch %d limit = %d, want %d", i, p.limits[i], archivedAssetBatchSize)
		}
	}
}

// A replica that finds the prune lock taken leaves the pass to the holder at once, rather than
// spinning through its batch cap re-asking for a lock another replica holds.
func TestArchivedAssetPrune_StopsWhenAnotherReplicaHoldsTheLock(t *testing.T) {
	p := &fakeAssetPruner{script: []pruneAnswer{{0, false, nil}, {0, false, nil}}}
	orch := newPruneOrchestrator(t, p)

	if got := orch.runArchivedAssetPrune(); got != 0 {
		t.Errorf("pass deleted %d, want 0", got)
	}
	if got := p.calls(); got != 1 {
		t.Errorf("pass made %d attempts, want 1: a held lock must end the pass", got)
	}
}

// A backlog larger than one pass is left for the next tick, so no single pass runs unbounded.
func TestArchivedAssetPrune_CapsBatchesPerPass(t *testing.T) {
	p := &fakeAssetPruner{} // every batch full
	orch := newPruneOrchestrator(t, p)

	orch.runArchivedAssetPrune()
	if got := p.calls(); got != archivedAssetMaxBatchesPerPass {
		t.Errorf("pass ran %d batches, want the cap %d", got, archivedAssetMaxBatchesPerPass)
	}
}

// A failed batch ends the pass and reports what was already deleted; the next tick retries.
func TestArchivedAssetPrune_ErrorEndsThePass(t *testing.T) {
	p := &fakeAssetPruner{script: []pruneAnswer{{100, true, nil}, {0, true, errors.New("db down")}}}
	orch := newPruneOrchestrator(t, p)

	if got := orch.runArchivedAssetPrune(); got != 100 {
		t.Errorf("pass deleted %d, want the 100 before the failure", got)
	}
	if got := p.calls(); got != 2 {
		t.Errorf("pass made %d calls, want 2: it must stop at the failure", got)
	}
}

// Unbound, the sweeper does nothing rather than panicking: direct-construction tests and any
// wiring without a database leave it nil.
func TestArchivedAssetPrune_NilPrunerIsInert(t *testing.T) {
	orch := newPruneOrchestrator(t, nil)
	if got := orch.runArchivedAssetPrune(); got != 0 {
		t.Errorf("pass deleted %d with no pruner", got)
	}
}

// The LOOP reaches the prune: a mis-wired ticker or select arm would pass every direct-call test
// above while pruning nothing in production.
func TestArchivedAssetRetentionSweeper_PrunesOnTick(t *testing.T) {
	p := &fakeAssetPruner{script: []pruneAnswer{{0, true, nil}, {0, true, nil}, {0, true, nil}}}
	orch := newPruneOrchestrator(t, p)
	orch.StartArchivedAssetRetentionSweeperWithInterval(5 * time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for p.calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if p.calls() == 0 {
		t.Fatal("the sweeper never called PruneArchivedBriefAssets")
	}
}

// The sweeper's lifetime is sweeperCtx: Shutdown must not wait out the hourly interval.
func TestArchivedAssetRetentionSweeper_StopsOnShutdown(t *testing.T) {
	orch := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), nil)
	orch.SetArchivedAssetPruner(&fakeAssetPruner{})
	orch.StartArchivedAssetRetentionSweeper()

	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(context.Background(), time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: the archived-asset sweeper is not bound to sweeperCtx")
	}
}
