// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestReportUnreconciledAudience_SeparatesAConfirmedHeldLeaseFromAProbableRollback pins the
// half of the Cursor finding on PR #106 that the live tests cannot reach.
//
// The finding was that a reconcile which had CONFIRMED the row present and still 'building'
// reported the same thing as one that never saw a row at all. Those are not the same event: the
// first is a lease this process knows is held, with no automatic path left to release it
// (migration 000018's escape hatch is deliberately manual), and the second is the ordinary
// rolled-back commit that happens whenever a Commit error really was a rollback. Logging the
// first at warn, in the second's hedged "if the commit did land" wording, buries a real stranded
// lease inside the routine case.
//
// This is a plain unit test rather than a live one on purpose. Reaching audienceReconcileHeld
// through the database requires the row to become visible in the microseconds BETWEEN an
// attempt's UPDATE and its classifying SELECT, on the LAST attempt — a window nothing can
// schedule into reliably. The classification itself is what the finding was about, and it is
// pure: given confirmedHeld, pick the level and the wording. Test it directly.
func TestReportUnreconciledAudience_SeparatesAConfirmedHeldLeaseFromAProbableRollback(t *testing.T) {
	row := &model.CampaignAudience{
		ID:        "aud-1",
		BriefID:   "brief-1",
		ProjectID: "cncf",
		Platform:  model.ProviderHubSpot,
	}

	tests := []struct {
		name          string
		confirmedHeld bool
		wantLevel     string
		wantPhrase    string
		notWant       string
	}{
		{
			name:          "confirmed held is an error and states the fact",
			confirmedHeld: true,
			wantLevel:     "level=ERROR",
			wantPhrase:    "CONFIRMED present and still 'building'",
			// The hedge belongs to the other case. Carrying it here is the defect: it invites
			// the reader to treat a known-held lease as a maybe.
			notWant: "if the commit did land",
		},
		{
			name:          "never seen is a warn and hedges",
			confirmedHeld: false,
			wantLevel:     "level=WARN",
			wantPhrase:    "if the commit did land",
			notWant:       "CONFIRMED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			reportUnreconciledAudience(context.Background(), row, 6, tc.confirmedHeld,
				"could not confirm an audience row's fate after an ambiguous commit error")

			got := buf.String()
			if !strings.Contains(got, tc.wantLevel) {
				t.Errorf("log line is not at %s — a confirmed held lease and a probable rollback "+
					"must not share a level, or the real one is unfindable:\n%s", tc.wantLevel, got)
			}
			if !strings.Contains(got, tc.wantPhrase) {
				t.Errorf("log line does not contain %q:\n%s", tc.wantPhrase, got)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("log line contains %q, which belongs to the other case:\n%s", tc.notWant, got)
			}
			// The operator has to find the row to run the manual escape hatch on it.
			for _, attr := range []string{"audience_id=aud-1", "brief_id=brief-1", "project_id=cncf", "attempts=6"} {
				if !strings.Contains(got, attr) {
					t.Errorf("log line does not carry %q, so the escape hatch has nothing to act on:\n%s", attr, got)
				}
			}
		})
	}
}
