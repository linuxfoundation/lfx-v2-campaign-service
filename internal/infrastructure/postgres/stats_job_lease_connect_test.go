// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Opening the lease session runs under the lease's mutex, so it must be bounded by its own
// timeout: a database that accepts the TCP connection and never answers, reached through a DSN
// with no connect_timeout by a caller whose context has no deadline, must not hold the mutex
// until the OS gives up.
func TestStatsJobLease_SessionConnectIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }() // accept, then never answer the startup message
		}
	}()

	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://u@%s/db?sslmode=disable", ln.Addr()))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg) // lazy: connects nothing yet
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()

	lease := NewStatsJobLease(&Pool{Pool: pool})
	lease.connectTimeout = 200 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- lease.Own(context.Background(), "18ce54d4x5t") }()
	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrStatsJobLeaseUnavailable) {
			t.Fatalf("Own err = %v, want ErrStatsJobLeaseUnavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Own did not return: the session connect is not bounded")
	}
}
