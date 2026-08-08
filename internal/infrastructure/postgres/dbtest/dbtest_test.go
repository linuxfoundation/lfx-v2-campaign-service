// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest

import (
	"strings"
	"testing"
)

// TestVerdict is the guard against a harness that quietly does nothing.
//
// Every live test in this package skips when TEST_DATABASE_URL is unset, which is what
// keeps `go test ./...` working on a laptop with no database. The same property would let
// the whole live suite skip forever in CI after a workflow edit dropped the service
// container or the env block — and a skipped test reports success, so nothing would say so.
//
// This test runs EVERYWHERE, including on the laptop, because verdict takes its two inputs
// as arguments rather than reading the environment. A live test could not check the
// "CI without a database" case at all: that combination is precisely the one in which no
// database exists to run it against.
func TestVerdict(t *testing.T) {
	cases := []struct {
		name      string
		dsn       string
		ci        string
		wantRun   bool
		wantFatal bool
	}{
		{name: "configured off CI runs", dsn: "postgres://x", wantRun: true},
		{name: "configured on CI runs", dsn: "postgres://x", ci: "true", wantRun: true},
		{name: "unconfigured off CI skips", wantRun: false, wantFatal: false},
		{name: "unconfigured on CI fails", ci: "true", wantRun: false, wantFatal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal := verdict(tc.dsn, tc.ci)

			if gotRun := reason == ""; gotRun != tc.wantRun {
				t.Fatalf("run = %v (reason %q), want %v", gotRun, reason, tc.wantRun)
			}
			if fatal != tc.wantFatal {
				t.Fatalf("fatal = %v, want %v — an unconfigured CI runner must FAIL, not skip: "+
					"a skip there is a green build for a suite that never ran", fatal, tc.wantFatal)
			}
			if !tc.wantRun && !strings.Contains(reason, EnvDatabaseURL) {
				t.Errorf("reason = %q, want it to name %s so the fix is obvious from the log",
					reason, EnvDatabaseURL)
			}
		})
	}
}
