// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// captureSlog routes the default logger into a buffer for the test's duration.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// The probe's WARNs must never carry the DSN's identifiers or a credential: a pgx connect error
// repeats host/user/database, and a server error quotes the role. They keep only the step, the
// error's type, the SQLSTATE and a fixed diagnostic.
func TestProbeWarningsWithholdDSNIdentifiersAndCredentials(t *testing.T) {
	secrets := []string{"probeSecretUser", "s3cretPassw0rd", "secret-db-host.invalid", "probeSecretDB"}

	t.Run("connect failure", func(t *testing.T) {
		buf := captureSlog(t)
		dsn := "postgres://probeSecretUser:s3cretPassw0rd@secret-db-host.invalid:5432/probeSecretDB?connect_timeout=1"
		if got := probeRequiredIndexDeparse(dsn); got != nil {
			t.Fatalf("a probe that cannot connect returned %v, want the nil (text-comparison) fallback", got)
		}
		out := buf.String()
		if !strings.Contains(out, "step=connect") || !strings.Contains(out, "error_class=") || !strings.Contains(out, "withheld") {
			t.Fatalf("the connect WARN lost its step/class/diagnostic: %s", out)
		}
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("the connect WARN leaked %q: %s", s, out)
			}
		}
	})

	t.Run("server error during the probe", func(t *testing.T) {
		buf := captureSlog(t)
		probeFault = func(_ context.Context, step string) error {
			return &pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "probeSecretUser" on database "probeSecretDB"`}
		}
		t.Cleanup(func() { probeFault = nil })
		// The hook fails "begin" before the connection is touched, so no database is needed.
		serverDeparsedRequiredIndexes(context.Background(), nil)
		out := buf.String()
		if !strings.Contains(out, "step=begin") || !strings.Contains(out, "sqlstate=28P01") || !strings.Contains(out, "withheld") {
			t.Fatalf("the probe WARN lost its step/SQLSTATE/diagnostic: %s", out)
		}
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("the probe WARN leaked %q: %s", s, out)
			}
		}
	})
}
