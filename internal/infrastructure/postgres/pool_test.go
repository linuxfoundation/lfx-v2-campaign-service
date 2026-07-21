// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestPgxURL_RewritesURLSchemes(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@host:5432/db":   "pgx5://u:p@host:5432/db",
		"postgresql://u:p@host:5432/db": "pgx5://u:p@host:5432/db",
	}
	for in, want := range cases {
		got, err := pgxURL(in)
		if err != nil {
			t.Errorf("pgxURL(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("pgxURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPgxURL_RejectsKeywordDSN(t *testing.T) {
	// golang-migrate cannot consume a keyword DSN, so Migrate must reject it
	// rather than pass it through and fail obscurely at driver selection.
	if _, err := pgxURL("host=localhost user=u dbname=d"); err == nil {
		t.Error("pgxURL(keyword DSN) = nil error, want a clear rejection")
	}
}

func TestPgxURL_RejectsRawPgx5DSN(t *testing.T) {
	// "pgx5://" is golang-migrate's INTERNAL scheme, produced only by pgxURL's own
	// translation. NewPool opens the same DATABASE_URL via pgxpool.ParseConfig,
	// which cannot parse "pgx5://" — so accepting a raw "pgx5://" input would let it
	// clear ValidateMigrationDSN and then 503-loop forever on every pool open. It
	// must be rejected up front as a deterministic config error.
	if _, err := pgxURL("pgx5://u:p@host:5432/db"); err == nil {
		t.Error("pgxURL(pgx5:// DSN) = nil error, want a clear rejection")
	}
}

func TestValidateMigrationDSN(t *testing.T) {
	// Valid URL DSNs pass (no connection is attempted).
	for _, ok := range []string{"postgres://app@host:5432/db?sslmode=disable", "postgresql://u:p@h/d"} {
		if err := ValidateMigrationDSN(ok); err != nil {
			t.Errorf("ValidateMigrationDSN(%q) = %v, want nil", ok, err)
		}
	}
	// A keyword DSN (no URL scheme), a syntactically MALFORMED URL, and a raw
	// "pgx5://" DSN (migrate's internal scheme, which pgxpool can't open — see
	// TestPgxURL_RejectsRawPgx5DSN) all fail up front rather than 503-looping:
	// the malformed one passes the prefix check but must be caught by the
	// parseability check, not deferred to NewPool/Migrate.
	for _, bad := range []string{"host=localhost user=u dbname=d", "postgres://[bad", "not a dsn at all", "pgx5://u@h/d"} {
		if err := ValidateMigrationDSN(bad); err == nil {
			t.Errorf("ValidateMigrationDSN(%q) = nil, want an error", bad)
		}
	}
}

// A malformed credential-bearing DATABASE_URL must NOT surface the password (or the
// raw DSN) in the returned error — NewContainer propagates it and main logs it. pgx's
// own ParseConfigError redacts the password, but we don't depend on that: the error
// message is DSN-free (dsnParseError), while the parse cause stays reachable via
// errors.Unwrap for diagnostics.
func TestValidateMigrationDSN_ErrorDoesNotLeakSecret(t *testing.T) {
	const secret = "SUPERSECRETpw"
	// A URL-form DSN that carries a password but fails to parse (bad port).
	dsn := "postgres://user:" + secret + "@host:notaport/db"
	err := ValidateMigrationDSN(dsn)
	if err == nil {
		t.Fatal("expected an error for a malformed DSN")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "notaport") || strings.Contains(err.Error(), "user:") {
		t.Errorf("error message leaked DSN material: %q", err.Error())
	}
	// The underlying pgx parse error must still be reachable for diagnostics.
	if errors.Unwrap(err) == nil {
		t.Error("the parse cause should remain reachable via errors.Unwrap")
	}
}

func TestIsPermanentMigrationErr(t *testing.T) {
	// A dirty schema is permanent — retrying can never clear it (needs a manual force).
	dirty := fmt.Errorf("apply migrations: %w", migrate.ErrDirty{Version: 3})
	if !IsPermanentMigrationErr(dirty) {
		t.Error("a wrapped migrate.ErrDirty must be classified permanent")
	}
	// Connectivity / deadline / generic errors are NOT permanent (they should retry).
	for _, transient := range []error{
		errors.New("dial tcp: connection refused"),
		context.DeadlineExceeded,
		fmt.Errorf("open database pool: %w", errors.New("ping database: timeout")),
		nil,
	} {
		if IsPermanentMigrationErr(transient) {
			t.Errorf("a non-dirty error must NOT be permanent: %v", transient)
		}
	}
}

func withSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

func TestCheckReady_SuccessRecordsOKSpan(t *testing.T) {
	sr := withSpanRecorder(t)
	p := &Pool{}

	ok := p.checkReady(context.Background(), func(context.Context) error { return nil })
	require.True(t, ok)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "postgres.ready", spans[0].Name())
	assert.Equal(t, codes.Ok, spans[0].Status().Code)
}

func TestCheckReady_FailureRecordsErrorSpan(t *testing.T) {
	sr := withSpanRecorder(t)
	p := &Pool{}

	ok := p.checkReady(context.Background(), func(context.Context) error {
		return errors.New("boom")
	})
	require.False(t, ok)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "postgres.ready", spans[0].Name())
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	require.NotEmpty(t, spans[0].Events(), "expected RecordError event")
}

func TestCheckReady_PassesContextToPing(t *testing.T) {
	_ = withSpanRecorder(t)
	p := &Pool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sawCanceled bool
	ok := p.checkReady(ctx, func(c context.Context) error {
		sawCanceled = c.Err() != nil
		return c.Err()
	})
	require.False(t, ok)
	assert.True(t, sawCanceled)
}
