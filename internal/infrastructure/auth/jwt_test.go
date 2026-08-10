// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	testIssuer   = "heimdall"
	testAudience = "lfx-v2-campaign-service"
	testKeyID    = "test-key"
)

// signer mints tokens as Heimdall does and serves the matching JWKS.
type signer struct {
	key     *rsa.PrivateKey
	jwksURL string
}

// newSigner starts a JWKS endpoint backed by a fresh RSA key. Served over HTTP because
// the FETCH is part of what is tested: an unreachable key set must refuse, not accept.
func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"kid": testKeyID,
			"alg": "PS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	return &signer{key: key, jwksURL: srv.URL + "/.well-known/jwks"}
}

// sign mints a PS256 token with overrides merged over a valid baseline; a nil value
// deletes the claim.
func (s *signer) sign(t *testing.T, overrides map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":       testIssuer,
		"aud":       testAudience,
		"sub":       "ada",
		"principal": "ada",
		"email":     "ada@lf.dev",
		"name":      "Ada Lovelace",
		"iat":       time.Now().Add(-time.Minute).Unix(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodPS256, claims)
	tok.Header["kid"] = testKeyID
	out, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return out
}

func (s *signer) verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := New(Config{JWKSURL: s.jwksURL, Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// TestVerifyActor_AcceptsAHeimdallToken also covers what Goa hands over — the raw header
// value, scheme included, whose scheme RFC 7235 makes case-insensitive.
func TestVerifyActor_AcceptsAHeimdallToken(t *testing.T) {
	s := newSigner(t)
	v := s.verifier(t)
	raw := s.sign(t, nil)
	for _, prefix := range []string{"", "Bearer ", "bearer ", "BEARER "} {
		actor, err := v.VerifyActor(context.Background(), prefix+raw)
		if err != nil {
			t.Fatalf("prefix %q rejected: %v", prefix, err)
		}
		if actor.Username != "ada" || actor.Email != "ada@lf.dev" || actor.Name != "Ada Lovelace" {
			t.Fatalf("actor = %+v, want the principal/email/name claims", actor)
		}
	}
}

// TestVerifyActor_Rejects is the security surface. Every case returns the SAME sentinel
// on purpose (see ErrUnauthenticated); the table exists so a later change to the
// validator options cannot quietly stop enforcing one of these.
func TestVerifyActor_Rejects(t *testing.T) {
	s := newSigner(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// signedWith mints a token with a key or algorithm the verifier must not accept.
	signedWith := func(method jwt.SigningMethod, key any) string {
		tok := jwt.NewWithClaims(method, jwt.MapClaims{
			"iss": testIssuer, "aud": testAudience, "principal": "ada",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = testKeyID
		out, serr := tok.SignedString(key)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return out
	}

	cases := map[string]string{
		"empty":                      "",
		"garbage":                    "not-a-jwt",
		"wrong audience":             s.sign(t, map[string]any{"aud": "lfx-v2-meeting-service"}),
		"wrong issuer":               s.sign(t, map[string]any{"iss": "https://evil.example"}),
		"expired":                    s.sign(t, map[string]any{"iat": time.Now().Add(-2 * time.Hour).Unix(), "exp": time.Now().Add(-time.Hour).Unix()}),
		"not yet valid":              s.sign(t, map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}),
		"foreign signature":          signedWith(jwt.SigningMethodPS256, other),
		"alg none":                   signedWith(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType),
		"alg confusion HS256":        signedWith(jwt.SigningMethodHS256, []byte("secret")),
		"missing principal":          s.sign(t, map[string]any{"principal": nil}),
		"blank principal":            s.sign(t, map[string]any{"principal": "   "}),
		"audience list without ours": s.sign(t, map[string]any{"aud": []string{"a", "b"}}),
		"no expiry":                  s.sign(t, map[string]any{"exp": nil}),
	}

	v := s.verifier(t)
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			actor, verr := v.VerifyActor(context.Background(), token)
			if verr == nil {
				t.Fatalf("accepted a token that is %s", name)
			}
			if !errors.Is(verr, ErrUnauthenticated) {
				t.Errorf("error does not wrap ErrUnauthenticated: %v", verr)
			}
			if actor != nil {
				t.Errorf("a refused token returned an actor: %+v", actor)
			}
		})
	}
}

// TestVerifyActor_UnreachableJWKSRefuses pins the fail-closed side of the key fetch; the
// tempting alternative, trusting claims until it recovers, is what this package replaced.
//
// It refuses as ErrKeyUnavailable, NOT ErrUnauthenticated, and the distinction is the
// whole point: the token was never checked, so nothing about it was established. The tag
// is applied inside coalesceKeyFunc and has to survive the validator wrapping it twice
// (validator.go:192 and :115, both %w) to be readable here — which is exactly what this
// asserts end to end.

func TestVerifyActor_UnreachableJWKSRefuses(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, nil)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	v, err := New(Config{JWKSURL: dead.URL + "/.well-known/jwks", Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = v.VerifyActor(context.Background(), token)
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable when the key set cannot be fetched", err)
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want NOT ErrUnauthenticated: the service maps that to 400, and a "+
			"caller holding a good token would be told their credential is bad and not to retry", err)
	}
}

// TestZeroVerifierRefuses pins the struct's own default: New never produces a zero
// Verifier, but a struct literal does, and it must not authenticate.
func TestZeroVerifierRefuses(t *testing.T) {
	var v Verifier
	if _, err := v.VerifyActor(context.Background(), "anything"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated from a zero Verifier", err)
	}
}

func TestNew_MockPrincipalBypassesVerification(t *testing.T) {
	v, err := New(Config{MockLocalPrincipal: "local-dev"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	actor, err := v.VerifyActor(context.Background(), "any old string")
	if err != nil {
		t.Fatalf("VerifyActor: %v", err)
	}
	if actor.Username != "local-dev" {
		t.Fatalf("username = %q, want the mock principal", actor.Username)
	}
	// A COPY: mutating it in place must not rewrite every later request's identity.
	actor.Username = "someone-else"
	again, err := v.VerifyActor(context.Background(), "x")
	if err != nil {
		t.Fatalf("VerifyActor: %v", err)
	}
	if again.Username != "local-dev" {
		t.Fatalf("username = %q after mutating an earlier actor; the mock is shared", again.Username)
	}
}

// TestNew_ConfigHandling pins both halves of the config contract: an empty field defaults
// (erroring instead would make every path that skips LoadConfig refuse all traffic) while
// a JWKS URL that was CONFIGURED and is wrong stops the pod rather than being defaulted
// over, which would hide the misconfiguration.
func TestNew_ConfigHandling(t *testing.T) {
	if _, err := New(Config{}); err != nil {
		t.Fatalf("New with an empty config: %v", err)
	}
	for _, bad := range []string{"lfx-platform-heimdall:4457/jwks", "/.well-known/jwks", "://nope", "ftp://h/jwks", "file:///jwks"} {
		if _, err := New(Config{JWKSURL: bad, Audience: testAudience, Issuer: testIssuer}); err == nil {
			t.Errorf("New accepted an unusable JWKS URL %q", bad)
		} else if !strings.Contains(err.Error(), "JWKS URL") {
			t.Errorf("error for %q does not name the JWKS URL: %v", bad, err)
		}
	}
}

// TestNew_MockPrincipalIsRefusedInCluster pins the guard that turns "local development
// only" from a comment into a control. The chart declares
// JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL under app.environment and deployment.yaml renders
// whatever it holds, so a `--set` on a real deploy would otherwise produce a pod that
// accepts any bearer token as this principal on the endpoints that spend money.
//
// The refusal must be an ERROR. Quietly ignoring the value and verifying for real would
// leave the request path safe and the values file still asking for no authentication —
// the next deploy of a build without this guard would ship the hole with nothing having
// ever complained.
func TestNew_MockPrincipalIsRefusedInCluster(t *testing.T) {
	v, err := New(Config{MockLocalPrincipal: "local-dev", InCluster: true})
	if err == nil {
		t.Fatal("New returned a verifier for a mock principal inside a cluster; a deployed " +
			"pod would accept any token as \"local-dev\"")
	}
	if v != nil {
		t.Errorf("New returned %v alongside the error; a caller ignoring err would still bypass", v)
	}
	// The operator has to find the values entry, so the message must name the key.
	if !strings.Contains(err.Error(), constants.EnvMockLocalPrincipal) {
		t.Errorf("error %q does not name %s, which is what an operator has to unset",
			err, constants.EnvMockLocalPrincipal)
	}

	// The guard is scoped to the cluster: the laptop workflow this switch exists for is
	// unchanged. Were InCluster ever inverted, this half fails rather than the guard
	// silently disabling local development.
	if _, oerr := New(Config{MockLocalPrincipal: "local-dev"}); oerr != nil {
		t.Fatalf("New outside a cluster: %v — the local-development bypass must still work", oerr)
	}
}

// TestCoalesceKeyFunc_CollapsesConcurrentColdFetches pins the fan-in.
//
// jwks.CachingProvider serializes cold misses without coalescing them: refreshKey takes
// the write lock and fetches WITHOUT re-checking the cache, so N simultaneous first
// requests each perform a full JWKS fetch, one after another. This wrapper must turn that
// into one fetch shared by all N.
// The count is EXACT, and getting there needed a different barrier than the obvious one.
// Signalling from the caller goroutine just before it enters the wrapper cannot establish
// that the caller is inside singleflight — every goroutine may be descheduled in that
// window, and once the fetch is released each one can then run to completion before the
// next enters, making the count exactly `callers` with the wrapper working perfectly. A
// range (1 <= n < callers) narrows that flake without removing it.
//
// The barrier that does work runs on the other side. `fetching` is signalled from INSIDE
// the fetch, so receiving it proves a flight is active, and it stays active because the
// fetch is parked on `release`. Followers then join deterministically: coalesceKeyFunc
// calls group.DoChan SYNCHRONOUSLY before its select, and DoChan on an in-flight key does
// not invoke fn — so a follower whose context is ALREADY cancelled has provably joined the
// leader's flight by the time it returns ctx.Err(), and its return is something the test
// can wait for. Sixteen followers join, none fetches, and the assertion is `== 1`.
func TestCoalesceKeyFunc_CollapsesConcurrentColdFetches(t *testing.T) {
	const followers = 16

	var fetches int32
	fetching := make(chan struct{})
	release := make(chan struct{})

	// Only the FIRST fetch parks. A second one is the defect this test exists to catch, and
	// parking it too would turn the failure into a hang (or, with an unguarded close, a
	// panic) instead of the count below.
	kf := coalesceKeyFunc(func(context.Context) (any, error) {
		if atomic.AddInt32(&fetches, 1) == 1 {
			close(fetching)
			<-release
		}
		return "keyset", nil
	})

	leader := make(chan any, 1)
	go func() {
		v, err := kf(context.Background())
		if err != nil {
			t.Errorf("leader: unexpected error %v", err)
		}
		leader <- v
	}()
	<-fetching // a flight is active, and parked on `release` until we say otherwise

	// Cancelled up front, so each of these returns only after DoChan has joined the flight.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	followerErrs := make([]error, followers)
	for i := range followers {
		_, followerErrs[i] = kf(ctx)
	}

	close(release)

	// The count first: a wrong follower error is usually the symptom of a missed join, and
	// reporting the join failure is more use than reporting sixteen copies of its effect.
	if n := atomic.LoadInt32(&fetches); n != 1 {
		t.Errorf("JWKS was fetched %d times while one fetch was already in flight and %d "+
			"further callers arrived, want exactly 1 — every one of them must join the "+
			"flight rather than start its own", n, followers)
	}
	for i, err := range followerErrs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("follower %d: got %v, want context.Canceled — a caller must stop waiting "+
				"on the shared fetch when its own context ends", i, err)
		}
	}
	if v := <-leader; v != "keyset" {
		t.Errorf("leader got %v, want the shared keyset", v)
	}
}

// TestCoalesceKeyFunc_EveryCallerGetsTheSharedResult is the other half, and it is a
// separate test because it is the half that CANNOT be made count-exact. Coalescing is
// invisible from a caller's side by design: whether it joined a flight or started one, it
// must come back with the keyset and no error. So this asserts only that — for every
// caller, under real concurrency, with no barrier that could bias the interleaving.
func TestCoalesceKeyFunc_EveryCallerGetsTheSharedResult(t *testing.T) {
	const callers = 16

	kf := coalesceKeyFunc(func(context.Context) (any, error) {
		return "keyset", nil
	})

	var wg sync.WaitGroup
	results := make([]any, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = kf(context.Background())
		}()
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Errorf("caller %d: unexpected error %v", i, errs[i])
		}
		if results[i] != "keyset" {
			t.Errorf("caller %d: got %v, want the shared keyset", i, results[i])
		}
	}
}

// TestCoalesceKeyFunc_CallerKeepsItsOwnDeadline is why this uses DoChan rather than Do.
// Do returns only when the shared call does, so one slow fetch would outlive a later
// caller's context. Each caller must stop waiting when ITS context ends.
func TestCoalesceKeyFunc_CallerKeepsItsOwnDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the in-flight fetch finish when the test ends

	kf := coalesceKeyFunc(func(context.Context) (any, error) {
		<-release
		return "keyset", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := kf(ctx); done <- err }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kf did not return after its context was cancelled; it is waiting on the shared fetch")
	}
}

// TestCoalesceKeyFunc_LeaderCancellationDoesNotFailFollowers covers the other half of
// that choice: the shared fetch must NOT be bound to whichever caller happened to arrive
// first, or that caller's cancellation would fail everyone waiting on it.
//
// The assertion that makes this binding is on the INNER context, not on the follower.
// Reading the regression off the follower's error cannot be made deterministic: the
// follower is a goroutine racing the leader's flight, and if it arrives after that flight
// completes it starts its own — with a live context, which succeeds and reports nothing,
// even with context.WithoutCancel removed. So the fetch records what its own ctx said at a
// moment the test controls: it parks on `release`, which is closed only AFTER the leader is
// cancelled, so by then a context carrying the leader's cancellation is definitely
// cancelled and one stripped of it is definitely not. The follower assertion stays as the
// behavioural half — it is what a user of this wrapper actually observes — but it is no
// longer what the guarantee rests on.
func TestCoalesceKeyFunc_LeaderCancellationDoesNotFailFollowers(t *testing.T) {
	fetching := make(chan struct{})
	release := make(chan struct{})
	fetched := make(chan struct{})

	// innerErr is written by the first fetch only and read after `fetched`, which that
	// fetch closes on its way out — so the handoff is ordered without a mutex.
	var innerErr error
	// Only the FIRST invocation parks. fn can legitimately run twice (a follower arriving
	// after the leader's flight completed starts its own), and parking the second would
	// turn this into a hang rather than the assertion below.
	var fetches int32
	kf := coalesceKeyFunc(func(ctx context.Context) (any, error) {
		if atomic.AddInt32(&fetches, 1) == 1 {
			close(fetching)
			<-release
			innerErr = ctx.Err()
			close(fetched)
		}
		return "keyset", nil
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() { _, _ = kf(leaderCtx) }() //nolint:errcheck // the leader's result is not under test
	<-fetching

	follower := make(chan error, 1)
	go func() { _, err := kf(context.Background()); follower <- err }()

	cancelLeader()
	close(release)
	<-fetched

	if innerErr != nil {
		t.Errorf("the shared fetch saw %v on its own context after the LEADER was cancelled, "+
			"want no error — the fetch is shared, so binding it to whichever caller happened "+
			"to arrive first makes that caller's cancellation everyone's", innerErr)
	}

	select {
	case err := <-follower:
		if err != nil {
			t.Errorf("follower failed because the LEADER was cancelled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower never returned")
	}
}
