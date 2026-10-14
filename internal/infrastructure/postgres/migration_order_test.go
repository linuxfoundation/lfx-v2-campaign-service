// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"fmt"
	"io/fs"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// TestMigration000047_RequiresItsPredecessorsEmbedded is a MERGE-ORDER guard (LFXV2-2665). It was
// red on the bidding branch until #328 merged; it now pins that the order stays right.
//
// 000047_campaigns_bidding_strategy_column was numbered above 000043-000046, which the PR
// fix/LFXV2-2665-adoption-binding-indexes (#328) claims. golang-migrate records the highest
// applied version and never applies a lower one afterwards, so a binary that ships 000047 WITHOUT
// 000043-000046 would, once deployed, skip those four forever. allowedVersionGaps excuses the gap so
// the contiguity test can stay green on a branch ahead of its sibling — which is exactly why it
// cannot catch the wrong merge order. This test can: it reads the EMBEDDED migrations (what the
// binary actually applies) and fails while version 47 is present and any of 43-46 is absent.
func TestMigration000047_RequiresItsPredecessorsEmbedded(t *testing.T) {
	present := map[int]bool{}
	ups, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	for _, name := range ups {
		var v int
		if _, serr := fmt.Sscanf(name, "%06d_", &v); serr == nil {
			present[v] = true
		}
	}
	if !present[47] {
		return
	}
	for v := 43; v <= 46; v++ {
		if !present[v] {
			t.Errorf("embedded migrations carry 000047 but not %06d: golang-migrate would skip %06d permanently once 000047 is applied. "+
				"Merge fix/LFXV2-2665-adoption-binding-indexes (#328, 000043-000046) first and rebase this branch onto it.", v, v)
		}
	}
}
