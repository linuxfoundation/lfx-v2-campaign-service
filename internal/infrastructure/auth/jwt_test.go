// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		writeKeySet(w, key)
	}))
	t.Cleanup(srv.Close)
	return &signer{key: key, jwksURL: srv.URL + "/.well-known/jwks"}
}

// writeKeySet serves the JWKS Heimdall would serve for key.
func writeKeySet(w http.ResponseWriter, key *rsa.PrivateKey) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"kid": testKeyID,
		"alg": "PS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
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

// TestVerifyActor_JWKSErrorBodyIsNotMistakenForAKeySet is the 200-shaped failure the
// status guard exists for, and it is the one that is NOT self-announcing: a 500 with an
// empty body fails the JSON decode on its own (see the test above), so the middleware
// surfaces it either way. A 404 carrying `{"error":"not found"}` — a wrong JWKS path, a
// proxy in front of Heimdall answering for it — decodes CLEANLY, because
// jose.JSONWebKeySet has one field and ignores every other. Without the guard the
// provider returns a zero-key set as a SUCCESS, CachingProvider stores it for the full
// five-minute TTL, and every caller holding a perfectly good token is told for five
// minutes that their credential is bad (HTTP 400) over a misconfiguration that is ours.
//
// The second half is the part the guard buys beyond the status code: because the failure
// is a transport error rather than a successful fetch, nothing is cached, so the very
// next request recovers the moment the endpoint does. Recovery inside the TTL is the
// assertion — with a poisoned cache it could not happen at all.
func TestVerifyActor_JWKSErrorBodyIsNotMistakenForAKeySet(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			writeKeySet(w, key)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	s := &signer{key: key, jwksURL: srv.URL + "/.well-known/jwks"}
	token := s.sign(t, nil)
	v := s.verifier(t)

	_, err = v.VerifyActor(context.Background(), token)
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable: a 404 error object is not a key set", err)
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want NOT ErrUnauthenticated: the token was never checked", err)
	}
	// The status has to reach the operator. The empty-key-set check ALSO catches this
	// case, which is why the sentinel above cannot distinguish the two — but it can only
	// say "no signing keys", which describes a healthy issuer mid-rotation just as well
	// as it describes a 404 on a mistyped path. The status is what names the actual
	// fault, and only the status arm has it. Without it this assertion is the one that
	// fails, and the operator is left diagnosing a wrong URL from a message about key
	// rotation.
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v, want the HTTP status in the message: it is what identifies the "+
			"misconfiguration as a wrong JWKS endpoint rather than an issuer with no keys", err)
	}

	healthy.Store(true)
	if _, err = v.VerifyActor(context.Background(), token); err != nil {
		t.Fatalf("VerifyActor after the endpoint recovered: %v — the failed fetch must not "+
			"have been cached, or recovery waits out the %s TTL", err, jwksCacheTTL)
	}
}

// TestVerifyActor_EmptyKeySetIsUnavailable covers the 200-shaped failure with no status
// anomaly at all: `{"keys":[]}` is what an issuer mid-rotation with nothing published
// returns. A key set with no keys cannot verify anything, so returning it as a success
// means every token that arrives is refused as bad — 400 for what is squarely a 503.
//
// The recovery half is the load-bearing assertion, and it is what forced the check down
// into the transport. CachingProvider.refreshKey STORES the decoded set before returning
// it (jwks/provider.go:207-221), so a check wrapped around provider.KeyFunc rejects an
// empty set only AFTER it is cached: every request answers 503 for the full TTL even
// though the endpoint recovered seconds later. Rejecting inside the RoundTripper makes
// the fetch an error, and an error is never cached. Move the check back up and this test
// hangs on the jwksCacheTTL rather than passing.
func TestVerifyActor_EmptyKeySetIsUnavailable(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			writeKeySet(w, key)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer srv.Close()

	s := &signer{key: key, jwksURL: srv.URL + "/.well-known/jwks"}
	token := s.sign(t, nil)
	v := s.verifier(t)

	_, err = v.VerifyActor(context.Background(), token)
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable: a key set with no keys cannot verify a token, "+
			"so this is our outage and not a verdict on the credential", err)
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want NOT ErrUnauthenticated", err)
	}

	healthy.Store(true)
	if _, err = v.VerifyActor(context.Background(), token); err != nil {
		t.Fatalf("VerifyActor after the issuer published keys: %v — the empty set must not "+
			"have been cached, or recovery waits out the %s TTL", err, jwksCacheTTL)
	}
}

// TestVerifyActor_UndecodableSuccessBodyIsUnavailable is the other 2xx shape: a 200 whose
// body is not a key-set object at all — an HTML sign-in page from a proxy that intercepted
// the request, say. The middleware's own decode would fail on it too, but with a JSON
// syntax error attributed to nothing in particular; naming the endpoint is what makes it
// diagnosable, and doing it here is what keeps the 2xx path's failures uniform.
func TestVerifyActor_UndecodableSuccessBodyIsUnavailable(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>sign in</html>`))
	}))
	defer srv.Close()
	v, err := New(Config{JWKSURL: srv.URL + "/.well-known/jwks", Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = v.VerifyActor(context.Background(), token)
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable for a 2xx body that is not a key set", err)
	}
	if strings.Contains(err.Error(), "sign in") {
		t.Errorf("err = %v, must not quote the upstream body: it is untrusted text bound for logs", err)
	}
}

// TestVerifyActor_OversizeKeySetIsRefused pins the bound rather than a truncation. A
// truncated key set decodes to FEWER keys, which is the empty-set failure in slow motion —
// so the read is capped at jwksMaxBody+1 and one byte over is an error, not a short read.
func TestVerifyActor_OversizeKeySetIsRefused(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[],"pad":"`))
		pad := bytes.Repeat([]byte("a"), 64*1024)
		for range 17 {
			_, _ = w.Write(pad)
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	v, err := New(Config{JWKSURL: srv.URL + "/.well-known/jwks", Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = v.VerifyActor(context.Background(), token)
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable for an oversize JWKS body", err)
	}
	if !strings.Contains(err.Error(), "more than") {
		t.Errorf("err = %v, want the size bound named: a body refused for its LENGTH and one "+
			"refused for having no keys are different faults", err)
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
		} else if !strings.Contains(err.Error(), constants.EnvJWKSURL) {
			t.Errorf("error for %q does not name the config key that set it: %v", bad, err)
		}
	}
}

// TestNew_RejectionDoesNotLeakURLCredentials pins that a rejected JWKS URL is never
// rendered verbatim.
//
// A URL may carry credentials in its userinfo, and main.go LOGS whatever New returns, so a
// `%q` of the raw value writes the password to the log of every pod that fails to start —
// a place it survives rotation of the thing it protects. Both refusal paths are covered
// because they had the same defect for different reasons: the scheme check formatted the
// raw string, and the parse path WRAPPED url.Parse's error, which embeds the whole URL of
// its own accord. That second one is the reason the parse error is reported rather than
// wrapped, which reads like lost detail until you know what the detail was.
func TestNew_RejectionDoesNotLeakURLCredentials(t *testing.T) {
	const secret = "hunter2"
	const username = "svcuser"
	for _, bad := range []string{
		"ftp://" + username + ":" + secret + "@h/jwks", // parses; refused by the scheme check
		"://" + username + ":" + secret + "@nope",      // does not parse
	} {
		_, err := New(Config{JWKSURL: bad, Audience: testAudience, Issuer: testIssuer})
		if err == nil {
			t.Fatalf("New accepted %q", bad)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error for a URL with userinfo carries its password, and this "+
				"error is logged at startup: %v", err)
		}
		// The username is credential material here, not an identifier: a JWKS endpoint
		// behind a basic-auth gateway is issued the pair together. url.URL.Redacted()
		// keeps it, which is why these sites use redact.URL instead.
		if strings.Contains(err.Error(), username) {
			t.Errorf("the error carries the userinfo USERNAME, which is half of the "+
				"credential and is logged at startup: %v", err)
		}
	}
}

// TestJWKSStatusGuard_ErrorsDoNotLeakURLCredentials covers the fetch-time formatting sites.
// The startup check above is one URL rendered once; the guard renders req.URL on every
// failed refresh, so a misconfigured endpoint writes the same line on a loop. Each arm here
// is a distinct error path, because the leak is per-format-verb: fixing one and missing
// another leaves the credential in the logs just as often.
func TestJWKSStatusGuard_ErrorsDoNotLeakURLCredentials(t *testing.T) {
	const secret = "hunter2"
	const username = "svcuser"

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"non-2xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`upstream down`))
		}},
		{"undecodable 2xx", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}},
		{"empty key set", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"keys":[]}`))
		}},
		{"oversize body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("a"), jwksMaxBody+1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			u, err := url.Parse(srv.URL + "/.well-known/jwks.json")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			u.User = url.UserPassword(username, secret)

			req, err := http.NewRequest(http.MethodGet, u.String(), nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			g := &jwksStatusGuard{next: http.DefaultTransport}
			resp, err := g.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("the guard accepted a response it must refuse")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("password leaked into a per-refresh error: %v", err)
			}
			if strings.Contains(err.Error(), username) {
				t.Errorf("userinfo username leaked into a per-refresh error: %v", err)
			}
			// The host has to survive: it is the only thing in these errors that tells an
			// operator WHICH endpoint is misconfigured, and a redactor that ate it would
			// be replaced by a raw print at the next outage.
			if !strings.Contains(err.Error(), u.Host) {
				t.Errorf("the error lost the host, leaving nothing diagnosable: %v", err)
			}
		})
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

// TestJWKSStatusGuard_UpstreamBodiesNeverReachTheLog pins the two debug lines in
// jwksStatusGuard.
//
// Both once logged a bounded PREFIX of the upstream response body, with a comment arguing
// that bounding the length and naming the origin made it safe. That conceded the premise —
// the body is untrusted upstream text — and then ignored it. Length is not the property that
// matters. A gateway that rejected our Authorization header is the case most likely to quote
// the request back, so the body most in need of redaction is the one this endpoint returns
// when it is misconfigured, which is exactly when an operator turns debug on. The debug LEVEL
// is not a mitigation either: it lands in the same log store as everything else.
//
// The assertion is on what the guard EMITS, not on the error it returns, because the error
// was already clean; the leak was in the line beside it.
func TestJWKSStatusGuard_UpstreamBodiesNeverReachTheLog(t *testing.T) {
	// A body shaped like the leak that matters: an error page reflecting the credential it
	// was sent. Long enough that a "bounded prefix" would still contain the secret.
	const reflected = "401 Unauthorized: rejected Authorization: Bearer s3cret-token-value"

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"non-2xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(reflected))
		}},
		{"undecodable 2xx", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(reflected))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			var logged bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(prev)

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/.well-known/jwks.json", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			g := &jwksStatusGuard{next: http.DefaultTransport}
			resp, err := g.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("the guard accepted a response it must refuse")
			}

			out := logged.String()
			if strings.Contains(out, "s3cret-token-value") {
				t.Errorf("an upstream response body reached the log: %s", out)
			}
			if strings.Contains(out, "body_prefix") {
				t.Errorf("the body_prefix attribute is back; it is the channel this test exists to keep closed: %s", out)
			}
			// Without this the test would also pass if the guard stopped logging at all,
			// or logged something with no diagnostic value.
			if !strings.Contains(out, srv.Listener.Addr().String()) {
				t.Errorf("the log lost the endpoint, leaving nothing diagnosable: %s", out)
			}
		})
	}
}

// credentialedJWKSURL rewrites a test server's URL into the shape an operator can legally
// configure: HTTP basic userinfo plus a query-string credential, both of which some
// gateways require to serve their key set.
// TestVerifyActor_FollowsAJWKSRedirectWithoutForwardingCredentials pins both halves of the
// redirect behaviour, because each one alone is satisfied by a broken implementation.
//
// FOLLOWING: an http.RoundTripper sits below http.Client's redirect handling, so returning an
// error for a 3xx means the Client never sees the response and never follows. An ordinary
// http->https upgrade or a CDN hop then becomes a permanent ErrKeyUnavailable on every refresh.
//
// NOT FORWARDING: the operator's credentials belong to the host they configured. net/http drops
// Authorization across hosts on its own, so the guard must be the thing that withholds it from
// the redirect target — and it must also withhold the query, which net/http would carry along
// were the guard still rewriting req.URL to the configured endpoint on every hop.
func TestVerifyActor_FollowsAJWKSRedirectWithoutForwardingCredentials(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var targetHits atomic.Int32
	var sawAuth, sawQuery atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		if r.URL.RawQuery != "" {
			sawQuery.Store(true)
		}
		writeKeySet(w, key)
	}))
	defer target.Close()

	// The configured endpoint authenticates the FIRST hop and then redirects elsewhere: the
	// credentials must reach it, and must stop there.
	var firstHopAuthed atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok && user == "svc" && pass == "pw" &&
			r.URL.Query().Get("access_token") == "s3cret" {
			firstHopAuthed.Store(true)
		}
		http.Redirect(w, r, target.URL+"/elsewhere/jwks", http.StatusFound)
	}))
	defer origin.Close()

	s := &signer{key: key, jwksURL: origin.URL}
	token := s.sign(t, nil)

	v, err := New(Config{JWKSURL: credentialedJWKSURL(t, origin.URL), Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	actor, err := v.VerifyActor(context.Background(), token)
	if err != nil {
		t.Fatalf("VerifyActor: %v — a redirected JWKS endpoint must still resolve", err)
	}
	if actor.Username != "ada" {
		t.Errorf("username = %q, want %q", actor.Username, "ada")
	}
	if !firstHopAuthed.Load() {
		t.Error("the configured endpoint did not receive the operator's credentials")
	}
	if got := targetHits.Load(); got != 1 {
		t.Fatalf("redirect target served %d times, want 1", got)
	}
	if sawAuth.Load() {
		t.Error("the redirect target received an Authorization header: the operator's credential " +
			"belongs to the host they configured, not to wherever it points")
	}
	if sawQuery.Load() {
		t.Error("the redirect target received the operator's query: the guard must not rewrite a " +
			"redirect hop's URL back to the configured endpoint")
	}
}

func credentialedJWKSURL(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	u.User = url.UserPassword("svc", "pw")
	u.Path = "/.well-known/jwks"
	u.RawQuery = "access_token=s3cret"
	return u.String()
}

// TestVerifyActor_JWKSURLCredentialsNeverReachTheError covers the leak end to end, through
// VerifyActor, because that is the only vantage point from which it is visible.
//
// The transport already redacts every URL it formats itself. What it cannot reach is the
// *url.Error http.Client.Do wraps around EVERY transport failure: Do builds that wrapper
// from req.URL, and net/url's own masking replaces the password with *** while keeping the
// username AND the whole query verbatim. A test that calls the guard's RoundTrip directly
// never sees that wrapper, so it would pass against the leaking version — which is why this
// one goes through the Verifier and asserts on the error the service actually logs
// (internal/service/auth.go renders it with slog on the ErrKeyUnavailable path).
//
// A connection refused is used rather than a bad status because it is the failure mode with
// no in-package error at all: everything about the message comes from net/http.
func TestVerifyActor_JWKSURLCredentialsNeverReachTheError(t *testing.T) {
	s := newSigner(t)
	token := s.sign(t, nil)

	// A server closed before use: the address is well-formed and nothing listens on it.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	v, err := New(Config{JWKSURL: credentialedJWKSURL(t, deadURL), Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = v.VerifyActor(context.Background(), token)
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable when the key set cannot be fetched", err)
	}

	// Walk every layer, not just the outermost Error(): the leak lives in a wrapper the
	// outer text happens to quote today, and it must stay gone if that ever changes.
	for e := err; e != nil; e = errors.Unwrap(e) {
		msg := e.Error()
		for _, secret := range []string{"svc", "pw", "s3cret", "access_token"} {
			if strings.Contains(msg, secret) {
				t.Fatalf("error layer %T leaks %q from the JWKS URL: %s", e, secret, msg)
			}
		}
	}
	// Naming the endpoint is the point of the message; an error that leaked nothing because
	// it said nothing would satisfy the assertions above and help no one.
	host := strings.TrimPrefix(deadURL, "http://")
	if !strings.Contains(err.Error(), host) {
		t.Errorf("err = %v, want the JWKS host %s named: without it the operator cannot tell "+
			"which endpoint is down", err, host)
	}
}

// TestVerifyActor_JWKSURLCredentialsStillAuthenticate is the other half, and the reason the
// fix is a swap rather than a strip.
//
// Removing the userinfo and query from the URL the http.Client sees is what keeps them out
// of its *url.Error — but it would also stop authenticating the fetch, and the resulting
// 401 is itself an ErrKeyUnavailable. Every assertion in the leak test above would still
// pass against that version. This one fails against it: the endpoint refuses anything that
// does not carry BOTH credentials, so a token verifies only if the guard put them back.
//
// The basic credential must be applied as a header rather than left on the URL because
// http.Client.send is what turns URL userinfo into an Authorization header, and it runs
// before the transport — a URL repaired inside RoundTrip would authenticate nothing.
func TestVerifyActor_JWKSURLCredentialsStillAuthenticate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "svc" || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("access_token") != "s3cret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		served.Add(1)
		writeKeySet(w, key)
	}))
	defer srv.Close()

	s := &signer{key: key, jwksURL: srv.URL}
	token := s.sign(t, nil)

	v, err := New(Config{JWKSURL: credentialedJWKSURL(t, srv.URL), Audience: testAudience, Issuer: testIssuer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	actor, err := v.VerifyActor(context.Background(), token)
	if err != nil {
		t.Fatalf("VerifyActor: %v — the key fetch must still carry the operator's credentials", err)
	}
	if actor.Username != "ada" {
		t.Errorf("username = %q, want %q", actor.Username, "ada")
	}
	if got := served.Load(); got != 1 {
		t.Errorf("key set served %d times, want 1", got)
	}
}
