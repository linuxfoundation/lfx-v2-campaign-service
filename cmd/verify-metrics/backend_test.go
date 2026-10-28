// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"strings"
	"testing"
)

// TestOpenReadOnlyPoolDoesNotLeakIdentifiers pins this command's half of #178.
//
// openReadOnlyPool is a hand-rolled sibling of postgres.NewPool — the same ParseConfig,
// NewWithConfig and Ping arms, in the same order — and when NewPool was fixed, only its
// ParseConfig arm here was. The other two returned the driver's error wrapped verbatim, and
// pgconn's ConnectError embeds a `user=%s database=%s` fragment built from the Config on every
// connection failure, so the username reached this command's stderr on the commonest failure
// there is. Its own doc comment claimed the DSN was never rendered into an error.
//
// It needs no database: the connection is refused by a closed local port, which is the arm a
// real run meets when the host or credentials are wrong. The password is asserted alongside the
// identifiers because a regression that scrubbed only the pgx template would still carry it.
func TestOpenReadOnlyPoolDoesNotLeakIdentifiers(t *testing.T) {
	t.Parallel()

	//nolint:gosec // synthetic DSN; no real credential.
	const dsn = "postgres://leakuser:s3cretpw@127.0.0.1:1/leakdb?sslmode=disable" // secretlint-disable-line -- synthetic DSN; the fixture asserting openReadOnlyPool names none of it

	pool, err := openReadOnlyPool(context.Background(), dsn)
	if err == nil {
		pool.Close()
		t.Fatal("openReadOnlyPool reported success against a closed port")
	}
	msg := err.Error()
	for _, forbidden := range []string{"leakuser", "leakdb", "s3cretpw", dsn} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("the open error reproduces %q: %s", forbidden, msg)
		}
	}
	// The withhold must still leave something to act on, and must name where to go and fix
	// it — the whole point of passing a source rather than a bare sentinel.
	if !strings.Contains(msg, "error_class=*pgconn.ConnectError") {
		t.Errorf("the withheld failure names no fault class: %s", msg)
	}
	if !strings.Contains(msg, dsnSourceVerifyMetrics) {
		t.Errorf("the withheld failure does not say where the DSN came from: %s", msg)
	}
}
