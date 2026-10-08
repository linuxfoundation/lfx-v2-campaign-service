// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"errors"
)

// ErrStatsJobLeaseNotHeld indicates PostgreSQL reported the X stats-job lease for the ad account
// as held by another session, so this instance refuses to create stats jobs there: another pod
// owns them. It is returned only on that definite answer — a database that could not be asked is
// ErrStatsJobLeaseUnavailable. Transient from the caller's side — retrying may
// reach the owning pod, or this one once it acquires the lease. The message is fixed,
// client-safe text: it reaches the HTTP body.
var ErrStatsJobLeaseNotHeld = errors.New("another instance owns X stats jobs; retry")

// ErrStatsJobLeaseUnavailable indicates this instance could not establish whether it owns the X
// stats-job lease — the database could not be asked (no connection, a failed lock query, the
// request ended mid-check) — so, as with ErrStatsJobLeaseNotHeld, it creates no stats jobs. It is
// a separate sentinel because "another pod owns it" is false here and would misdirect an
// operator. Same 503 everywhere; the wrapped detail is logged, never sent. The message is fixed,
// client-safe text.
var ErrStatsJobLeaseUnavailable = errors.New("coordination of X stats jobs is unavailable; retry")

// StatsJobLease makes ONE process the owner of X stats-job creation per ad account, across pods.
//
// The X stats-job features' limits — the audience read's per-account outstanding-job budget,
// singleflight and slot, and the account write pacer — are process-local, so two pods each
// running them would each spend X's 100 concurrent jobs per account (shared with every
// foundation on the LF account) at full budget. The chart refuses to render more than one replica
// while TWITTER_METRICS_ENABLED is on, but it cannot see an out-of-band scale or an external HPA;
// this lease is the runtime guarantee.
type StatsJobLease interface {
	// Own returns nil when this process holds the lease for accountID — re-verifying a held lease
	// is still alive, and trying to acquire it otherwise (never waiting). A lease held elsewhere
	// is ErrStatsJobLeaseNotHeld; a database (or request) that cannot answer is
	// ErrStatsJobLeaseUnavailable. On either the caller must not create stats jobs.
	Own(ctx context.Context, accountID string) error
}
