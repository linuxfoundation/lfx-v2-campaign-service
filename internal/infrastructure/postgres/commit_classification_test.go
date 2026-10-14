// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestClassifyCommitErr pins which COMMIT failures leave the outcome unknown
// (domain.ErrWriteOutcomeUnknown) and which confirm a rollback (PR #309). A PgError is a confirmed
// rollback UNLESS its SQLSTATE itself says the outcome is not known: 08007, 40003, the rest of the
// connection-exception class 08, and a server shutting down around the COMMIT.
func TestClassifyCommitErr(t *testing.T) {
	pg := func(code string) error {
		return fmt.Errorf("commit: %w", &pgconn.PgError{Code: code, Message: "test"})
	}
	for _, tc := range []struct {
		name    string
		err     error
		unknown bool
	}{
		{"08007 transaction_resolution_unknown", pg("08007"), true},
		{"40003 statement_completion_unknown", pg("40003"), true},
		{"08000 connection_exception", pg("08000"), true},
		{"08003 connection_does_not_exist", pg("08003"), true},
		{"08006 connection_failure", pg("08006"), true},
		{"08P01 protocol_violation", pg("08P01"), true},
		{"57P01 admin_shutdown", pg("57P01"), true},
		{"57P02 crash_shutdown", pg("57P02"), true},
		{"lost acknowledgement (I/O)", fmt.Errorf("commit: %w", io.ErrUnexpectedEOF), true},
		{"context ended", context.DeadlineExceeded, true},
		{"P0001 raise from a deferred trigger", pg("P0001"), false},
		{"23505 deferred unique violation", pg("23505"), false},
		{"40001 serialization failure", pg("40001"), false},
		{"57014 query_canceled", pg("57014"), false},
		{"server reported the rollback", pgx.ErrTxCommitRollback, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := errors.Is(classifyCommitErr("replace campaign", tc.err), domain.ErrWriteOutcomeUnknown)
			if got != tc.unknown {
				t.Fatalf("ErrWriteOutcomeUnknown = %v, want %v", got, tc.unknown)
			}
		})
	}
}
