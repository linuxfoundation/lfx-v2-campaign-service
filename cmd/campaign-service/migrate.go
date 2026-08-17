// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// migrateCmd applies all pending schema migrations and exits. It is a subcommand of the
// SERVING binary for the same reason as bootstrap-system-account: ko publishes only
// cmd/campaign-service, so the ArgoCD PreSync Job runs THIS image with `migrate` rather than
// a separate artifact.
//
// It is the single writer of schema. The server no longer migrates at boot (it only
// VERIFIES the schema via postgres.VerifySchema).
//
// Be precise about what the PreSync ordering does and does not buy, because the intuitive
// reading is backwards. Running before the Deployment rolls does NOT protect the previous
// release from being migrated out from under it — that is exactly what happens: the hook
// completes while the OLD ReplicaSet is still serving, so the prior release runs against the
// NEW schema for the length of the rollout. What makes that overlap safe is that migrations
// are expand/contract — additive and backward-compatible with the release before them — which
// is the authoring rule this comment must not appear to relax.
//
// What the ordering genuinely buys is FAILURE HANDLING. A failure here fails the Job with
// logs and halts the sync, so the prior ReplicaSet keeps serving rather than a new pod
// crash-looping against a half-migrated database.
//
// Note what that does NOT promise: the database is not rolled back. golang-migrate marks the
// version dirty BEFORE running a migration's SQL, and statements that already committed stay
// committed — so a failed Job can leave the schema part-changed, with the prior release still
// pointed at it. Expand/contract is what keeps that survivable (the old binary only relies on
// what it already had); halting the sync is what keeps it from getting worse.
//
// The new pods then VERIFY rather than trust that: postgres.VerifySchema refuses to serve
// against a schema older than this binary requires, or one whose migration row is dirty. So a
// skipped or partially-applied Job surfaces as a pod that will not report ready, not as a pod
// serving queries against columns that do not exist.
const migrateCmd = "migrate"

// runMigrate applies every pending migration. args is the subcommand's own arguments
// (os.Args after the command word); it takes none.
func runMigrate(args []string) error {
	// No flags and no positional grammar: the command applies every pending migration.
	// Reject stray args rather than ignore them — Go's flag parser would swallow a typo
	// silently, and this Job's whole contract is "apply the migrations or fail loudly".
	if len(args) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", migrateCmd, args[0])
	}

	// The SERVER's resolver, not os.Getenv: in-cluster DATABASE_URL is unset and the DSN is
	// composed from PG* in-process, so the password never enters the pod spec.
	dsn, err := config.ResolveDatabaseURL()
	if err != nil {
		return fmt.Errorf("resolve database settings: %w", err)
	}
	if dsn == "" {
		return fmt.Errorf("no database configured; set PGHOST/PGUSER/PGPASSWORD/PGDATABASE or %s", constants.EnvDatabaseURL)
	}
	// Fail fast on a keyword or malformed DSN here rather than deep inside Migrate: it is a
	// deterministic config error no amount of running can fix.
	if err := postgres.ValidateMigrationDSN(dsn); err != nil {
		return err
	}

	slog.Info("applying database migrations")
	if err := postgres.Migrate(dsn); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	slog.Info("database migrations applied")
	return nil
}
