// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package membudget is the ONE per-pod memory budget that creative-asset uploads draw against.
//
// Two admission points charge it, and before #179 each charged its own independent semaphore:
//
//   - middleware.UploadAdmission charges the WIRE side of an upload (the buffered body, the
//     base64 string the decoder materialises and the base64-decoded slice), priced from the
//     declared Content-Length before the body is read;
//   - service.DecodeReserver charges the decoded PIXEL buffer, priced from the image header
//     after it is read and before image.Decode allocates.
//
// Two budgets that were each a quarter of the pod capped uploads at half the pod only if their
// charges never overlapped, and they did: the wire weight included the pixel buffer the decode
// reservation also charged. One budget with non-overlapping charges is a process-wide bound by
// construction — the sum of what every admitted upload holds can never exceed Total — and the
// arithmetic a reader has to check is one inequality, not two that have to be added up.
package membudget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// Budget is a weighted, process-wide byte budget with a bounded wait.
//
// The zero value is not usable; build one with New. A nil *Budget admits everything and holds
// nothing, so a construction that wires no budget keeps working.
type Budget struct {
	sem   *semaphore.Weighted
	total int64
	inUse atomic.Int64
}

// New returns a budget of total bytes, or nil when total is not positive.
func New(total int64) *Budget {
	if total <= 0 {
		return nil
	}
	return &Budget{sem: semaphore.NewWeighted(total), total: total}
}

// Total is the budget's size in bytes; 0 for a nil budget.
func (b *Budget) Total() int64 {
	if b == nil {
		return 0
	}
	return b.total
}

// InUse is the bytes currently held by admitted callers; 0 for a nil budget. It is what the
// campaign_upload_memory_budget_in_use_bytes gauge reports.
func (b *Budget) InUse() int64 {
	if b == nil {
		return 0
	}
	return b.inUse.Load()
}

// Acquire waits at most wait for want bytes and reports whether it got them. The returned
// release gives back exactly what was taken and is safe to call more than once; it is a no-op
// when ok is false.
//
// A want above the whole budget is refused at once rather than after the wait: it could never be
// admitted, so waiting only delays the same answer. A non-positive want is admitted holding
// nothing. The wait derives from ctx, so a cancelled caller still returns promptly — it only ever
// makes the wait shorter. It is bounded here rather than by ctx because net/http gives a
// handler's r.Context() no deadline, and an unbounded wait converts a memory bound into goroutine
// exhaustion.
//
// semaphore.Weighted is FIFO: a large waiter at the head blocks smaller ones behind it. That is
// the right order here. An upload already holding its wire charge and waiting for its pixel
// charge is not overtaken by new uploads arriving after it, so admitted work drains before more
// is admitted.
func (b *Budget) Acquire(ctx context.Context, want int64, wait time.Duration) (release func(), ok bool) {
	if b == nil || want <= 0 {
		return func() {}, true
	}
	if want > b.total {
		return func() {}, false
	}
	actx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := b.sem.Acquire(actx, want); err != nil {
		return func() {}, false
	}
	b.inUse.Add(want)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			b.inUse.Add(-want)
			b.sem.Release(want)
		}
	}, true
}

// EnvBudgetBytes is the environment variable that sets the budget explicitly, in bytes, or as a
// Kubernetes binary quantity (for example 200Mi, 1Gi). Unset is the normal case: the budget is
// then DefaultFraction of the container memory limit.
const EnvBudgetBytes = "UPLOAD_MEMORY_BUDGET_BYTES"

// DefaultFractionPercent is the share of the container memory limit the budget takes when the
// env var is unset. 40%: the runtime, the Postgres pool, the platform clients, the dispatch
// asset budget and every non-upload request share the same limit, so uploads may claim only a
// minority of it.
const DefaultFractionPercent = 40

// Source names where a resolved budget came from, for the startup log.
type Source string

const (
	SourceEnv      Source = "env"
	SourceCgroup   Source = "cgroup"
	SourceFallback Source = "fallback"
)

// Resolution is the outcome of Resolve.
type Resolution struct {
	Bytes  int64
	Source Source
	// LimitBytes is the memory limit the default was derived from (0 for SourceEnv).
	LimitBytes int64
	// RaisedToFloor reports that the derived or configured value was below floorBytes and was
	// raised to it, so the startup log can say so.
	RaisedToFloor bool
}

// Resolve picks the budget size.
//
//   - EnvBudgetBytes, when set, wins. An unparseable or non-positive value is an error: a
//     typo must stop the pod rather than silently fall back to a default nobody chose.
//   - Otherwise DefaultFractionPercent of the container memory limit read from the cgroup
//     (readLimit), when one is set.
//   - Otherwise DefaultFractionPercent of fallbackLimitBytes, the chart's default limit.
//
// The result is never below floorBytes, the cost of ONE worst-case legal upload. A budget below
// it would refuse every maximum-size upload forever with a retryable 503 that no retry can
// satisfy, which is a functional outage dressed as back-pressure. Raising it to the floor can
// exceed the intended share on a very small pod; RaisedToFloor makes that visible in the log.
func Resolve(getenv func(string) string, readLimit func() (int64, bool), fallbackLimitBytes, floorBytes int64) (Resolution, error) {
	var res Resolution
	if raw := strings.TrimSpace(getenv(EnvBudgetBytes)); raw != "" {
		n, err := ParseBytes(raw)
		if err != nil {
			return Resolution{}, fmt.Errorf("%s: %w", EnvBudgetBytes, err)
		}
		res = Resolution{Bytes: n, Source: SourceEnv}
	} else if limit, ok := readLimit(); ok {
		res = Resolution{Bytes: limit / 100 * DefaultFractionPercent, Source: SourceCgroup, LimitBytes: limit}
	} else {
		res = Resolution{Bytes: fallbackLimitBytes / 100 * DefaultFractionPercent, Source: SourceFallback, LimitBytes: fallbackLimitBytes}
	}
	if res.Bytes < floorBytes {
		res.Bytes = floorBytes
		res.RaisedToFloor = true
	}
	return res, nil
}

var errBadQuantity = errors.New("want a positive byte count, optionally with a Ki, Mi or Gi suffix")

// ParseBytes parses a positive byte count, optionally suffixed Ki, Mi or Gi.
func ParseBytes(s string) (int64, error) {
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, errBadQuantity
	}
	return n * mult, nil
}

// cgroup files holding the container memory limit, v2 first.
const (
	cgroupV2MemoryMax   = "/sys/fs/cgroup/memory.max"
	cgroupV1MemoryLimit = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// unlimitedThreshold: cgroup v1 reports "no limit" as a page-aligned value near MaxInt64.
// Anything this large is no limit at all, and 40% of it would be no bound.
const unlimitedThreshold = int64(1) << 60

// ReadCgroupLimit reads the container memory limit from cgroup v2, then v1. It reports false
// when neither file is readable or the limit is unset ("max" on v2, a near-MaxInt64 value on v1).
func ReadCgroupLimit() (int64, bool) {
	return readCgroupLimitFrom(os.ReadFile, cgroupV2MemoryMax, cgroupV1MemoryLimit)
}

func readCgroupLimitFrom(readFile func(string) ([]byte, error), paths ...string) (int64, bool) {
	for _, p := range paths {
		raw, err := readFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(raw))
		if v == "max" {
			return 0, false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n >= unlimitedThreshold {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
