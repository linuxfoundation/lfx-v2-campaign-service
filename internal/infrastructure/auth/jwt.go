// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package auth verifies the Heimdall-issued bearer token on every authenticated request
// and turns its claims into the domain actor recorded on writes.
//
// Heimdall already validates these tokens, so this is not here to make the happy path
// work — it is here because the gateway's guarantee stops at the cluster boundary.
// Anything reaching the pod directly (a misconfigured NetworkPolicy, a sibling workload,
// a port-forward) bypasses it, and these claims are written to created_by / updated_by.
// An unverified claim is therefore not merely an unauthenticated request: it is a
// forgeable audit trail for who authorized paid ad spend.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/auth0/go-jwt-middleware/v2/jwks"
	"github.com/auth0/go-jwt-middleware/v2/validator"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	// PINNED, never read from the token header: trusting the declared alg is what makes
	// algorithm-confusion attacks work. The TTL's refetch on expiry is what lets a
	// Heimdall key rotation take effect without restarting this pod.
	signatureAlgorithm = validator.PS256
	jwksCacheTTL       = 5 * time.Minute
	clockSkew          = 5 * time.Second // exp/nbf drift, as the sibling services allow
	bearerPrefix       = "bearer "       // stripped case-insensitively
)

// ErrUnauthenticated is returned for every token this package refuses — one sentinel on
// purpose: a bad signature, wrong audience, expired token and missing principal are
// indistinguishable from outside, so an attacker learns only "no".
var ErrUnauthenticated = errors.New("the bearer token is not valid for this service")

// Config carries the deployment's JWT settings. An empty JWKS URL, audience or issuer
// falls back to the pkg/constants default — the value LoadConfig supplies — so a
// hand-built Config yields a verifier that verifies rather than one refusing all.
type Config struct {
	JWKSURL  string // Heimdall's JSON Web Key Set endpoint
	Audience string // the audience claim this service requires
	Issuer   string // the issuer claim this service requires
	// MockLocalPrincipal, when non-empty, DISABLES verification and treats every request
	// as this principal. Local development only.
	MockLocalPrincipal string
}

// heimdallClaims are Heimdall's custom claims. Principal is the LFID username (the
// platform's canonical identity); the rest are best-effort display fields.
type heimdallClaims struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Validate runs after signature, audience, issuer and expiry pass. A token with no
// principal is refused rather than accepted with a blank actor, since a VERIFIED token
// attributing a write to nobody carries the authority of having been checked.
func (c *heimdallClaims) Validate(_ context.Context) error {
	if strings.TrimSpace(c.Principal) == "" {
		return errors.New("principal claim is empty")
	}
	return nil
}

// Verifier validates bearer tokens against Heimdall's JWKS.
type Verifier struct {
	validator *validator.Validator // nil exactly when mock is non-nil
	mock      *model.Actor         // the fixed actor of local-development mock mode
}

// New builds a Verifier, erroring rather than returning a degraded one when the JWKS URL is
// unusable: a degraded verifier has two behaviours and both are wrong — refusing everything
// is a confusing outage, allowing everything is the hole this package closes.
func New(cfg Config) (*Verifier, error) {
	if p := strings.TrimSpace(cfg.MockLocalPrincipal); p != "" {
		return &Verifier{mock: &model.Actor{Username: p, Name: p}}, nil
	}

	rawJWKS := orDefault(cfg.JWKSURL, constants.DefaultJWKSURL)
	jwksURL, err := url.Parse(rawJWKS)
	if err != nil {
		return nil, fmt.Errorf("parse JWKS URL: %w", err)
	}
	if jwksURL.Scheme == "" || jwksURL.Host == "" {
		return nil, fmt.Errorf("JWKS URL %q is not an absolute URL", rawJWKS)
	}
	issuer, err := url.Parse(orDefault(cfg.Issuer, constants.DefaultIssuer))
	if err != nil {
		return nil, fmt.Errorf("parse JWT issuer: %w", err)
	}
	if issuer.String() == "" {
		return nil, errors.New("JWT issuer is empty")
	}
	audience := orDefault(cfg.Audience, constants.DefaultAudience)

	// WithCustomJWKSURI is required: "heimdall" is a bare name, not an OIDC discovery URL.
	provider := jwks.NewCachingProvider(issuer, jwksCacheTTL, jwks.WithCustomJWKSURI(jwksURL))

	v, err := validator.New(
		provider.KeyFunc,
		signatureAlgorithm,
		issuer.String(),
		[]string{audience},
		validator.WithCustomClaims(func() validator.CustomClaims { return &heimdallClaims{} }),
		validator.WithAllowedClockSkew(clockSkew),
	)
	if err != nil {
		return nil, fmt.Errorf("build JWT validator: %w", err)
	}
	return &Verifier{validator: v}, nil
}

// orDefault trims v and substitutes def when the result is empty.
func orDefault(v, def string) string {
	if t := strings.TrimSpace(v); t != "" {
		return t
	}
	return def
}

// VerifyActor validates the token and returns the actor its claims describe. Every
// failure wraps ErrUnauthenticated; the reason is for the caller to LOG, never send.
func (v *Verifier) VerifyActor(ctx context.Context, token string) (*model.Actor, error) {
	if v.mock != nil {
		a := *v.mock
		return &a, nil
	}
	if v.validator == nil {
		// Unreachable through New; a zero Verifier must still not authenticate.
		return nil, fmt.Errorf("%w: verifier is not configured", ErrUnauthenticated)
	}

	raw := strings.TrimSpace(token)
	if len(raw) >= len(bearerPrefix) && strings.EqualFold(raw[:len(bearerPrefix)], bearerPrefix) {
		raw = strings.TrimSpace(raw[len(bearerPrefix):])
	}
	if raw == "" {
		return nil, fmt.Errorf("%w: token is empty", ErrUnauthenticated)
	}

	parsed, err := v.validator.ValidateToken(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	claims, ok := parsed.(*validator.ValidatedClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims type %T", ErrUnauthenticated, parsed)
	}
	custom, ok := claims.CustomClaims.(*heimdallClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected custom claims type %T", ErrUnauthenticated, claims.CustomClaims)
	}

	return &model.Actor{
		Username: strings.TrimSpace(custom.Principal),
		Email:    strings.TrimSpace(custom.Email),
		Name:     strings.TrimSpace(custom.Name),
	}, nil
}
