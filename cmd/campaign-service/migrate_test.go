// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

// TestRunMigrateRejectsResidualArguments pins the stray-argument refusal. The migrate Job's
// grammar has no positional arguments, and Go's flag parser would swallow a typo silently, so
// anything left over is a mistake the Job must fail on rather than ignore. This is the
// deterministic path — it runs before any DATABASE_URL resolution, so it needs no database.
func TestRunMigrateRejectsResidualArguments(t *testing.T) {
	err := runMigrate([]string{"extra"})
	if err == nil {
		t.Fatal("runMigrate([extra]) = nil error, want a refusal of the stray argument")
	}
	if !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("error = %v, want it to name the stray-argument refusal", err)
	}
}

// TestRunCommandDispatchesMigrate confirms runCommand ROUTES the migrate subcommand rather
// than falling through to server startup or the unknown-command path. It passes a stray
// argument so the check completes before any DATABASE_URL resolution, keeping the test
// deterministic and DB-free while still proving the command is dispatched (handled=true,
// code=1 from the argument refusal — not code 2 "unknown command").
func TestRunCommandDispatchesMigrate(t *testing.T) {
	var stderr strings.Builder
	handled, code := runCommand([]string{migrateCmd, "extra"}, &stderr)
	if !handled || code != 1 {
		t.Fatalf("runCommand(migrate extra) = (handled %v, code %d), want (true, 1)", handled, code)
	}
	if !strings.Contains(stderr.String(), "takes no arguments") {
		t.Fatalf("stderr = %q, want the stray-argument refusal", stderr.String())
	}
}
