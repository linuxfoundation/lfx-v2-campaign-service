// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// A Take or Release whose checkout cannot connect fails with pgconn text that names the
// configured user, host and database; the ledger's errors reach a warning log, so they must not
// carry any of the three (nor the password).
func TestRedditReportLedger_ErrorsDoNotCarryTheDSN(t *testing.T) {
	const dsn = "postgres://ledgeruser9:ledgerpass9@127.0.0.1:1/ledgerdb9?sslmode=disable&connect_timeout=1" // secretlint-disable-line -- intentional fake DSN, asserting the ledger's errors withhold it
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(p.Close)
	l := NewRedditReportLedger(&Pool{Pool: p})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, takeErr := l.Take(ctx, "t2_x", false, domain.RedditReportLimits{Window: time.Minute, Total: 45, Insights: 24})
	relErr := l.Release(ctx, 1)
	for name, err := range map[string]error{"Take": takeErr, "Release": relErr} {
		if err == nil {
			t.Fatalf("%s against an unreachable database succeeded", name)
		}
		for _, secret := range []string{"ledgeruser9", "ledgerpass9", "ledgerdb9"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s error carries %q: %v", name, secret, err)
			}
		}
	}
}
