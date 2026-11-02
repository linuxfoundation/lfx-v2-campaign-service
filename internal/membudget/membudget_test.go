// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package membudget

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestBudget_AcquireTracksInUseAndReleasesOnce(t *testing.T) {
	b := New(100)
	rel, ok := b.Acquire(context.Background(), 60, time.Second)
	if !ok {
		t.Fatal("60 of 100 must be admitted")
	}
	if got := b.InUse(); got != 60 {
		t.Fatalf("InUse = %d, want 60", got)
	}
	rel()
	rel() // a second call must not release twice
	if got := b.InUse(); got != 0 {
		t.Fatalf("InUse after release = %d, want 0", got)
	}
	// A double release would have corrupted the semaphore so 200 would fit; it must not.
	if _, ok := b.Acquire(context.Background(), 101, time.Millisecond); ok {
		t.Fatal("more than the budget must never be admitted")
	}
}

// The wait is bounded by the budget's own timeout, not by the caller's context, which in
// net/http carries no deadline at all.
func TestBudget_WaitIsBoundedAndShedsWhenExhausted(t *testing.T) {
	b := New(100)
	hold, ok := b.Acquire(context.Background(), 80, time.Second)
	if !ok {
		t.Fatal("first acquire must succeed")
	}
	defer hold()

	start := time.Now()
	_, ok = b.Acquire(context.Background(), 30, 50*time.Millisecond)
	if ok {
		t.Fatal("30 more on an 80/100 budget must shed")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %v, want about the 50ms bound", waited)
	}
	if got := b.InUse(); got != 80 {
		t.Fatalf("a shed acquire must hold nothing; InUse = %d", got)
	}
}

func TestBudget_WaiterIsAdmittedWhenBudgetFrees(t *testing.T) {
	b := New(100)
	hold, _ := b.Acquire(context.Background(), 80, time.Second)
	go func() {
		time.Sleep(20 * time.Millisecond)
		hold()
	}()
	rel, ok := b.Acquire(context.Background(), 50, time.Second)
	if !ok {
		t.Fatal("a waiter must be admitted once the holder releases")
	}
	rel()
}

func TestBudget_NilAndEdgeCases(t *testing.T) {
	if New(0) != nil || New(-1) != nil {
		t.Fatal("a non-positive budget must be nil")
	}
	var nb *Budget
	if rel, ok := nb.Acquire(context.Background(), 1<<40, time.Millisecond); !ok {
		t.Fatal("a nil budget admits everything")
	} else {
		rel()
	}
	if nb.Total() != 0 || nb.InUse() != 0 {
		t.Fatal("a nil budget reports zero")
	}
	b := New(10)
	if _, ok := b.Acquire(context.Background(), 11, time.Hour); ok {
		t.Fatal("a want above the whole budget is refused")
	}
}

func TestResolve(t *testing.T) {
	const mi = int64(1) << 20
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvBudgetBytes {
				return v
			}
			return ""
		}
	}
	cgroup := func(n int64, ok bool) func() (int64, bool) { return func() (int64, bool) { return n, ok } }

	cases := []struct {
		name     string
		env      string
		limit    int64
		hasLimit bool
		want     int64
		source   Source
		floored  bool
	}{
		{"env wins over cgroup", "300Mi", 1024 * mi, true, 300 * mi, SourceEnv, false},
		{"env in raw bytes", "268435456", 0, false, 256 * mi, SourceEnv, false},
		{"40% of the cgroup limit", "", 1000 * mi, true, 400 * mi, SourceCgroup, false},
		{"40% of the fallback without a cgroup", "", 0, false, 512 * mi / 100 * 40, SourceFallback, false},
		{"raised to one worst-case upload", "", 256 * mi, true, 192 * mi, SourceCgroup, true},
		{"an explicit env below the floor is raised too", "64Mi", 0, false, 192 * mi, SourceEnv, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(env(tc.env), cgroup(tc.limit, tc.hasLimit), 512*mi, 192*mi)
			if err != nil {
				t.Fatal(err)
			}
			if res.Bytes != tc.want || res.Source != tc.source || res.RaisedToFloor != tc.floored {
				t.Fatalf("Resolve = %+v, want bytes=%d source=%s floored=%v", res, tc.want, tc.source, tc.floored)
			}
		})
	}

	for _, bad := range []string{"lots", "-5", "0", "12MB", "1.5Gi"} {
		if _, err := Resolve(env(bad), cgroup(0, false), 512*mi, 192*mi); err == nil {
			t.Errorf("Resolve(%q) succeeded; a malformed budget must stop the pod, not fall back", bad)
		}
	}
}

func TestReadCgroupLimit(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	cases := []struct {
		name  string
		files map[string]string
		want  int64
		ok    bool
	}{
		{"v2 limit", map[string]string{"v2": "536870912\n"}, 512 << 20, true},
		{"v2 unlimited", map[string]string{"v2": "max\n"}, 0, false},
		{"v1 limit", map[string]string{"v1": "268435456"}, 256 << 20, true},
		{"v1 unlimited", map[string]string{"v1": "9223372036854771712"}, 0, false},
		{"v2 preferred over v1", map[string]string{"v2": "1048576", "v1": "2097152"}, 1 << 20, true},
		{"neither readable", map[string]string{}, 0, false},
		{"garbage", map[string]string{"v2": "lots"}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := readCgroupLimitFrom(files(tc.files), "v2", "v1")
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "2Ki": 2 << 10, "200Mi": 200 << 20, "1Gi": 1 << 30} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = (%d, %v), want %d", in, got, err, want)
		}
	}
	if _, err := ParseBytes("99999999999Gi"); !errors.Is(err, errBadQuantity) {
		t.Errorf("an overflowing quantity must be refused, got %v", err)
	}
}

// The three hold-and-wait scenarios that a single FIFO semaphore produced (#179 review), on the
// production shape: a ~205 MiB budget with an 80 MiB second-stage reserve, 112 MiB worst-case
// wire charges, 80 MiB worst-case pixel charges. Units are MiB here.

// Scenario 1/2: an admitted upload's pixel request must not wait behind a NEW upload's queued wire
// request when the budget has room for the pixels.
func TestBudget_SecondStageIsNotQueuedBehindAWaitingFirstStage(t *testing.T) {
	b := NewWithReserve(205, 80)
	wireA, ok := b.Acquire(context.Background(), 112, time.Second)
	if !ok {
		t.Fatal("premise: A's wire charge fits")
	}
	defer wireA()

	queued := make(chan bool, 1)
	go func() { // B: a second worst-case wire request, which cannot fit beside A's
		rel, ok := b.Acquire(context.Background(), 112, 300*time.Millisecond)
		rel()
		queued <- ok
	}()
	time.Sleep(20 * time.Millisecond) // let B queue

	start := time.Now()
	pixA, ok := b.AcquireSecondStage(context.Background(), 80, 250*time.Millisecond)
	if !ok {
		t.Fatal("A's pixel charge was shed although 93 MiB were free: it queued behind B's wire request")
	}
	if waited := time.Since(start); waited > 50*time.Millisecond {
		t.Fatalf("A's pixel charge waited %v behind a first-stage waiter", waited)
	}
	pixA()
	if <-queued {
		t.Fatal("B's wire charge must not be admitted beside A's: it would exceed the first-stage limit")
	}
}

// Scenario 3: two admitted uploads that each need pixels must not shed each other. The first-stage
// limit (Total - reserve) refuses the second wire charge up front, before its body is read, so
// the one admitted upload always gets its pixels.
func TestBudget_FirstStageAlwaysLeavesRoomForOnePixelCharge(t *testing.T) {
	b := NewWithReserve(205, 80)
	if got := b.FirstStageLimit(); got != 125 {
		t.Fatalf("FirstStageLimit = %d, want 125", got)
	}
	wireA, ok := b.Acquire(context.Background(), 102, time.Second)
	if !ok {
		t.Fatal("premise: the first 102 MiB wire charge fits")
	}
	defer wireA()
	if rel, ok := b.Acquire(context.Background(), 102, 30*time.Millisecond); ok {
		rel()
		t.Fatal("a second 102 MiB wire charge was admitted; the two uploads would then starve each other's pixels")
	}
	pix, ok := b.AcquireSecondStage(context.Background(), 80, 30*time.Millisecond)
	if !ok {
		t.Fatal("the admitted upload must always get a worst-case pixel charge")
	}
	pix()
	if rel, ok := b.Acquire(context.Background(), 126, time.Hour); ok {
		rel()
		t.Fatal("a first-stage want above FirstStageLimit must be refused at once")
	}
}

// When budget frees, a waiting second-stage request is served before a first-stage one queued
// earlier.
func TestBudget_ReleaseServesSecondStageFirst(t *testing.T) {
	b := NewWithReserve(200, 80)
	hold, _ := b.Acquire(context.Background(), 120, time.Second)           // first stage full
	pix1, _ := b.AcquireSecondStage(context.Background(), 70, time.Second) // 190 of 200

	order := make(chan string, 2)
	go func() {
		if rel, ok := b.Acquire(context.Background(), 10, time.Second); ok {
			order <- "first"
			rel()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		if rel, ok := b.AcquireSecondStage(context.Background(), 60, time.Second); ok {
			order <- "second"
			time.Sleep(20 * time.Millisecond)
			rel()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	pix1() // frees 70: the second-stage waiter (60) fits and must go first
	if got := <-order; got != "second" {
		t.Fatalf("first grant after release went to the %s stage, want second", got)
	}
	hold()
	if got := <-order; got != "first" {
		t.Fatalf("got %q", got)
	}
}
