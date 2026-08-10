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
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auth0/go-jwt-middleware/v2/jwks"
	"github.com/auth0/go-jwt-middleware/v2/validator"
	"golang.org/x/sync/singleflight"
	jose "gopkg.in/go-jose/go-jose.v2"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
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
	// jwksFetchTimeout bounds the key fetch. The provider defaults to a client with NO
	// timeout, and the cold fetch runs while holding the provider's write lock — a stalled
	// Heimdall would block every authentication on this pod for as long as the TCP
	// connection hangs, which no HTTP server timeout cancels.
	jwksFetchTimeout = 10 * time.Second
)

// ErrUnauthenticated is returned for every token this package refuses — one sentinel on
// purpose: a bad signature, wrong audience, expired token and missing principal are
// indistinguishable from outside, so an attacker learns only "no".
var ErrUnauthenticated = errors.New("the bearer token is not valid for this service")

// ErrKeyUnavailable is domain.ErrKeyUnavailable, re-exported so this package's callers
// and tests name it where they name ErrUnauthenticated. It reports that Heimdall's signing
// keys could not be retrieved — a fetch that failed, timed out, or was cancelled — which is
// NOT a statement about the token: nothing was learned about it, because it was never
// checked. The service layer maps it to 503 while every token-side refusal gets 400.
var ErrKeyUnavailable = domain.ErrKeyUnavailable

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
	// InCluster reports that this process is running as a Kubernetes pod, in which case
	// MockLocalPrincipal is refused rather than honoured. See New.
	InCluster bool
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
		// "Local development only" was a convention, not a control. The chart declares
		// JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL under app.environment and deployment.yaml
		// renders whatever it holds, so `--set` (or an edited values file) could ship a
		// running pod that accepts ANY bearer token as this principal — with no signature
		// check, on the endpoints that spend money. The empty default and the parity test
		// keep the chart honest about its own value; neither can stop an override.
		//
		// InCluster is the discriminator, and it deliberately does NOT rest on
		// KUBERNETES_SERVICE_HOST alone — deployment.yaml renders arbitrary env names, so
		// the same override that enables this bypass could have cleared that variable too.
		// See config.runningInCluster for the two signals and the chart-side guard. A
		// laptop, `go run`, a plain container and CI trip neither, so the developer
		// workflow this exists for is untouched.
		//
		// The refusal is an error, not a silent downgrade to real verification: a deploy
		// that asked for no authentication has a broken intent, and starting anyway —
		// verifying, and serving as if nothing were wrong — leaves that intent live in a
		// values file for the next person. Fail the pod and say why.
		if cfg.InCluster {
			return nil, fmt.Errorf("%s is set to %q, but this process is running in Kubernetes: "+
				"it disables JWT verification entirely and is for local development only. Unset it "+
				"in the chart values for this deployment", constants.EnvMockLocalPrincipal, p)
		}
		return &Verifier{mock: &model.Actor{Username: p, Name: p}}, nil
	}

	rawJWKS := orDefault(cfg.JWKSURL, constants.DefaultJWKSURL)
	jwksURL, err := url.Parse(rawJWKS)
	if err != nil {
		return nil, fmt.Errorf("parse JWKS URL: %w", err)
	}
	// Scheme, not just absoluteness: http.Client cannot fetch ftp:// or file://, so
	// accepting one would satisfy the fail-fast check and then refuse every request.
	if (jwksURL.Scheme != "http" && jwksURL.Scheme != "https") || jwksURL.Host == "" {
		return nil, fmt.Errorf("JWKS URL %q is not an absolute http(s) URL", rawJWKS)
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
	provider := jwks.NewCachingProvider(issuer, jwksCacheTTL, jwks.WithCustomJWKSURI(jwksURL),
		jwks.WithCustomClient(&http.Client{
			Timeout:   jwksFetchTimeout,
			Transport: &jwksStatusGuard{next: http.DefaultTransport},
		}))

	v, err := validator.New(
		coalesceKeyFunc(requireKeys(provider.KeyFunc)),
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

// jwksErrorBodyPeek bounds how much of a non-2xx JWKS body is read before it is discarded.
// Enough to make a misconfiguration diagnosable in a log line; small enough that a hostile
// or runaway endpoint cannot make the read itself the outage.
const jwksErrorBodyPeek = 512

// jwksStatusGuard rejects a non-2xx JWKS response BEFORE the provider decodes it.
//
// go-jwt-middleware v2.3.1 does not check the status: jwks.Provider.KeyFunc goes straight
// from Client.Do to json.NewDecoder(response.Body).Decode(&jwks) (jwks/provider.go:84-93).
// jose.JSONWebKeySet has one field and ignores unknown ones, so an error object — a 404
// `{"error":"not found"}` from a wrong path, a 502 from a proxy in front of Heimdall —
// decodes CLEANLY into a key set with zero keys. The provider then returns it as a success
// and CachingProvider caches it for the full TTL. Every valid token for the next five
// minutes fails to find its signing key, and that is a verdict on the TOKEN: HTTP 400,
// "invalid bearer token", to callers whose credentials are perfectly good, for a
// misconfiguration that is entirely ours.
//
// Failing at the transport is what turns that into an error the provider propagates, so
// coalesceKeyFunc tags it ErrKeyUnavailable and the service answers 503. It also means
// nothing is cached: CachingProvider stores only successful fetches, so the next request
// retries rather than waiting out a TTL on poisoned data.
type jwksStatusGuard struct{ next http.RoundTripper }

func (g *jwksStatusGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err != nil || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return resp, err
	}
	// We are returning an error instead of the response, so nothing downstream will close
	// this body. Drain a bounded prefix for the diagnostic, then close: draining also lets
	// net/http return the connection to the idle pool instead of tearing down TCP+TLS,
	// which matters because a misconfigured endpoint fails on EVERY refresh.
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, jwksErrorBodyPeek))
	_ = resp.Body.Close()
	// The body is NOT quoted into the error. It is untrusted upstream text that would
	// travel into logs; the status and the URL are what identify the misconfiguration, and
	// the body goes to a debug log where its length is bounded and its origin is obvious.
	slog.Debug("JWKS endpoint returned a non-2xx response",
		"url", req.URL.Redacted(), "status", resp.StatusCode, "body_prefix", string(peek))
	return nil, fmt.Errorf("JWKS endpoint %s returned HTTP %d", req.URL.Redacted(), resp.StatusCode)
}

// requireKeys refuses a key set with no keys in it.
//
// The assertion is against gopkg.in/go-jose/go-jose.v2, which is the version
// go-jwt-middleware v2.3.1 decodes into (jwks/provider.go:13). Asserting the wrong jose
// major here would compile, never match, and pass everything through — the check would be
// vacuous with nothing to show for it, which is why
// TestRequireKeys_EmptyKeySetFromTheRealProviderIsUnavailable drives it through the actual
// provider rather than constructing the type itself.
//
// jwksStatusGuard closes the reachable path to one (a decoded error object), but the
// INVARIANT is what this states: a key set with nothing in it cannot verify any token, so
// returning it as a success means every token that arrives is refused as bad. That is a
// 503 in every case that produces it — a 200 carrying `{}` or `{"keys":[]}`, an issuer
// mid-rotation with nothing published, a future provider version that drops the status
// check some other way. The guard above prevents one cause; this prevents the outcome.
func requireKeys(inner func(context.Context) (any, error)) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		val, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		if set, ok := val.(*jose.JSONWebKeySet); ok && len(set.Keys) == 0 {
			return nil, errors.New("JWKS endpoint returned no signing keys")
		}
		return val, nil
	}
}

// coalesceKeyFunc collapses concurrent COLD-cache JWKS fetches into one.
//
// jwks.CachingProvider does not do this itself. On a miss its KeyFunc calls refreshKey,
// which takes the write lock and then fetches WITHOUT re-checking the cache — so N
// callers that all miss produce N serialized HTTP fetches rather than one, each bounded
// by jwksFetchTimeout, and each holding the write lock for its full duration. The Nth
// caller therefore waits roughly N × fetch, not one fetch. That is reachable twice: the
// startup burst before anything is cached, and a TTL expiry that coincides with the JWKS
// endpoint being slow or down. Authentication is on every request, so the queue is the
// whole request load.
//
// DoChan rather than Do so each caller keeps its OWN deadline. Do returns only when the
// shared call returns, which would let one caller's slow fetch outlive a later caller's
// context; here a caller whose ctx expires stops waiting while the in-flight fetch
// continues for whoever is still listening.
//
// The key is constant: this verifier has exactly one issuer, fixed at construction.
func coalesceKeyFunc(inner func(context.Context) (any, error)) func(context.Context) (any, error) {
	var group singleflight.Group
	return func(ctx context.Context) (any, error) {
		ch := group.DoChan("jwks", func() (any, error) {
			// NOT the caller's ctx. The result is shared, so binding the fetch to whichever
			// caller happened to arrive first would let that one's cancellation fail every
			// other caller waiting on it. The provider's own http.Client timeout
			// (jwksFetchTimeout) is what bounds this.
			return inner(context.WithoutCancel(ctx))
		})
		// Both arms tag ErrKeyUnavailable, and this is the ONLY place that can: the
		// validator wraps whatever the key func returns (validator.go:192, with %w), so
		// tagging here is what lets VerifyActor tell "we could not fetch the keys" apart
		// from "the token failed a check" — the two arrive as one error otherwise.
		select {
		case res := <-ch:
			if res.Err != nil {
				return nil, fmt.Errorf("%w: %w", ErrKeyUnavailable, res.Err)
			}
			return res.Val, nil
		case <-ctx.Done():
			// A caller-side cancellation still means no key was obtained, so no claim
			// about the token was established. That it was OUR wait that ended rather
			// than Heimdall's answer does not make the token bad.
			return nil, fmt.Errorf("%w: %w", ErrKeyUnavailable, ctx.Err())
		}
	}
}

// orDefault trims v and substitutes def when the result is empty.
func orDefault(v, def string) string {
	if t := strings.TrimSpace(v); t != "" {
		return t
	}
	return def
}

// VerifyActor validates the token and returns the actor its claims describe.
//
// Every failure that is a verdict on the TOKEN wraps ErrUnauthenticated, undifferentiated
// on purpose; the reason is for the caller to LOG, never send. The one failure that is not
// such a verdict — the signing keys could not be retrieved — wraps ErrKeyUnavailable
// instead, so the caller can report this service's outage as one rather than as the
// caller's fault.
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
		// ValidateToken answers two different questions with one error. Most of what it
		// returns is a verdict ON THE TOKEN; a key-func failure is not — it is this
		// service unable to check anything. Collapsing the second into ErrUnauthenticated
		// makes a Heimdall outage present as "your token is invalid" with a 400 to every
		// caller holding a good one, and 400 tells them not to retry a condition that
		// clears by itself. coalesceKeyFunc tags it so the two are separable here.
		if errors.Is(err, ErrKeyUnavailable) {
			return nil, err
		}
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

	// The library validates exp only when the claim is PRESENT (validator.go:167), so a
	// correctly signed token that simply omits it would never stop working. A credential
	// with no lifetime is the one this service must not honour.
	if claims.RegisteredClaims.Expiry == 0 {
		return nil, fmt.Errorf("%w: token has no expiry", ErrUnauthenticated)
	}

	return &model.Actor{
		Username: strings.TrimSpace(custom.Principal),
		Email:    strings.TrimSpace(custom.Email),
		Name:     strings.TrimSpace(custom.Name),
	}, nil
}
