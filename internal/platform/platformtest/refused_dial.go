// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package platformtest holds test support shared by the platform clients' and dispatch's tests.
// It is imported only from _test.go files; nothing in production code depends on it.
package platformtest

import (
	"context"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// RefusedURL names a host RefusedDialTransport never dials: every connection to it is
// refused deterministically. The .invalid TLD (RFC 6761) cannot resolve, so the address
// can never reach a real listener even outside this transport.
//
// It replaces closing an httptest server and reusing its URL (or hard-coding
// 127.0.0.1:1): between Close and the dial the OS may hand that port to another
// listener, and the request is then answered instead of refused (issue #206).
const RefusedURL = "http://refused.invalid"

// refusedAddr is the host:port http.Transport passes to DialContext for RefusedURL.
const refusedAddr = "refused.invalid:80"

// RefusedDialTransport is a real *http.Transport whose dial to refusedAddr fails with the
// exact error a closed port produces — a *net.OpError{Op: "dial"} wrapping ECONNREFUSED in
// an *os.SyscallError — so the error reaches the client wrapped as a real refusal would be
// and the production classifiers match it through errors.Is/errors.As. Every other address
// is dialed normally, so token and API test servers keep working through the same client.
//
// The test fails at cleanup if no dial was refused: refused.invalid cannot resolve, so a
// client that bypassed this transport would fail with a *net.DNSError instead — which the
// classifiers also call pre-send — and the test would pass without proving the refusal.
func RefusedDialTransport(t *testing.T) *http.Transport {
	t.Helper()
	var d net.Dialer
	var refused atomic.Int32
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == refusedAddr {
				refused.Add(1)
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
			}
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		if refused.Load() == 0 {
			t.Error("RefusedDialTransport: no dial to RefusedURL was refused; the client did not use this transport")
		}
	})
	return tr
}
