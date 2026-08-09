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
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	if _, err := v.VerifyActor(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated when the key set cannot be fetched", err)
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
