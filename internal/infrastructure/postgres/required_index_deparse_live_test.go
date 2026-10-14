// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveRequiredIndexCheckComparesTheServersOwnDeparse pins that the boot check does not depend
// on the TEXT a definition was captured in. A stored definition spelled differently from the live
// server's deparse — exactly what a PostgreSQL major upgrade that changes the rendering of the
// 000044-000046 expression keys would produce — must still be accepted, because the check compares
// the server's deparse of BOTH sides; a genuinely different definition must still be refused.
func TestLiveRequiredIndexCheckComparesTheServersOwnDeparse(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	conn := c.Conn()

	missing, wrong, err := checkRequiredIndexes(ctx, conn, serverDeparsedRequiredIndexes(ctx, conn))
	if err != nil || len(missing) != 0 || len(wrong) != 0 {
		t.Fatalf("migrated schema: missing %v, wrong %v, err %v", missing, wrong, err)
	}

	const name = "uq_campaigns_microsoft_campaign_live"
	i := slices.IndexFunc(requiredIndexes, func(r requiredIndex) bool { return r.name == name })
	if i < 0 {
		t.Fatalf("%s is not in requiredIndexes", name)
	}
	saved := requiredIndexes[i]
	t.Cleanup(func() { requiredIndexes[i] = saved })

	// The MIGRATION's spelling (no casts, unquoted substring, FROM syntax, unparenthesised
	// predicate) — text that differs from PG16's deparse at every turn, yet the same definition.
	requiredIndexes[i].keys = []string{"platform", "platform_campaign_id",
		`COALESCE(NULLIF(result ->> 'accountId', ''), NULLIF(substring(result ->> 'microsoftAdsUrl' from '[?&]aid=([^&#]+)'), ''))`}
	requiredIndexes[i].predicate = "status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'microsoft-ads'"
	if _, wrong, err := checkRequiredIndexes(ctx, conn, serverDeparsedRequiredIndexes(ctx, conn)); err != nil || len(wrong) != 0 {
		t.Fatalf("a differently-SPELLED but identical definition was refused (wrong %v, err %v) — a Postgres upgrade changing the deparse would fail boot", wrong, err)
	}

	// A genuinely different definition: the account read from another key.
	requiredIndexes[i].keys = []string{"platform", "platform_campaign_id", `NULLIF(result ->> 'AccountID', '')`}
	_, wrong, err = checkRequiredIndexes(ctx, conn, serverDeparsedRequiredIndexes(ctx, conn))
	if err != nil || len(wrong) != 1 || !strings.Contains(wrong[0], name) {
		t.Fatalf("a different definition was not refused: wrong %v, err %v", wrong, err)
	}
}

// TestLiveRequiredIndexCheckFallsBackWhenTheProbeCannotRun: a production role may lack the
// TEMPORARY privilege, run read-only, or sit on a hot standby. Whichever probe step fails, the
// check must fall back to the stored-text comparison — NOT fail boot — so it is never stricter
// than before the probe existed: a correct schema still passes, and a genuinely different
// definition is still refused.
func TestLiveRequiredIndexCheckFallsBackWhenTheProbeCannotRun(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	conn := c.Conn()

	const name = "uq_campaigns_microsoft_campaign_live"
	i := slices.IndexFunc(requiredIndexes, func(r requiredIndex) bool { return r.name == name })
	saved := requiredIndexes[i]
	t.Cleanup(func() { requiredIndexes[i] = saved; probeFault = nil })

	for _, step := range []string{"begin", "create temp table", "savepoint", "read probe"} {
		t.Run(step, func(t *testing.T) {
			calls := 0
			probeFault = func(_ context.Context, s string) error {
				if s == step {
					calls++
					return errors.New("permission denied to create temporary tables in database (simulated)")
				}
				return nil
			}
			defer func() { probeFault = nil }()

			requiredIndexes[i] = saved
			missing, wrong, err := checkRequiredIndexes(ctx, conn, serverDeparsedRequiredIndexes(ctx, conn))
			if err != nil || len(missing) != 0 || len(wrong) != 0 {
				t.Fatalf("correct schema with the probe failing at %q: missing %v, wrong %v, err %v — the probe made boot STRICTER", step, missing, wrong, err)
			}
			if calls == 0 {
				t.Fatalf("the %q step was never reached, so the fallback was not exercised", step)
			}

			requiredIndexes[i].keys = []string{"platform", "platform_campaign_id", `NULLIF(result ->> 'AccountID'::text, ''::text)`}
			_, wrong, err = checkRequiredIndexes(ctx, conn, serverDeparsedRequiredIndexes(ctx, conn))
			if err != nil || len(wrong) != 1 || !strings.Contains(wrong[0], name) {
				t.Fatalf("a different definition with the probe failing at %q was not refused: wrong %v, err %v", step, wrong, err)
			}
		})
	}
}

// TestLiveSlowProbeCannotSpendTheRequiredChecksDeadline: the optional deparse probe runs on its OWN
// connection under its OWN short budget (probeBudget) before the required checks' deadline starts.
// Here every probe step blocks until that budget is gone — a held lock or a slow server — and the
// boot check must still pass on a correct schema, within roughly the probe budget plus the real
// checks, rather than failing because the probe ate the required checks' time.
func TestLiveSlowProbeCannotSpendTheRequiredChecksDeadline(t *testing.T) {
	creativeAssetTestPool(t) // migrates, and skips/fails exactly as the other live tests do
	dsn := os.Getenv("TEST_DATABASE_URL")
	t.Cleanup(func() { probeFault = nil })
	blocked := 0
	probeFault = func(ctx context.Context, step string) error {
		blocked++
		<-ctx.Done() // a step that never completes inside the probe's budget
		return ctx.Err()
	}
	start := time.Now()
	if err := checkNoInvalidIndexes(dsn); err != nil {
		t.Fatalf("boot check with a probe that blocks until its budget is spent: %v — the optional probe consumed the required checks' deadline", err)
	}
	if blocked == 0 {
		t.Fatal("the probe hook was never reached")
	}
	if elapsed := time.Since(start); elapsed > probeBudget+invalidIndexCheckTimeout {
		t.Fatalf("boot check took %v; the probe was not bounded by its own budget", elapsed)
	}
}
