// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"slices"
	"strings"
	"testing"
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

	missing, wrong, err := checkRequiredIndexes(ctx, conn)
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
	if _, wrong, err := checkRequiredIndexes(ctx, conn); err != nil || len(wrong) != 0 {
		t.Fatalf("a differently-SPELLED but identical definition was refused (wrong %v, err %v) — a Postgres upgrade changing the deparse would fail boot", wrong, err)
	}

	// A genuinely different definition: the account read from another key.
	requiredIndexes[i].keys = []string{"platform", "platform_campaign_id", `NULLIF(result ->> 'AccountID', '')`}
	_, wrong, err = checkRequiredIndexes(ctx, conn)
	if err != nil || len(wrong) != 1 || !strings.Contains(wrong[0], name) {
		t.Fatalf("a different definition was not refused: wrong %v, err %v", wrong, err)
	}
}
