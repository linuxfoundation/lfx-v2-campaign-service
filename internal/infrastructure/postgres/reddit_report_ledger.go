// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// redditReportLedgerClass is the first key of the two-int transaction advisory lock a Take holds:
// "RRPT". The class keeps it apart from every other two-int lock in this package — the X
// stats-job lease ("XSTJ"), the keyword-targeting lock ("RKTW"), the campaign slot lock
// (campaignSlotLockNamespace) and the archived-asset prune ("CAPR") — and the two-int form is a
// separate key space from the one-bigint form.
const redditReportLedgerClass int32 = 0x52525054

// redditReportLedgerTimeout bounds one Take or Release: the lock wait, the statements and the
// commit. A Take holds the lock for two short statements, so a wait this long means the database
// is not answering — and the attempt is refused (fail closed) rather than left to the request's
// own, much longer, deadline.
const redditReportLedgerTimeout = 5 * time.Second

// redditReportLedgerPruneLimit bounds the expired rows one Take deletes, so a backlog (after an
// idle spell, or a restore) is cleared over several Takes instead of inside one.
const redditReportLedgerPruneLimit = 500

// redditReportLedgerLockBusyWait is the Wait a Take returns when another Take on the account
// holds the lock: the time to hand the connection back and ask again, not a budget answer. Two
// short statements is all the holder does, so this is generous.
const redditReportLedgerLockBusyWait = 20 * time.Millisecond

const (
	redditReportLedgerLockSQL = "SELECT pg_try_advisory_xact_lock($1, $2)"

	// redditReportLedgerTakeSQL counts the account's slots inside the window and inserts one when
	// both limits have room, in ONE statement on ONE clock reading (now, MATERIALIZED so every
	// reference sees the same value). It runs after the lock statement, so its snapshot — taken
	// at statement start under READ COMMITTED — sees every slot a previous holder committed.
	// It returns the new slot's id (NULL when full) and, when full, the seconds until the
	// earliest blocking slot leaves the window.
	//
	// The prune deletes expired rows of ANY account, skipping rows another Take is deleting
	// (SKIP LOCKED) so two prunes never wait on each other. An expired row is outside every
	// window, so deleting it — or not — changes no count. The count does not rely on it: it
	// filters by the window itself (a data-modifying CTE's effect is not visible to the rest of
	// the statement).
	//
	// $1 account, $2 insights, $3 window seconds, $4 total, $5 insights limit, $6 prune limit.
	redditReportLedgerTakeSQL = `
WITH t AS MATERIALIZED (SELECT clock_timestamp() AS now),
pruned AS (
    DELETE FROM reddit_report_slots
    WHERE id IN (
        SELECT s.id FROM reddit_report_slots s, t
        WHERE s.taken_at <= t.now - make_interval(secs => $3)
        LIMIT $6
        FOR UPDATE OF s SKIP LOCKED
    )
),
w AS (
    SELECT count(*) AS all_n,
           count(*) FILTER (WHERE s.insights) AS ins_n,
           min(s.taken_at) AS all_oldest,
           min(s.taken_at) FILTER (WHERE s.insights) AS ins_oldest
    FROM reddit_report_slots s, t
    WHERE s.ad_account_id = $1 AND s.taken_at > t.now - make_interval(secs => $3)
),
ins AS (
    INSERT INTO reddit_report_slots (ad_account_id, insights, taken_at)
    SELECT $1, $2, t.now FROM t, w
    WHERE w.all_n < $4 AND (NOT $2 OR w.ins_n < $5)
    RETURNING id
)
SELECT (SELECT id FROM ins),
       GREATEST(
           CASE WHEN w.all_n >= $4
                THEN EXTRACT(EPOCH FROM (w.all_oldest + make_interval(secs => $3) - t.now)) ELSE 0 END,
           CASE WHEN $2 AND w.ins_n >= $5
                THEN EXTRACT(EPOCH FROM (w.ins_oldest + make_interval(secs => $3) - t.now)) ELSE 0 END
       )::float8
FROM t, w`

	redditReportLedgerReleaseSQL = "DELETE FROM reddit_report_slots WHERE id = $1"
)

// RedditReportLedger implements domain.RedditReportLedger on the reddit_report_slots table
// (migration 000055): one row per reporting attempt, so every replica counts every replica's
// attempts and the 45-a-minute budget is account-wide (#380).
//
// ATOMIC PER ACCOUNT. A Take is one transaction: pg_try_advisory_xact_lock on the account's key,
// then one statement that counts and inserts. Two Takes on one account — on any replicas —
// therefore run one after the other, and the second counts the first's slot; without the lock two
// concurrent counts could both see 44 and both insert. The lock is the TRANSACTION form, released
// by the commit, so it can never be stranded on a pooled connection; it is held for two short
// statements, never across a Reddit call.
//
// TRY, NEVER BLOCK — the keyword-targeting lock's rule. A blocked pg_advisory_xact_lock would pin
// a pooled connection for as long as the holder takes (a stalled holder: up to the 5 s bound, on
// every replica), and refused readers that wake together would fill the pool queued on one lock.
// A Take that finds the lock held rolls back at once — the connection goes back to the pool — and
// answers Taken false with redditReportLedgerLockBusyWait; reddit.ReportBudgets' wait loop (with
// jitter) asks again.
//
// THE DATABASE'S CLOCK. Slots are stamped and the window measured with clock_timestamp(), read
// after the lock is granted — never a pod's clock, which can skew between replicas, and never
// now(), which is the transaction's START and would back-date a Take that waited for the lock.
//
// FAILS CLOSED. Every error — no pool, a failed checkout, the lock or statement failing, the
// redditReportLedgerTimeout running out — is returned, and reddit.ReportBudgets refuses the
// attempt (ErrReportBudgetUnavailable): nothing is sent unbudgeted.
//
// KEYS, NOT ACCOUNTS. The lock key is a 32-bit FNV of the account id. Two accounts that collide
// share a lock, which only serializes their Takes; the COUNT is always by account id, so neither
// spends the other's budget.
type RedditReportLedger struct {
	pool    *Pool
	timeout time.Duration
}

// NewRedditReportLedger returns the ledger on pool.
func NewRedditReportLedger(pool *Pool) *RedditReportLedger {
	return &RedditReportLedger{pool: pool, timeout: redditReportLedgerTimeout}
}

var _ domain.RedditReportLedger = (*RedditReportLedger)(nil)

// redditReportLedgerKey is the second lock key for accountID: FNV-1a over the id, folded to int32.
func redditReportLedgerKey(accountID string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountID))
	return int32(h.Sum32()) //nolint:gosec // a hash folded to the lock's int4 key, overflow intended
}

// RedditReportLedgerLockKeys returns the two advisory-lock keys a Take on accountID takes, for
// the live tests.
func RedditReportLedgerLockKeys(accountID string) (int32, int32) {
	return redditReportLedgerClass, redditReportLedgerKey(accountID)
}

var errNoReportLedgerPool = errors.New("reddit report ledger: no database pool")

// Take implements domain.RedditReportLedger. See the type comment.
func (l *RedditReportLedger) Take(ctx context.Context, accountID string, insights bool, limits domain.RedditReportLimits) (domain.RedditReportTake, error) {
	if l == nil || l.pool == nil || l.pool.Pool == nil {
		return domain.RedditReportTake{}, errNoReportLedgerPool
	}
	if limits.Window <= 0 || limits.Total <= 0 || limits.Insights < 0 {
		return domain.RedditReportTake{}, fmt.Errorf("reddit report ledger: invalid limits %+v", limits)
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	var (
		id     *int64
		wait   float64
		locked bool
	)
	err := pgx.BeginFunc(ctx, l.pool.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, redditReportLedgerLockSQL, redditReportLedgerClass, redditReportLedgerKey(accountID)).Scan(&locked); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if !locked {
			return nil // held by another Take: commit the empty transaction and ask again
		}
		if err := tx.QueryRow(ctx, redditReportLedgerTakeSQL, accountID, insights, limits.Window.Seconds(),
			limits.Total, limits.Insights, redditReportLedgerPruneLimit).Scan(&id, &wait); err != nil {
			return fmt.Errorf("take: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.RedditReportTake{}, fmt.Errorf("reddit report ledger: %w", err)
	}
	if !locked {
		return domain.RedditReportTake{Wait: redditReportLedgerLockBusyWait}, nil
	}
	if id == nil {
		return domain.RedditReportTake{Wait: time.Duration(wait * float64(time.Second))}, nil
	}
	return domain.RedditReportTake{SlotID: *id, Taken: true}, nil
}

// Release implements domain.RedditReportLedger: it deletes the slot's row. Deleting a row only
// lowers a count, so it needs no lock; a slot already pruned is a no-op.
func (l *RedditReportLedger) Release(ctx context.Context, slotID int64) error {
	if l == nil || l.pool == nil || l.pool.Pool == nil {
		return errNoReportLedgerPool
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	if _, err := l.pool.Exec(ctx, redditReportLedgerReleaseSQL, slotID); err != nil {
		return fmt.Errorf("reddit report ledger: release: %w", err)
	}
	return nil
}
