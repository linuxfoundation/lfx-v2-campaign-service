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
