// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package assetsign

import (
	"crypto/hmac"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testProject = "cncf"
	testBrief   = "11111111-2222-4333-8444-555555555555"
	testAsset   = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

func testKey(fill byte) string {
	b := make([]byte, MinKeyBytes)
	for i := range b {
		b[i] = fill
	}
	return base64.StdEncoding.EncodeToString(b)
}

func newTestSigner(t *testing.T, now time.Time) *Signer {
	t.Helper()
	s, err := New(testKey(7), "https://lfx-api.example.org", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.WithClock(func() time.Time { return now })
}

// parts splits a minted URL back into the route's inputs.
func parts(t *testing.T, raw string) (project, brief, asset string, exp int64, sig string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse minted URL: %v", err)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, RoutePrefix), "/")
	if len(segs) != 3 {
		t.Fatalf("minted path %q has %d segments after the prefix", u.Path, len(segs))
	}
	exp, err = strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("exp: %v", err)
	}
	return segs[0], segs[1], segs[2], exp, u.Query().Get("sig")
}

func TestMintThenVerifyRoundTrips(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newTestSigner(t, now)
	raw, exp, err := s.Mint(testProject, testBrief, testAsset)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got, want := exp.Unix(), now.Add(DefaultTTL).Unix(); got != want {
		t.Fatalf("exp = %d, want %d", got, want)
	}
	p, b, a, e, sig := parts(t, raw)
	if !s.Verify(p, b, a, e, sig) {
		t.Fatal("a freshly minted URL must verify")
	}
	if !strings.HasPrefix(raw, "https://lfx-api.example.org"+RoutePrefix) {
		t.Fatalf("minted URL %q is not under the route prefix", raw)
	}
}

// TestMintedURLMatchesTheRoute pins RoutePrefix to the Goa route declared in
// design/signed_asset.go: if one moves without the other, every minted URL 404s.
func TestMintedURLMatchesTheRoute(t *testing.T) {
	const route = "/_campaigns/creative-assets/{project_id}/{brief_id}/{asset_id}"
	if !strings.HasPrefix(route, RoutePrefix) {
		t.Fatalf("RoutePrefix %q does not prefix the design route %q", RoutePrefix, route)
	}
}

func TestVerifyRejectsEachTamperedField(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newTestSigner(t, now)
	raw, _, err := s.Mint(testProject, testBrief, testAsset)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, b, a, e, sig := parts(t, raw)
	other := "66666666-7777-4888-9999-aaaaaaaaaaab"
	flipped := []byte(sig)
	if flipped[0] == 'A' {
		flipped[0] = 'B'
	} else {
		flipped[0] = 'A'
	}
	cases := map[string]func() bool{
		"project":             func() bool { return s.Verify("lfn", b, a, e, sig) },
		"brief":               func() bool { return s.Verify(p, other, a, e, sig) },
		"asset":               func() bool { return s.Verify(p, b, other, e, sig) },
		"exp later":           func() bool { return s.Verify(p, b, a, e+1, sig) },
		"exp earlier":         func() bool { return s.Verify(p, b, a, e-1, sig) },
		"signature":           func() bool { return s.Verify(p, b, a, e, string(flipped)) },
		"uppercase asset id":  func() bool { return s.Verify(p, b, strings.ToUpper(a), e, sig) },
		"fields swapped":      func() bool { return s.Verify(p, a, b, e, sig) },
		"project absorbs id":  func() bool { return s.Verify(p+b, "", a, e, sig) },
		"empty signature":     func() bool { return s.Verify(p, b, a, e, "") },
		"short signature":     func() bool { return s.Verify(p, b, a, e, sig[:42]) },
		"long signature":      func() bool { return s.Verify(p, b, a, e, sig+"A") },
		"padded signature":    func() bool { return s.Verify(p, b, a, e, sig[:42]+"=") },
		"non-base64 char":     func() bool { return s.Verify(p, b, a, e, sig[:42]+"!") },
		"std-alphabet plus":   func() bool { return s.Verify(p, b, a, e, sig[:42]+"+") },
		"negative exp":        func() bool { return s.Verify(p, b, a, -1, sig) },
		"nil signer":          func() bool { return (*Signer)(nil).Verify(p, b, a, e, sig) },
		"wrong key same data": func() bool { return wrongKey(t, now).Verify(p, b, a, e, sig) },
	}
	for name, verify := range cases {
		t.Run(name, func(t *testing.T) {
			if verify() {
				t.Fatalf("Verify accepted a URL with a tampered %s", name)
			}
		})
	}
}

func wrongKey(t *testing.T, now time.Time) *Signer {
	t.Helper()
	s, err := New(testKey(8), "https://lfx-api.example.org", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.WithClock(func() time.Time { return now })
}

func TestVerifyRejectsExpiredAndOverlongExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newTestSigner(t, now)
	raw, exp, err := s.Mint(testProject, testBrief, testAsset)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, b, a, e, sig := parts(t, raw)

	atExpiry := s.WithClock(func() time.Time { return exp })
	if atExpiry.Verify(p, b, a, e, sig) {
		t.Fatal("a URL must not verify at its expiry second")
	}
	after := s.WithClock(func() time.Time { return exp.Add(time.Hour) })
	if after.Verify(p, b, a, e, sig) {
		t.Fatal("an expired URL must not verify")
	}

	// A VALIDLY signed URL whose expiry is further away than MaxTTL is refused: a leaked key
	// must not be able to mint long-lived URLs this service honours.
	far := now.Add(MaxTTL + time.Minute).Unix()
	farSig := base64.RawURLEncoding.EncodeToString(s.mac(p, b, a, far))
	if s.Verify(p, b, a, far, farSig) {
		t.Fatal("a signature over an expiry beyond MaxTTL must not verify")
	}
	edge := now.Add(MaxTTL).Unix()
	edgeSig := base64.RawURLEncoding.EncodeToString(s.mac(p, b, a, edge))
	if !s.Verify(p, b, a, edge, edgeSig) {
		t.Fatal("an expiry exactly MaxTTL away should verify")
	}
}

// TestVerifyUsesConstantTimeCompare proves the MAC comparison goes through macEqual
// (hmac.Equal) on every verify that reaches it, valid or not.
func TestVerifyUsesConstantTimeCompare(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newTestSigner(t, now)
	raw, _, err := s.Mint(testProject, testBrief, testAsset)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, b, a, e, sig := parts(t, raw)

	calls := 0
	orig := macEqual
	macEqual = func(x, y []byte) bool { calls++; return hmac.Equal(x, y) }
	t.Cleanup(func() { macEqual = orig })

	if !s.Verify(p, b, a, e, sig) {
		t.Fatal("valid URL failed to verify")
	}
	if s.Verify(p, b, "66666666-7777-4888-9999-aaaaaaaaaaab", e, sig) {
		t.Fatal("tampered URL verified")
	}
	if calls != 2 {
		t.Fatalf("macEqual called %d times, want 2 (every comparison must be constant-time)", calls)
	}
}

func TestMintRefusesUncarriableIDs(t *testing.T) {
	s := newTestSigner(t, time.Unix(1_800_000_000, 0))
	for name, in := range map[string][3]string{
		"project with slash": {"cncf/../x", testBrief, testAsset},
		"project with dot":   {"..", testBrief, testAsset},
		"empty project":      {"", testBrief, testAsset},
		"brief not uuid":     {testProject, "nope", testAsset},
		"asset not uuid":     {testProject, testBrief, "../../etc"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := s.Mint(in[0], in[1], in[2]); err == nil {
				t.Fatalf("Mint accepted %v", in)
			}
		})
	}
	if _, _, err := (*Signer)(nil).Mint(testProject, testBrief, testAsset); err != ErrDisabled {
		t.Fatalf("nil signer Mint err = %v, want ErrDisabled", err)
	}
}

func TestMintCanonicalisesUUIDSpelling(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newTestSigner(t, now)
	raw, _, err := s.Mint(testProject, strings.ToUpper(testBrief), "{"+testAsset+"}")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	p, b, a, e, sig := parts(t, raw)
	if b != testBrief || a != testAsset {
		t.Fatalf("minted ids %q/%q are not canonical", b, a)
	}
	if !s.Verify(p, b, a, e, sig) {
		t.Fatal("canonicalised URL failed to verify")
	}
}

func TestNewConfiguration(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, MinKeyBytes-1))
	cases := []struct {
		name, key, base, ttl string
		wantOff, wantErr     bool
	}{
		{name: "both unset is off", wantOff: true},
		{name: "key only is off", key: testKey(1), wantOff: true},
		{name: "base only is off", base: "https://lfx-api.example.org", wantOff: true},
		{name: "valid", key: testKey(1), base: "https://lfx-api.example.org"},
		{name: "valid with trailing slash", key: testKey(1), base: "https://lfx-api.example.org/"},
		{name: "short key", key: short, base: "https://lfx-api.example.org", wantErr: true},
		{name: "non-base64 key", key: "not base64!!", base: "https://lfx-api.example.org", wantErr: true},
		{name: "http base", key: testKey(1), base: "http://lfx-api.example.org", wantErr: true},
		{name: "no host", key: testKey(1), base: "https://", wantErr: true},
		{name: "userinfo", key: testKey(1), base: "https://u:p@lfx-api.example.org", wantErr: true},
		{name: "path", key: testKey(1), base: "https://lfx-api.example.org/api", wantErr: true},
		{name: "query", key: testKey(1), base: "https://lfx-api.example.org?x=1", wantErr: true},
		{name: "ttl too long", key: testKey(1), base: "https://lfx-api.example.org", ttl: "61m", wantErr: true},
		{name: "ttl too short", key: testKey(1), base: "https://lfx-api.example.org", ttl: "10s", wantErr: true},
		{name: "ttl garbage", key: testKey(1), base: "https://lfx-api.example.org", ttl: "soon", wantErr: true},
		{name: "ttl max", key: testKey(1), base: "https://lfx-api.example.org", ttl: "60m"},
		{name: "ttl min", key: testKey(1), base: "https://lfx-api.example.org", ttl: "5m"},
		{name: "ttl below the 5m floor", key: testKey(1), base: "https://lfx-api.example.org", ttl: "4m59s", wantErr: true},
		{name: "ttl 1m (the old floor) is now refused", key: testKey(1), base: "https://lfx-api.example.org", ttl: "1m", wantErr: true},
		// Partial combinations (#310 review): every SUPPLIED value is validated even when the
		// feature would be off because its other half is missing.
		{name: "malformed key, no base", key: "not base64!!", wantErr: true},
		{name: "short key, no base", key: short, wantErr: true},
		{name: "no key, http base", base: "http://lfx-api.example.org", wantErr: true},
		{name: "no key, base with path", base: "https://lfx-api.example.org/x", wantErr: true},
		{name: "no key, base with userinfo", base: "https://u:p@lfx-api.example.org", wantErr: true},
		{name: "ttl too long, both unset", ttl: "61m", wantErr: true},
		{name: "ttl too short, both unset", ttl: "1m", wantErr: true},
		{name: "ttl garbage, key only", key: testKey(1), ttl: "soon", wantErr: true},
		{name: "valid key, no base, valid ttl is off", key: testKey(1), ttl: "10m", wantOff: true},
		{name: "no key, valid base, valid ttl is off", base: "https://lfx-api.example.org", ttl: "10m", wantOff: true},
		{name: "valid ttl alone is off", ttl: "30m", wantOff: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.key, tc.base, tc.ttl)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if strings.Contains(err.Error(), tc.key) && tc.key != "" {
					t.Fatal("error quotes the key")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantOff != (s == nil) {
				t.Fatalf("signer enabled = %v, want off = %v", s != nil, tc.wantOff)
			}
		})
	}
}

// Every unusable value is reported at once, not just the first, so an operator fixing a broken
// rollout does not discover them one restart at a time.
func TestNewReportsEveryUnusableValue(t *testing.T) {
	_, err := New("not base64!!", "http://lfx-api.example.org", "2h")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"signing key", "https", "TTL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateIDs(t *testing.T) {
	if err := ValidateIDs(testProject, testBrief, testAsset); err != nil {
		t.Fatalf("valid ids refused: %v", err)
	}
	for name, in := range map[string][3]string{
		"project": {"a/b", testBrief, testAsset},
		"brief":   {testProject, "x", testAsset},
		"asset":   {testProject, testBrief, "x"},
	} {
		if ValidateIDs(in[0], in[1], in[2]) == nil {
			t.Errorf("%s: invalid id accepted", name)
		}
	}
}
