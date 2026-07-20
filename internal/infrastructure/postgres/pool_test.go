// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"testing"

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
		"pgx5://u:p@host:5432/db":       "pgx5://u:p@host:5432/db",
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

func TestValidateMigrationDSN(t *testing.T) {
	// Valid URL DSNs pass (no connection is attempted).
	for _, ok := range []string{"postgres://app@host:5432/db?sslmode=disable", "postgresql://u:p@h/d", "pgx5://u@h/d"} {
		if err := ValidateMigrationDSN(ok); err != nil {
			t.Errorf("ValidateMigrationDSN(%q) = %v, want nil", ok, err)
		}
	}
	// A keyword DSN (no URL scheme) and a syntactically MALFORMED URL both fail up
	// front — the malformed one passes the prefix check but must be caught by the
	// parseability check, not deferred to NewPool/Migrate.
	for _, bad := range []string{"host=localhost user=u dbname=d", "postgres://[bad", "not a dsn at all"} {
		if err := ValidateMigrationDSN(bad); err == nil {
			t.Errorf("ValidateMigrationDSN(%q) = nil, want an error", bad)
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
