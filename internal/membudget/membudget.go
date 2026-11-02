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
	"sync"
	"sync/atomic"
	"time"
)

// Budget is a process-wide byte budget with two admission stages and a bounded wait.
//
// # Two stages, because one upload asks twice
//
// An upload takes its WIRE charge before the body is read (Acquire) and holds it for the whole
// handler, then asks for its PIXEL charge after the image header is read (AcquireSecondStage)
// while still holding the first. On one plain FIFO semaphore that is hold-and-wait: an admitted
// upload's pixel request queued behind a NEW upload's wire request, which could not be admitted
// until the admitted upload finished, which could not finish without its pixels. Only the wait
// timeouts broke the cycle, so admitted uploads whose bodies were already read stalled and shed
// with 503. Two rules remove it:
//
//   - Acquire (first stage) may only fill the budget up to Total - reserve. With reserve set to
//     the largest second-stage charge (constants.CreativeDecodePeakBytes), there is always room
//     for at least one pixel charge, whatever the first stage has admitted.
//   - AcquireSecondStage queues AHEAD of every first-stage waiter and is never blocked by one;
//     first-stage requests wait while any second-stage request is waiting.
//
// So a second-stage request only ever waits on other second-stage holders, which release as soon
// as their decode returns, and admitted work always drains.
//
// The bound is on LIVE charges: a pixel buffer is released when image.Decode returns, but the Go
// heap frees it on the next GC. The budget is a minority share of the memory limit (40%) partly
// so that this lag, like the body buffer's, fits in the remainder.
//
// The zero value is not usable; build one with New or NewWithReserve. A nil *Budget admits
// everything and holds nothing, so a construction that wires no budget keeps working.
type Budget struct {
	mu      sync.Mutex
	total   int64
	reserve int64
	used    int64
	second  []*waiter // second-stage waiters, FIFO, served first
	first   []*waiter // first-stage waiters, FIFO

	// Test hooks, nil in production and scoped to one budget so no other test sees them.
	// testBeforeWait runs after an acquire has queued and released the lock, before it waits,
	// so a test can have the grant and the cancellation both land before the wait's select.
	// testAfterWait runs after a queued acquire wakes, before it takes the lock to settle the
	// outcome, so a test can land a grant in exactly that window. Set before the budget is used.
	testBeforeWait func()
	testAfterWait  func()
}

type waiter struct {
	want  int64
	ready chan struct{}
}

// New returns a budget of total bytes with no second-stage reserve, or nil when total is not
// positive.
func New(total int64) *Budget { return NewWithReserve(total, 0) }

// NewWithReserve returns a budget of total bytes whose first stage may only fill it up to
// total - reserve (see Budget). Returns nil when total is not positive; a reserve outside
// [0, total) is clamped to 0.
func NewWithReserve(total, reserve int64) *Budget {
	if total <= 0 {
		return nil
	}
	if reserve < 0 || reserve >= total {
		reserve = 0
	}
	return &Budget{total: total, reserve: reserve}
}

// Total is the budget's size in bytes; 0 for a nil budget.
func (b *Budget) Total() int64 {
	if b == nil {
		return 0
	}
	return b.total
}

// FirstStageLimit is the most the first stage may hold in total (Total - reserve), and so the
// largest single first-stage charge that can ever be admitted; 0 for a nil budget.
func (b *Budget) FirstStageLimit() int64 {
	if b == nil {
		return 0
	}
	return b.total - b.reserve
}

// InUse is the bytes currently held by admitted callers of either stage; 0 for a nil budget. It
// is what the campaign_upload_memory_budget_in_use_bytes gauge reports.
func (b *Budget) InUse() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Acquire takes a FIRST-STAGE charge: it waits at most wait for want bytes and reports whether it
// got them. The returned release gives back exactly what was taken and is safe to call more than
// once; it is a no-op when ok is false.
//
// A want above FirstStageLimit is refused at once rather than after the wait: it could never be
// admitted. A non-positive want is admitted holding nothing. A caller whose ctx is already done
// is refused even when capacity is free, and a grant that lands as ctx is cancelled is given
// back rather than returned. The wait derives from ctx, so a cancelled caller still returns
// promptly. It is bounded here rather than by ctx because net/http
// gives a handler's r.Context() no deadline, and an unbounded wait converts a memory bound into
// goroutine exhaustion.
func (b *Budget) Acquire(ctx context.Context, want int64, wait time.Duration) (release func(), ok bool) {
	return b.acquire(ctx, want, wait, false)
}

// AcquireSecondStage takes a SECOND-STAGE charge, for a caller that already holds a first-stage
// one. It may use the whole budget including the reserve, and it is never queued behind a
// first-stage waiter. Otherwise it behaves like Acquire; a want above Total is refused at once.
func (b *Budget) AcquireSecondStage(ctx context.Context, want int64, wait time.Duration) (release func(), ok bool) {
	return b.acquire(ctx, want, wait, true)
}

func noop() {}

func (b *Budget) limit(second bool) int64 {
	if second {
		return b.total
	}
	return b.total - b.reserve
}

func (b *Budget) acquire(ctx context.Context, want int64, wait time.Duration, second bool) (func(), bool) {
	if b == nil || want <= 0 {
		return noop, true
	}
	if want > b.limit(second) {
		return noop, false
	}
	// A caller that is already gone is refused before admission, even with capacity free: the
	// upload behind it would otherwise go on to read its body and run image.Decode, which
	// cannot be cancelled, holding budget that a live upload could use.
	if ctx.Err() != nil {
		return noop, false
	}

	b.mu.Lock()
	ahead := len(b.second) > 0
	if !second {
		ahead = ahead || len(b.first) > 0
	}
	if !ahead && b.used+want <= b.limit(second) {
		b.used += want
		b.mu.Unlock()
		return b.releaser(want), true
	}
	w := &waiter{want: want, ready: make(chan struct{})}
	if second {
		b.second = append(b.second, w)
	} else {
		b.first = append(b.first, w)
	}
	b.mu.Unlock()
	if b.testBeforeWait != nil {
		b.testBeforeWait()
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ready:
	case <-ctx.Done():
	case <-timer.C:
	}
	if b.testAfterWait != nil {
		b.testAfterWait()
	}

	// Every outcome is settled under the lock, so a grant that raced with cancellation or the
	// timeout is seen exactly once.
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-w.ready:
		if ctx.Err() == nil {
			// Granted (possibly between the timeout and taking the lock): it is ours.
			return b.releaser(want), true
		}
		// Granted, but the caller was cancelled concurrently: give the charge back rather
		// than admit an upload nobody is waiting for. grantLocked already took w off its
		// queue, so only the charge needs undoing.
		b.used -= want
		b.grantLocked()
		return noop, false
	default:
	}
	if second {
		b.second = remove(b.second, w)
	} else {
		b.first = remove(b.first, w)
	}
	// Leaving the head of a queue can unblock the waiters behind it.
	b.grantLocked()
	return noop, false
}

func remove(q []*waiter, w *waiter) []*waiter {
	for i, x := range q {
		if x == w {
			return append(q[:i], q[i+1:]...)
		}
	}
	return q
}

// grantLocked admits waiters in order: second-stage first, and first-stage only once no
// second-stage request is waiting. Each queue is FIFO, so a large head is not overtaken by a
// smaller request behind it.
func (b *Budget) grantLocked() {
	for len(b.second) > 0 && b.used+b.second[0].want <= b.limit(true) {
		w := b.second[0]
		b.second = b.second[1:]
		b.used += w.want
		close(w.ready)
	}
	if len(b.second) > 0 {
		return
	}
	for len(b.first) > 0 && b.used+b.first[0].want <= b.limit(false) {
		w := b.first[0]
		b.first = b.first[1:]
		b.used += w.want
		close(w.ready)
	}
}

func (b *Budget) releaser(want int64) func() {
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			b.mu.Lock()
			b.used -= want
			b.grantLocked()
			b.mu.Unlock()
		}
	}
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
